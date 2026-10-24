package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

// A fix's rounds review and fix their own output, so they can keep each
// other busy: a round fixes what the check of the one before found, and
// the check finds something in that fix. roundTracker decides which
// issues a round works on; fixGuard decides when the rounds stop
// altogether. Each of its breakers stops the rounds with the last good
// state kept and says why in the fix's warning:
//
//   - no progress: two rounds in a row fixed nothing and left as much to do
//   - oscillation: a round put the files back the way an earlier round had
//     them
//   - worse: a round's check found more new medium-or-worse issues in what
//     it changed than it fixed; that round is undone
//   - growth: the fix added more than fixGrowthFactor times what its first
//     round did, and at least fixGrowthFloor lines
//   - tests: a round broke a test that passed before it; it is undone and
//     the next round is told what broke, twice at most
//   - time: the fix ran past its time budget
//   - total rounds: a PR's fixes, one continuing another, ran
//     fixTotalRounds rounds, unless the request says to go on
//
// And in CI, fix commits carry fixTrailer: a fix run whose branch already
// ends in fixCIRepeat of its own commits only reviews (see fixcmd.go),
// whatever token pushed them.

const (
	fixGrowthFactor = 3
	fixGrowthFloor  = 200
	fixTotalRounds  = 10
	fixTestRetries  = 2
	fixCIRepeat     = 2
	// fixTimeBudget caps a fix job's rounds; the round that is running
	// finishes.
	fixTimeBudget = 30 * time.Minute
)

// fixTrailer marks a commit as a fix pr-manager made.
const fixTrailer = "Fixed-by: pr-manager"

// errFixRounds is startFix's answer when a PR's fixes already ran
// fixTotalRounds rounds and the request doesn't say to go on.
var errFixRounds = errors.New("this PR's fixes have run many rounds already")

// fixGuard is the rounds' circuit breakers.
type fixGuard struct {
	start         time.Time
	budget        time.Duration
	totalBefore   int // rounds earlier fixes of this PR ran
	totalCap      int
	stalls        int
	lastLeft      int
	orig          map[string]string // each tracked file's hash before any round changed it
	states        []roundState      // the files each round that stood left
	firstAdded    int
	added         int
	testRetries   int
	fixedPerRound map[int]int
}

func newFixGuard(old *PRResult, req fixRequest) *fixGuard {
	g := &fixGuard{start: time.Now(), budget: fixTimeBudget, totalBefore: old.FixRounds, totalCap: fixTotalRounds, lastLeft: -1, orig: map[string]string{}, fixedPerRound: map[int]int{}}
	if req.MoreRounds {
		g.totalCap = 0
	}
	return g
}

// beforeRound says why round may not start, or "".
func (g *fixGuard) beforeRound(round int) string {
	switch {
	case round > 1 && g.budget > 0 && time.Since(g.start) > g.budget:
		return fmt.Sprintf("stopped before round %d: the fix ran past its %s budget", round, g.budget)
	case g.totalCap > 0 && g.totalBefore+round > g.totalCap:
		return fmt.Sprintf("stopped before round %d: this PR's fixes have run %d rounds, the most without asking", round, g.totalCap)
	}
	return ""
}

// roundState is the tracked files' hashes, by path, as a round left them.
type roundState struct {
	round int
	files map[string]string
}

// initial records the targeted files' state before a round changes them,
// for the ones it hasn't seen yet, so a round that undoes everything is
// caught too. It runs before every round: the tracked files grow.
func (g *fixGuard) initial(dir string, files map[string]bool) {
	for p, h := range fileHashes(dir, files) {
		if _, ok := g.orig[p]; !ok {
			g.orig[p] = h
		}
	}
}

