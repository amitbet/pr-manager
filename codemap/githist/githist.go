// Package githist reads a repo's commit history with plain git and turns it
// into per-file defect signals: fix and revert commits, recent churn,
// distinct authors, author experience and files that change together.
//
// It only reads commit trees (git log --name-status --no-renames), never
// file contents, so it is cheap on blobless clones. Renames are not
// followed: a moved file starts a new history.
package githist

import (
	"fmt"
	"math"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/internal/proc"
)

type Commit struct {
	Hash    string    `json:"h"`
	Time    time.Time `json:"t"`
	Email   string    `json:"e"` // lower-cased author email
	Subject string    `json:"s"`
	Files   []Change  `json:"f"`
}

type Change struct {
	Status byte   `json:"s"` // A M D T
	Path   string `json:"p"`
}

// Log returns the non-merge commits reachable from rev, newest first,
// committed after since (zero: no limit), at most max (0: no limit).
func Log(dir, rev string, since time.Time, limit int) ([]Commit, error) {
	args := []string{"-C", dir, "-c", "core.quotePath=false", "-c", "log.showSignature=false", "log", "--no-merges", "--no-renames", "--name-status", "--format=%x1e%H%x1f%ct%x1f%ae%x1f%s"}
	if !since.IsZero() {
		args = append(args, "--since="+strconv.FormatInt(since.Unix(), 10))
	}
	if limit > 0 {
		args = append(args, "-n", strconv.Itoa(limit))
	}
	args = append(args, rev, "--")
	out, err := proc.Command("git", args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git log in %s: %w: %s", dir, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("git log in %s: %w", dir, err)
	}
	return Parse(string(out)), nil
}

// Parse reads the output format Log asks git for.
func Parse(out string) []Commit {
	var cs []Commit
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		head, body, _ := strings.Cut(rec, "\n")
		f := strings.SplitN(head, "\x1f", 4)
		if len(f) < 4 {
			continue
		}
		ts, _ := strconv.ParseInt(f[1], 10, 64)
		c := Commit{Hash: f[0], Time: time.Unix(ts, 0).UTC(), Email: strings.ToLower(f[2]), Subject: f[3]}
		for _, l := range strings.Split(body, "\n") {
			st, p, ok := strings.Cut(l, "\t")
			if !ok || st == "" {
				continue
			}
			c.Files = append(c.Files, Change{Status: st[0], Path: unquote(p)})
		}
		cs = append(cs, c)
	}
	return cs
}

// unquote decodes a path git C-quoted (control characters, quotes and
// backslashes stay quoted even with core.quotePath off).
func unquote(p string) string {
	if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
		if u, err := strconv.Unquote(p); err == nil {
			return u
		}
	}
	return p
}

// Authors returns the distinct author emails of base..head.
func Authors(dir, base, head string) ([]string, error) {
	out, err := proc.Command("git", "-C", dir, "log", "--no-merges", "--format=%ae", base+".."+head).Output()
	if err != nil {
		return nil, fmt.Errorf("git log %s..%s: %w", base, head, err)
	}
	seen := map[string]bool{}
	var list []string
	for _, e := range strings.Fields(strings.ToLower(string(out))) {
		if !seen[e] {
			seen[e] = true
			list = append(list, e)
		}
	}
	return list, nil
}

// IsAncestor reports whether commit a is an ancestor of (or is) b.
func IsAncestor(dir, a, b string) bool {
	return proc.Command("git", "-C", dir, "merge-base", "--is-ancestor", a, b).Run() == nil
}

