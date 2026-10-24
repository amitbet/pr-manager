package main

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/amitbet/pr-manager/codemap/githist"
	"github.com/amitbet/pr-manager/triage"
)

// `pr-manager eval -history N` measures what the triage skips against the
// repository's own history, so the review budget can be loosened on
// evidence rather than hope. A later fix or revert that changed a line
// says the commit that wrote the line had a bug there: that commit is an
// incident, and the unit holding the line should not have been skipped.
// Commits whose lines no later fix touched are the clean set: what of
// them goes to human review is what a person was asked to read for
// nothing (as far as the history knows).
//
// Each commit is reviewed as a single commit (path#sha) with the current
// settings, and its units placed under every budget from the scores, so
// one run reports every step: per budget, how many incident lines landed
// in human, skim and none, and how much of the clean commits' code went
// to human review.

const (
	historyDays     = 365
	historyMaxLines = 40 // lines blamed per fix commit
)

// incident is a line a later fix changed, in the commit that wrote it.
type incident struct {
	commit, file, fix string
	line              int
}

// historySample picks up to n incident commits and n clean ones from dir's
// last historyDays of history.
func historySample(dir string, n int, cfg githist.Config) (map[string][]incident, []string, error) {
	since := time.Now().AddDate(0, 0, -historyDays)
	out, err := triage.Git(dir, "log", "--no-merges", "--since="+since.Format(time.RFC3339), "--format=%H%x00%s")
	if err != nil {
		return nil, nil, err
	}
	var fixes, others []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		sha, subject, ok := strings.Cut(line, "\x00")
		if !ok {
			continue
		}
		if cfg.Kind(subject) != "" {
			fixes = append(fixes, sha)
		} else {
			others = append(others, sha)
		}
	}
	// Every fix is blamed, so no commit a fix touched counts as clean, but
	// incidents are only collected until there are n of them.
	incidents := map[string][]incident{}
	touched := map[string]bool{}
	for _, fix := range fixes {
		full := len(incidents) >= n
		for _, in := range blameFix(dir, fix) {
			touched[in.commit] = true
			if full || in.commit == fix || bulkCommit(dir, in.commit, cfg.Bulk) {
				continue
			}
			incidents[in.commit] = append(incidents[in.commit], in)
		}
	}
	rnd := rand.New(rand.NewSource(1)) // the same sample every run, to compare settings
	rnd.Shuffle(len(others), func(i, j int) { others[i], others[j] = others[j], others[i] })
	var clean []string
	for _, sha := range others {
		if len(clean) >= n {
			break
		}
		if !touched[sha] && !bulkCommit(dir, sha, cfg.Bulk) {
			clean = append(clean, sha)
		}
	}
	return incidents, clean, nil
}

// blameFix is the lines a fix commit changed or removed, each in the
// commit that wrote it, at that commit's line number.
func blameFix(dir, fix string) []incident {
	diff, err := triage.Git(dir, "diff", "-U0", "--no-color", "--no-ext-diff", fix+"^", fix)
	if err != nil {
		return nil
	}
	files, err := triage.ParseDiff(diff)
	if err != nil {
		return nil
	}
	var out []incident
	budget := historyMaxLines
	for _, f := range files {
		if f.Status == triage.StatusAdded || githist.Skip(f.Path) {
			continue
		}
		for _, h := range f.Hunks {
			if h.OldLines == 0 || budget <= 0 {
				continue
			}
			n := min(h.OldLines, budget)
			budget -= n
			blame, err := triage.Git(dir, "blame", "--porcelain", "-L", fmt.Sprintf("%d,+%d", h.OldStart, n), fix+"^", "--", f.OldPath)
			if err != nil {
				continue
			}
			// Each line is a header (sha, line in that commit, ...), a
			// commit's details, which name its file (filename) the first
			// time it shows, then the line itself after a tab. The line
			// number is in the commit's file, which a rename since may
			// have moved.
			fileOf := map[string]string{}
			var cur incident
			have := false
			for _, line := range strings.Split(blame, "\n") {
				if strings.HasPrefix(line, "\t") {
					if have {
						if cur.file = fileOf[cur.commit]; cur.file == "" {
							cur.file = f.OldPath
						}
						out = append(out, cur)
					}
					have = false
					continue
				}
				if name, ok := strings.CutPrefix(line, "filename "); ok {
					if have {
						fileOf[cur.commit] = name
					}
					continue
				}
				fs := strings.Fields(line)
				if len(fs) < 3 || len(fs[0]) != 40 {
					continue
				}
				orig, err := strconv.Atoi(fs[1])
				if err != nil {
					continue
				}
				cur, have = incident{commit: fs[0], line: orig, fix: fix}, true
			}
		}
	}
	return out
}