// afterRound records a round that stood: the files it left, the lines it
// added, how many issues it fixed and how many the next round works on.
// It says why the rounds stop here, or "".
func (g *fixGuard) afterRound(round int, dir string, files map[string]bool, added, fixed, left int) string {
	if round == 1 {
		g.firstAdded = added
	}
	g.added += added
	// A file an earlier state doesn't have wasn't tracked then, so no round
	// had changed it: it was as it is in orig. One first tracked now
	// hasn't been changed either.
	cur := fileHashes(dir, files)
	for p, h := range cur {
		if _, ok := g.orig[p]; !ok {
			g.orig[p] = h
		}
	}
	same := func(s map[string]string) bool {
		for p, h := range cur {
			was, ok := s[p]
			if !ok {
				was = g.orig[p]
			}
			if was != h {
				return false
			}
		}
		return true
	}
	if left > 0 {
		prev := -1
		if same(g.orig) {
			prev = 0
		} else {
			for _, s := range g.states {
				if same(s.files) {
					prev = s.round
					break
				}
			}
		}
		if prev >= 0 {
			what := "the code as it was before the fix"
			if prev > 0 {
				what = fmt.Sprintf("the code round %d left", prev)
			}
			return fmt.Sprintf("stopped after round %d: it went back to %s, so the rounds were going in circles", round, what)
		}
	}
	g.states = append(g.states, roundState{round, cur})
	if limit := max(fixGrowthFactor*g.firstAdded, fixGrowthFloor); round > 1 && left > 0 && g.added > limit {
		return fmt.Sprintf("stopped after round %d: the fix grew to %d added lines, past %d (%d× its first round's, or %d)", round, g.added, limit, fixGrowthFactor, fixGrowthFloor)
	}
	if fixed == 0 && g.lastLeft >= 0 && left >= g.lastLeft {
		g.stalls++
	} else {
		g.stalls = 0
	}
	g.lastLeft = left
	if g.stalls >= 2 && left > 0 {
		return fmt.Sprintf("stopped after round %d: two rounds in a row fixed nothing and left as much to do", round)
	}
	return ""
}

// fileHashes hashes each of files' contents in dir, by path; a missing
// file hashes as "missing".
func fileHashes(dir string, files map[string]bool) map[string]string {
	out := make(map[string]string, len(files))
	for p := range files {
		if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p))); err == nil {
			sum := sha256.Sum256(b)
			out[p] = hex.EncodeToString(sum[:])
		} else {
			out[p] = "missing"
		}
	}
	return out
}

// fileState is files' contents in a checkout, to put back when a round
// is undone.
type fileState struct {
	dir   string
	files map[string][]byte // nil: the file didn't exist
	modes map[string]fs.FileMode
}

func saveFiles(dir string, paths map[string]bool) *fileState {
	s := &fileState{dir: dir, files: map[string][]byte{}, modes: map[string]fs.FileMode{}}
	for p := range paths {
		full := filepath.Join(dir, filepath.FromSlash(p))
		st, err := os.Lstat(full)
		if errors.Is(err, os.ErrNotExist) {
			s.files[p] = nil // removed on restore
			continue
		}
		if err != nil || !st.Mode().IsRegular() {
			continue // a symlink or the like is left alone
		}
		b, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		s.files[p], s.modes[p] = b, st.Mode().Perm()
	}
	return s
}