// HeadTime is the commit time of rev.
func HeadTime(dir, rev string) (time.Time, error) {
	out, err := proc.Command("git", "-C", dir, "show", "-s", "--format=%ct", rev).Output()
	if err != nil {
		return time.Time{}, err
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return time.Unix(ts, 0).UTC(), err
}

// Config says how commits are read.
type Config struct {
	Days     int // history window, counted back from the reference time
	HalfLife int // days; a commit this old counts half
	// Bulk: commits touching more files than this are mass edits
	// (formatting, renames, dependency bumps) and do not count as churn or
	// co-change. They still count as fixes and reverts.
	Bulk   int
	Fix    *regexp.Regexp // subject of a bug-fix commit
	NotFix *regexp.Regexp // subjects that match Fix but are not bug fixes
	Revert *regexp.Regexp
}

func DefaultConfig() Config {
	return Config{
		Days: 365, HalfLife: 180, Bulk: 40,
		Fix:    regexp.MustCompile(`(?i)\b(fix(es|ed|ing)?|bug(fix)?|hotfix|regression|incident|outage|crash(es|ed)?|panic(s|ed)?|broken|leak)\b`),
		NotFix: regexp.MustCompile(`(?i)\b(typos?|lint(er|ing)?|flaky|spelling|readme|docs?|comments?|format(ting)?)\b`),
		Revert: regexp.MustCompile(`^(Revert|revert)\b`),
	}
}

// Kind classifies a commit subject: "revert", "fix" or "".
func (c Config) Kind(subject string) string {
	switch {
	case c.Revert != nil && c.Revert.MatchString(subject):
		return "revert"
	case c.Fix != nil && c.Fix.MatchString(subject) && (c.NotFix == nil || !c.NotFix.MatchString(subject)):
		return "fix"
	}
	return ""
}

// FileStats is one file's history inside the window.
type FileStats struct {
	Commits, Fixes, Reverts int
	Recent, RecentFixes     float64
	Authors                 map[string]int // email -> commits
	Last                    time.Time
	Created                 string // email of the commit that added it, if in the window
}

// Stats is a repo's history summary.
type Stats struct {
	Ref   time.Time
	Days  int
	Files map[string]*FileStats
	// Pairs counts, per file, the non-bulk commits it shared with each
	// other file.
	Pairs map[string]map[string]int
	// AuthorCommits is every author's commit count in the window.
	AuthorCommits map[string]int
}

// Skip reports files that say nothing about co-change or authorship:
// lockfiles and vendored code.
func Skip(p string) bool {
	switch path.Base(p) {
	case "go.sum", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "Cargo.lock", "poetry.lock", "uv.lock":
		return true
	}
	return strings.HasPrefix(p, "vendor/") || strings.Contains(p, "/vendor/") || strings.Contains(p, "node_modules/")
}

// Summarize folds commits (any order) into per-file stats, relative to ref.
// Commits before ref-Days or after ref are ignored.
func Summarize(commits []Commit, ref time.Time, cfg Config) *Stats {
	st := &Stats{Ref: ref, Days: cfg.Days, Files: map[string]*FileStats{}, Pairs: map[string]map[string]int{}, AuthorCommits: map[string]int{}}
	from := ref.AddDate(0, 0, -cfg.Days)
	hl := float64(max(1, cfg.HalfLife))
	// oldest first, so Created is the first add in the window
	sorted := append([]Commit(nil), commits...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })
	for _, c := range sorted {
		if c.Time.Before(from) || c.Time.After(ref.Add(time.Minute)) {
			continue
		}
		st.AuthorCommits[c.Email]++
		kind := cfg.Kind(c.Subject)
		age := math.Max(0, ref.Sub(c.Time).Hours()/24)
		wt := math.Pow(0.5, age/hl)
		var files []string
		for _, ch := range c.Files {
			if !Skip(ch.Path) {
				files = append(files, ch.Path)
			}
		}
		bulk := cfg.Bulk > 0 && len(files) > cfg.Bulk
		for _, ch := range c.Files {
			if Skip(ch.Path) {
				continue
			}
			fs := st.Files[ch.Path]
			if fs == nil {
				fs = &FileStats{Authors: map[string]int{}}
				st.Files[ch.Path] = fs
			}
			if ch.Status == 'A' && fs.Created == "" {
				fs.Created = c.Email
			}
			if ch.Status == 'D' {
				// A deleted file's history ends; a later re-add starts over,
				// its co-changes too.
				delete(st.Files, ch.Path)
				for q := range st.Pairs[ch.Path] {
					delete(st.Pairs[q], ch.Path)
				}
				delete(st.Pairs, ch.Path)
				continue
			}
			switch kind {
			case "fix":
				fs.Fixes++
				fs.RecentFixes += wt
			case "revert":
				fs.Reverts++
			}
			if !bulk {
				fs.Commits++
				fs.Recent += wt
			}
			fs.Authors[c.Email]++
			if c.Time.After(fs.Last) {
				fs.Last = c.Time
			}
		}
		var live []string // a file deleted here has no history left to pair
		for _, ch := range c.Files {
			if ch.Status != 'D' && !Skip(ch.Path) {
				live = append(live, ch.Path)
			}
		}
		if bulk || len(live) < 2 {
			continue
		}
		for _, a := range live {
			m := st.Pairs[a]
			if m == nil {
				m = map[string]int{}
				st.Pairs[a] = m
			}
			for _, b := range live {
				if a != b {
					m[b]++
				}
			}
		}
	}
	return st
}

// Partner is a file that usually changes with another.
type Partner struct {
	Path string
	N    int     // shared commits
	Conf float64 // N / commits of the file asked about
}

// Partners returns up to limit files that changed with p in at least
// minN commits and at least minConf of p's commits, strongest first.
func (st *Stats) Partners(p string, minN int, minConf float64, limit int) []Partner {
	fs := st.Files[p]
	if fs == nil || fs.Commits == 0 {
		return nil
	}
	var out []Partner
	for q, n := range st.Pairs[p] {
		if _, alive := st.Files[q]; !alive || noPartner(q) {
			continue
		}
		conf := float64(n) / float64(fs.Commits)
		if n >= minN && conf >= minConf {
			out = append(out, Partner{Path: q, N: n, Conf: math.Round(conf*100) / 100})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Path < out[j].Path
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// noPartner reports files that change with everything (manifests,
// changelogs, CI) and so say nothing as co-change partners.
func noPartner(p string) bool {
	switch path.Base(p) {
	case "go.mod", "package.json", "Makefile", "Dockerfile", "CHANGELOG.md", "README.md", "Chart.yaml", "values.yaml":
		return true
	}
	return strings.HasPrefix(p, ".github/") || strings.HasSuffix(p, ".md")
}

// AgeDays is the days from the file's last change to the reference time.
func (st *Stats) AgeDays(fs *FileStats) int {
	if fs == nil || fs.Last.IsZero() {
		return -1
	}
	return int(st.Ref.Sub(fs.Last).Hours() / 24)
}