func bulkCommit(dir, sha string, bulk int) bool {
	out, err := triage.Git(dir, "show", "--name-only", "--format=", sha)
	if err != nil {
		return true
	}
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l != "" {
			n++
		}
	}
	return n == 0 || (bulk > 0 && n > bulk)
}

// historyTally is one budget's counts.
type historyTally struct {
	human, skim, aux, none, unplaced int // incident lines, by bucket
	cleanHuman, cleanCode            int // clean commits' code units: in human, in all
}

// runHistoryEval reviews the sample and prints the tallies per budget.
func runHistoryEval(ctx context.Context, o options, w io.Writer) error {
	dir, err := checkoutRoot(o.dir)
	if err != nil {
		return err
	}
	t, err := newTriager(o)
	if err != nil {
		return err
	}
	incidents, clean, err := historySample(dir, o.historyN, githist.DefaultConfig())
	if err != nil {
		return err
	}
	if len(incidents) == 0 {
		return fmt.Errorf("no fix or revert commits in the last %d days of %s to learn from", historyDays, dir)
	}
	fmt.Fprintf(os.Stderr, "history eval: %d incident commits, %d clean ones, each reviewed as a single commit\n", len(incidents), len(clean))
	tallies := map[string]*historyTally{}
	for _, name := range triage.BudgetNames {
		tallies[name] = &historyTally{}
	}
	review := func(sha string) []*triage.Unit {
		r, err := t.RunLocal(ctx, dir+"#"+sha, jobOptions{}, func(string, int, int) {})
		if err != nil {
			fmt.Fprintf(os.Stderr, "history eval: %s: %v\n", sha[:10], err)
			return nil
		}
		return resultUnitsWithHunks(r)
	}
	shas := make([]string, 0, len(incidents))
	for sha := range incidents {
		shas = append(shas, sha)
	}
	sort.Strings(shas)
	tp := triage.DefaultTierPolicy()
	for i, sha := range shas {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(os.Stderr, "history eval: incident %d/%d %s\n", i+1, len(shas), sha[:10])
		units := review(sha)
		if units == nil {
			continue
		}
		for _, name := range triage.BudgetNames {
			if err := triage.Rebucket(units, tp, name); err != nil {
				return err
			}
			tl := tallies[name]
			for _, in := range incidents[sha] {
				var at *triage.Unit
				for _, u := range units {
					if u.File == in.file && u.HasLine(in.line) {
						at = u
						break
					}
				}
				switch {
				case at == nil:
					tl.unplaced++
				case at.Decision.Bucket == triage.BucketHuman:
					tl.human++
				case at.Decision.Bucket == triage.BucketSkim:
					tl.skim++
				case at.Decision.Bucket == triage.BucketAux:
					tl.aux++
				default:
					tl.none++
				}
			}
		}
	}
	for i, sha := range clean {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(os.Stderr, "history eval: clean %d/%d %s\n", i+1, len(clean), sha[:10])
		units := review(sha)
		for _, name := range triage.BudgetNames {
			if err := triage.Rebucket(units, tp, name); err != nil {
				return err
			}
			for _, u := range units {
				if u.Decision.Source == "rule" || u.Decision.Bucket == triage.BucketAux {
					continue
				}
				tallies[name].cleanCode++
				if u.Decision.Bucket == triage.BucketHuman {
					tallies[name].cleanHuman++
				}
			}
		}
	}
	printHistory(w, tallies)
	return nil
}

func printHistory(w io.Writer, tallies map[string]*historyTally) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "budget\tincident lines in human\tskim\taux\tnone (missed)\tclean code in human")
	pct := func(a, b int) string {
		if b == 0 {
			return "-"
		}
		return fmt.Sprintf("%d%%", (100*a+b/2)/b)
	}
	for _, name := range triage.BudgetNames {
		tl := tallies[name]
		placed := tl.human + tl.skim + tl.aux + tl.none
		fmt.Fprintf(tw, "%s\t%d (%s)\t%d (%s)\t%d\t%d (%s)\t%d of %d (%s)\n", name, tl.human, pct(tl.human, placed), tl.skim, pct(tl.skim, placed), tl.aux, tl.none, pct(tl.none, placed), tl.cleanHuman, tl.cleanCode, pct(tl.cleanHuman, tl.cleanCode))
	}
	tw.Flush()
	fmt.Fprintln(w, "\nIncident lines are lines a later fix or revert changed, in the commit that wrote them; none is a miss. Clean code is units of commits no later fix touched; what of it went to human review was read for nothing, as far as the history knows.")
}