// restore puts the files back as they were saved, removing the ones that
// didn't exist then.
func (s *fileState) restore() error {
	var errs []error
	for p, b := range s.files {
		full := filepath.Join(s.dir, filepath.FromSlash(p))
		if b == nil {
			if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.WriteFile(full, b, s.modes[p]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// addedLines counts the lines a round's diffs add.
func addedLines(diffs []triage.FileDiff) int {
	n := 0
	for _, f := range diffs {
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if strings.HasPrefix(l, "+") {
					n++
				}
			}
		}
	}
	return n
}

// fixLines is what the rounds' patches added, by file: line numbers and
// text. A check's new issue counts as the fix's own only when it is on
// one of these lines or quotes one of them (see roundTracker).
type fixLines struct {
	lines map[string]map[int]bool
	text  map[string][]string
}

func newFixLines() *fixLines {
	return &fixLines{lines: map[string]map[int]bool{}, text: map[string][]string{}}
}

func (fl *fixLines) add(diffs []triage.FileDiff) {
	for _, f := range diffs {
		// A later round's patch moves the lines earlier rounds added, and
		// drops the ones it deletes.
		from := f.Path
		if f.OldPath != "" {
			from = f.OldPath
		}
		if old := fl.lines[from]; len(old) > 0 {
			moved := map[int]bool{}
			for l := range old {
				if m := shiftLine(l, f.Hunks); m > 0 {
					moved[m] = true
				}
			}
			delete(fl.lines, from)
			fl.lines[f.Path] = moved
		}
		if from != f.Path && len(fl.text[from]) > 0 {
			fl.text[f.Path] = append(fl.text[f.Path], fl.text[from]...)
			delete(fl.text, from)
		}
		for _, h := range f.Hunks {
			n := h.NewStart
			for _, l := range h.Lines {
				switch {
				case strings.HasPrefix(l, "+"):
					if fl.lines[f.Path] == nil {
						fl.lines[f.Path] = map[int]bool{}
					}
					fl.lines[f.Path][n] = true
					if t := strings.TrimSpace(l[1:]); len(t) >= 8 {
						fl.text[f.Path] = append(fl.text[f.Path], t)
					}
					n++
				case strings.HasPrefix(l, "-"), strings.HasPrefix(l, `\`):
				default:
					n++
				}
			}
		}
	}
}

// shiftLine is where old line l of a file is after hunks change it, or 0
// when they delete it.
func shiftLine(l int, hunks []triage.Hunk) int {
	delta := 0
	for _, h := range hunks {
		end := h.OldStart + h.OldLines // the first old line after the hunk
		if h.OldLines == 0 {
			end = h.OldStart + 1 // a pure insertion goes after line OldStart
		}
		if l >= end {
			delta += h.NewLines - h.OldLines
			continue
		}
		if l < h.OldStart || h.OldLines == 0 {
			break
		}
		o, n := h.OldStart, h.NewStart
		for _, s := range h.Lines {
			switch {
			case strings.HasPrefix(s, "+"):
				n++
			case strings.HasPrefix(s, "-"):
				if o == l {
					return 0
				}
				o++
			case strings.HasPrefix(s, `\`):
			default:
				if o == l {
					return n
				}
				o++
				n++
			}
		}
		return 0
	}
	return l + delta
}

// owns reports whether the issue, in file, is on or quotes a line a round
// added.
func (fl *fixLines) owns(file string, is triage.Issue) bool {
	if fl == nil {
		return true
	}
	if is.Line > 0 && fl.lines[file][is.Line] {
		return true
	}
	ev := strings.TrimSpace(is.Evidence)
	if ev == "" {
		return false
	}
	for _, line := range strings.Split(ev, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 8 {
			continue
		}
		for _, t := range fl.text[file] {
			if strings.Contains(t, line) || strings.Contains(line, t) {
				return true
			}
		}
	}
	return false
}

// roundTests runs a fix's tests: once on the targeted files before a round
// (the baseline), and on what the round changed after it. A test that
// fails after and didn't before is one the round broke.
type roundTests struct {
	tester   *triage.Tester
	dir      string
	baseline map[string]bool // failing tests' keys, by package tested
	tested   map[string]bool // packages (or "" for a command) with a baseline
	last     *triage.TestRun
}

func newRoundTests(o options, old *PRResult, dir string) *roundTests {
	tr, _ := testerFor(o, old.PR)
	if tr == nil {
		return nil
	}
	return &roundTests{tester: tr, dir: dir, baseline: map[string]bool{}, tested: map[string]bool{}}
}

func failureKey(f triage.TestFailure) string { return f.Package + "\x00" + f.Test }

// testScope is the packages a set of files' tests run in: their
// directories for Go, "" for a command that tests everything.
func (rt *roundTests) testScope(files map[string]bool) map[string]bool {
	out := map[string]bool{}
	goRepo := rt.tester.Policy.Command == "" && fileExistsAt(rt.dir, "go.mod")
	for p := range files {
		switch {
		case !goRepo:
			out[""] = true
		case strings.HasSuffix(p, ".go"):
			out[filepath.ToSlash(filepath.Dir(filepath.FromSlash(p)))] = true
		}
	}
	return out
}

func fileExistsAt(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// source is a Source that names files, which is all the tester reads.
func filesSource(files map[string]bool) *triage.Source {
	src := &triage.Source{}
	for p := range files {
		src.Files = append(src.Files, triage.FileDiff{Path: p, Status: triage.StatusModified})
	}
	return src
}

// before runs the baseline for the packages of files that have none yet.
func (rt *roundTests) before(ctx context.Context, files map[string]bool) {
	todo := map[string]bool{}
	for pkg := range rt.testScope(files) {
		if !rt.tested[pkg] {
			todo[pkg] = true
		}
	}
	if len(todo) == 0 {
		return
	}
	run := rt.tester.Run(ctx, rt.dir, "", filesSource(pkgFiles(todo, files)))
	for pkg := range todo {
		rt.tested[pkg] = true
	}
	for _, f := range run.Failures {
		rt.baseline[failureKey(f)] = true
	}
}

// pkgFiles are the files of files in pkgs.
func pkgFiles(pkgs, files map[string]bool) map[string]bool {
	out := map[string]bool{}
	for p := range files {
		d := filepath.ToSlash(filepath.Dir(filepath.FromSlash(p)))
		if pkgs[d] || pkgs[""] {
			out[p] = true
		}
	}
	return out
}

// after runs the tests of what a round changed and returns the failures
// that are new since the baseline. A run that couldn't happen breaks
// nothing.
func (rt *roundTests) after(ctx context.Context, changed []triage.FileDiff) []triage.TestFailure {
	files := map[string]bool{}
	for _, f := range changed {
		files[f.Path] = true
	}
	if len(rt.testScope(files)) == 0 {
		return nil
	}
	run := rt.tester.Run(ctx, rt.dir, "", filesSource(files))
	rt.last = run
	if !run.Ran {
		return nil
	}
	var broke []triage.TestFailure
	for _, f := range run.Failures {
		if !rt.baseline[failureKey(f)] {
			broke = append(broke, f)
		}
	}
	return broke
}

// testsNote is what the next round is told about the tests a round broke.
func testsNote(round int, broke []triage.TestFailure) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Round %d's change was undone because it broke tests that passed before it. Fix the issues again without breaking them:\n", round)
	for i, f := range broke {
		if i == 5 {
			fmt.Fprintf(&b, "...and %d more.\n", len(broke)-i)
			break
		}
		fmt.Fprintf(&b, "- %s:\n%s\n", f.Name(), indent(firstLinesOf(f.Output, 20)))
	}
	return b.String()
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

func firstLinesOf(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	return strings.Join(lines, "\n")
}

// ownFixCommits counts the commits at dir's HEAD, newest first, that are
// pr-manager's fixes, up to n.
func ownFixCommits(dir string, n int) int {
	out, err := triage.Git(dir, "log", fmt.Sprintf("-%d", n), "--format=%B%x00")
	if err != nil {
		return 0
	}
	count := 0
	for _, msg := range strings.Split(out, "\x00") {
		if strings.TrimSpace(msg) == "" {
			continue
		}
		if !strings.Contains(msg, fixTrailer) {
			break
		}
		count++
	}
	return count
}

// withFixTrailer adds fixTrailer to a commit message.
func withFixTrailer(msg string) string {
	msg = strings.TrimSpace(msg)
	if strings.Contains(msg, fixTrailer) {
		return msg
	}
	return msg + "\n\n" + fixTrailer
}
