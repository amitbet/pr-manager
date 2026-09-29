package triage

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/codemap"
	"github.com/amitbet/pr-manager/codemap/cx"
	"github.com/amitbet/pr-manager/codemap/decls"
	"github.com/amitbet/pr-manager/codemap/githist"
)

// Likelihood is how likely a unit's change is to go wrong: the touched
// code's own history and complexity (from the code map) plus what the change
// does (complexity it adds, author experience, missing tests and partner
// files, how spread out the PR is). 0-100, the sum of capped factors.
type Likelihood struct {
	Score   int              `json:"score"`
	Level   string           `json:"level"` // low | medium | high | critical
	Factors []codemap.Factor `json:"factors,omitempty"`
	Metrics ChangeMetrics    `json:"metrics"`
	Notes   []string         `json:"notes,omitempty"`
}

// ChangeMetrics are the raw measurements behind a unit's likelihood.
type ChangeMetrics struct {
	// Worst changed declaration: cyclomatic complexity at head and at the
	// merge base (0 for new code), and nesting at head.
	Cyclo     int      `json:"cyclo"`
	CycloBase int      `json:"cyclo_base"`
	Nest      int      `json:"nest"`
	Decls     []string `json:"decls,omitempty"` // declarations measured
	Changed   int      `json:"changed"`         // +/- lines
	// Hist is the file's history from the code map; DirFixes the
	// recency-weighted fixes to other files in its directory.
	Hist     *codemap.History `json:"hist,omitempty"`
	DirFixes float64          `json:"dir_fixes,omitempty"`
	// Author experience over the author history window; -1 when unknown.
	AuthorFileCommits int  `json:"author_file_commits"`
	AuthorRepoCommits int  `json:"author_repo_commits"`
	AuthorCreated     bool `json:"author_created,omitempty"`
	// MissingPartners are files that usually change with this one and are
	// not in the PR.
	MissingPartners []string `json:"missing_partners,omitempty"`
	TestGap         bool     `json:"test_gap,omitempty"`
	PRDirs          int      `json:"pr_dirs"`
	FixPR           bool     `json:"fix_pr,omitempty"`
}

// authorDays is how far back author experience is counted.
const authorDays = 730

// likelihoodCtx holds what every unit of one PR shares.
type likelihoodCtx struct {
	w        codemap.LikelihoodWeights
	days     int // map history window
	cm       *CodeMap
	src      *Source
	files    map[string]bool // paths the PR changes (both sides of renames)
	testDirs map[string]bool // directories with a changed test file
	dirs     int
	fixPR    bool
	authors  []string
	hist     *githist.Stats // author history at the base; nil if unknown
	histNote string
	decls    map[string][]cx.Decl // "head:path" / "base:path"
}

func newLikelihoodCtx(src *Source, cm *CodeMap) *likelihoodCtx {
	lc := &likelihoodCtx{w: codemap.DefaultLikelihoodWeights(), cm: cm, src: src,
		files: map[string]bool{}, testDirs: map[string]bool{}, decls: map[string][]cx.Decl{}}
	if cm != nil {
		lc.w, lc.days = cm.Map.LikelihoodWeights(), cm.Map.Meta.HistoryDays
	}
	dirs := map[string]bool{}
	for _, f := range src.Files {
		lc.files[f.Path] = true
		if f.OldPath != "" {
			lc.files[f.OldPath] = true
		}
		dirs[path.Dir(f.Path)] = true
		if isTestPath(f.Path) {
			lc.testDirs[path.Dir(f.Path)] = true
		}
	}
	lc.dirs = len(dirs)
	lc.fixPR = src.Title != "" && githist.DefaultConfig().Kind(src.Title) == "fix"
	lc.loadAuthors()
	return lc
}

// loadAuthors reads the PR's authors and the base's history once. Without a
// git checkout (a saved diff) author experience is unknown.
func (lc *likelihoodCtx) loadAuthors() {
	s := lc.src
	if s.Dir == "" || s.Base == "" || s.Head == "" {
		lc.histNote = "author experience unknown (no git checkout)"
		return
	}
	authors, err := githist.Authors(s.Dir, s.Base, s.Head)
	if err != nil || len(authors) == 0 {
		lc.histNote = "author experience unknown (no PR commits found)"
		return
	}
	for _, a := range authors {
		if !strings.Contains(a, "[bot]") {
			lc.authors = append(lc.authors, a)
		}
	}
	if len(lc.authors) == 0 {
		lc.histNote = "authored by a bot: author experience not scored"
		return
	}
	ref, err := githist.HeadTime(s.Dir, s.Base)
	if err != nil {
		lc.histNote = "author experience unknown: " + err.Error()
		return
	}
	cs, err := githist.Log(s.Dir, s.Base, ref.AddDate(0, 0, -authorDays), 50000)
	if err != nil {
		lc.histNote = "author experience unknown: " + err.Error()
		return
	}
	cfg := githist.DefaultConfig()
	cfg.Days = authorDays
	lc.hist = githist.Summarize(cs, ref.Add(time.Minute), cfg)
}

// assess scores one unit. Units a rule skipped (generated, formatting,
// pure renames) score 0: nothing a person wrote changed.
func (lc *likelihoodCtx) assess(u *Unit, f FileDiff) *Likelihood {
	lk := &Likelihood{Metrics: ChangeMetrics{AuthorFileCommits: -1, AuthorRepoCommits: -1, PRDirs: lc.dirs, FixPR: lc.fixPR}}
	if u.Decision.Source == "rule" && u.Decision.Bucket == BucketNone {
		lk.Level = "low"
		lk.Notes = []string{"not scored: " + u.Decision.Reason}
		return lk
	}
	if isTestPath(f.Path) {
		// Defects in tests don't reach production; weakened tests are the
		// classifier's call (tiers pins the ones it sent to human).
		lk.Level = "low"
		lk.Notes = []string{"not scored: test code"}
		return lk
	}
	m := &lk.Metrics
	var t codemap.Tally
	add, del := u.Added()
	m.Changed = add + del
	oldPath := f.Path
	if f.OldPath != "" {
		oldPath = f.OldPath
	}

	// The code's own history, from the map.
	if lc.cm != nil && f.Status != StatusAdded {
		file, dir := lc.cm.Place(oldPath)
		if file != nil {
			m.Hist = file.Hist
			lc.w.AddHistory(&t, file.Hist, lc.days)
			for _, p := range file.CoChange {
				if !lc.files[p.Path] {
					m.MissingPartners = append(m.MissingPartners, p.Path)
				}
			}
		} else {
			lk.Notes = append(lk.Notes, "file not in the code map: no history")
		}
		if dir != nil && dir.Hist != nil {
			own := 0.0
			if file != nil && file.Hist != nil {
				own = file.Hist.RecentFixes
			}
			m.DirFixes = max(0, dir.Hist.RecentFixes-own)
			t.Add("dir_fixes", lc.w.DirFix.Of(m.DirFixes), "%.1f recency-weighted fixes to other files in %s/", m.DirFixes, path.Dir(oldPath))
		}
	} else if lc.cm == nil {
		lk.Notes = append(lk.Notes, "no code map: no file history")
	}

	// Complexity of what changed.
	if c, ok := lc.complexity(u, f, oldPath); ok {
		m.Cyclo, m.CycloBase, m.Nest, m.Decls = c.cyclo, c.base, c.nest, c.names
		lc.w.AddComplexity(&t, &codemap.Complexity{Cyclo: c.cyclo, Nest: c.nest})
		if c.delta > 0 {
			what := fmt.Sprintf("the change adds %d to cyclomatic complexity (%d → %d)", c.delta, c.cyclo-c.delta, c.cyclo)
			if c.base == 0 && c.delta == c.cyclo {
				what = fmt.Sprintf("new code with cyclomatic complexity %d", c.cyclo)
			}
			t.Add("complexity_added", lc.w.CycloDelta.Of(float64(c.delta)), "%s", what)
		}
	}
	t.Add("size", lc.w.Size.Of(float64(m.Changed)), "%d changed lines", m.Changed)

	// Who is changing it.
	if lc.hist != nil {
		m.AuthorFileCommits, m.AuthorRepoCommits = 0, 0
		fs := lc.hist.Files[oldPath]
		for _, a := range lc.authors {
			m.AuthorRepoCommits += lc.hist.AuthorCommits[a]
			if fs != nil {
				m.AuthorFileCommits += fs.Authors[a]
				if fs.Created == a {
					m.AuthorCreated = true
				}
			}
		}
		if f.Status != StatusAdded && m.AuthorFileCommits == 0 {
			t.Add("new_to_file", lc.w.NewToFile.Of(1), "the PR's author has not changed this file in %d days", authorDays)
		}
		if m.AuthorRepoCommits < lc.w.NewcomerCommits {
			t.Add("newcomer", lc.w.Newcomer.Of(1), "the PR's author has %d commit%s in this repo in %d days", m.AuthorRepoCommits, plural(m.AuthorRepoCommits), authorDays)
		}
	} else if lc.histNote != "" {
		lk.Notes = append(lk.Notes, lc.histNote)
	}

	// What the PR leaves out, and how spread out it is.
	if n := len(m.MissingPartners); n > 0 {
		t.Add("cochange", lc.w.CoChange.Of(float64(n)), "usually changes with %s, which this PR does not touch", strings.Join(m.MissingPartners, ", "))
	}
	// What a linter already knows. The reviewer is told not to repeat
	// these, so this is where they move the unit.
	if n := len(u.Lint); n > 0 {
		t.Add("lint", lc.w.Lint.Of(float64(n)), "static analysis reports %d problem%s on the added lines (%s)", n, plural(n), lintFactorDetail(u.Lint))
	}
	if lc.testGap(u, f) {
		m.TestGap = true
		t.Add("test_gap", lc.w.TestGap.Of(1), "code changed and no test in %s/ did", path.Dir(f.Path))
	}
	t.Add("diffusion", lc.w.Diffusion.Of(float64(lc.dirs)), "the PR touches %d directories", lc.dirs)
	if lc.fixPR {
		t.Add("fix_pr", lc.w.FixPR.Of(1), "the PR is a bug fix (%q)", lc.src.Title)
	}

	lk.Score, lk.Factors = t.Score(), t.Factors
	lk.Level = scoreLevel(lc.cm, lk.Score)
	return lk
}

// lintFactorDetail names the tools and rules behind the lint factor,
// shortest useful form: "golangci-lint errcheck, secrets aws-access-key".
func lintFactorDetail(fs []LintFinding) string {
	var out []string
	seen := map[string]bool{}
	for _, f := range fs {
		if l := f.Label(); !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	if len(out) > 3 {
		return strings.Join(out[:3], ", ") + ", …"
	}
	return strings.Join(out, ", ")
}

type unitCx struct {
	cyclo, base, nest, delta int
	names                    []string
}

// complexity measures the declarations a unit changes: those whose head
// range holds an added line, or whose base range holds a removed one. A Go
// unit's own declaration always counts.
func (lc *likelihoodCtx) complexity(u *Unit, f FileDiff, oldPath string) (unitCx, bool) {
	var head, base []cx.Decl
	if f.Status != StatusDeleted {
		head = lc.fileDecls("head", f.Path, lc.src.Content)
	}
	if f.Status != StatusAdded {
		base = lc.fileDecls("base", oldPath, lc.src.BaseContent)
	}
	if head == nil && base == nil {
		return unitCx{}, false
	}
	picked := map[string]bool{}
	if u.Symbol != "" {
		for _, d := range head {
			if d.Name == u.Symbol {
				picked[d.Name] = true
			}
		}
	}
	added, removed := changedLines(u.Hunks)
	for _, d := range head {
		for _, n := range added {
			if n >= d.Start && n <= d.End {
				picked[d.Name] = true
				break
			}
		}
	}
	for _, d := range base {
		for _, n := range removed {
			if n >= d.Start && n <= d.End {
				picked[d.Name] = true
				break
			}
		}
	}
	if len(picked) == 0 {
		return unitCx{}, false
	}
	byName := func(ds []cx.Decl) map[string]cx.Decl {
		m := map[string]cx.Decl{}
		for _, d := range ds {
			m[d.Name] = d
		}
		return m
	}
	h, b := byName(head), byName(base)
	var c unitCx
	for _, d := range append(append([]cx.Decl(nil), head...), base...) {
		if !picked[d.Name] {
			continue
		}
		picked[d.Name] = false // once
		c.names = append(c.names, d.Name)
		hd, hok := h[d.Name]
		bd := b[d.Name]
		if hok && hd.Cyclo > c.cyclo {
			c.cyclo, c.base = hd.Cyclo, bd.Cyclo
		}
		if hok {
			c.nest = max(c.nest, hd.Nest)
			c.delta = max(c.delta, hd.Cyclo-bd.Cyclo)
		}
	}
	return c, true
}

func (lc *likelihoodCtx) fileDecls(side, p string, content ContentFunc) []cx.Decl {
	key := side + ":" + p
	if ds, ok := lc.decls[key]; ok {
		return ds
	}
	var ds []cx.Decl
	if content != nil && !isTestPath(p) {
		if src, err := content(p); err == nil {
			ds = cx.FileDecls(p, src)
		}
	}
	lc.decls[key] = ds
	return ds
}

// changedLines returns the new-file lines a unit adds and the base-file
// lines it removes.
func changedLines(hunks []Hunk) (added, removed []int) {
	for _, h := range hunks {
		o, n := h.OldStart, h.NewStart
		for _, l := range h.Lines {
			switch {
			case strings.HasPrefix(l, "+"):
				added = append(added, n)
				n++
			case strings.HasPrefix(l, "-"):
				removed = append(removed, o)
				o++
			case strings.HasPrefix(l, "\\"):
			default:
				o++
				n++
			}
		}
	}
	return added, removed
}

// testGap: a source change with no test changed next to it. For Go that is
// any _test.go in the package directory. For TS/JS, Java and Python, where
// many directories have no tests at all, only when the file has a test at
// the base (by the usual naming conventions) that the PR left alone.
func (lc *likelihoodCtx) testGap(u *Unit, f FileDiff) bool {
	if len(u.Hunks) == 0 || f.Status == StatusDeleted || isTestPath(f.Path) {
		return false
	}
	dir := path.Dir(f.Path)
	switch {
	case strings.HasSuffix(f.Path, ".go"):
		return !lc.testDirs[dir]
	case lc.src.BaseContent == nil:
		return false
	}
	for _, t := range testCandidates(f.Path) {
		if _, err := lc.src.BaseContent(t); err == nil {
			return !lc.files[t]
		}
	}
	return false
}

// testCandidates lists where a TS/JS, Java, Python, C#, Rust, Kotlin, Scala,
// Ruby, Swift, Dart, PHP, C/C++ or PowerShell file's own test usually
// lives. Rust unit tests usually sit in the file itself, so only the
// separate tests.rs and tests/ conventions count.
func testCandidates(p string) []string {
	dir, ext := path.Dir(p), path.Ext(p)
	stem := strings.TrimSuffix(p, ext)
	base := path.Base(stem)
	switch {
	case isTSSource(p):
		return []string{stem + ".test" + ext, stem + ".spec" + ext, path.Join(dir, "__tests__", base+".test"+ext)}
	case ext == ".java":
		// src/main/java/x/Foo.java -> src/test/java/x/FooTest.java
		i := strings.LastIndex(stem, "src/main/")
		if i < 0 {
			return nil
		}
		t := stem[:i] + "src/test/" + stem[i+len("src/main/"):]
		return []string{t + "Test.java", t + "Tests.java", t + "IT.java"}
	case ext == ".py":
		return []string{path.Join(dir, "test_"+base+".py"), path.Join(dir, base+"_test.py"),
			path.Join(dir, "tests", "test_"+base+".py"), path.Join("tests", "test_"+base+".py")}
	case ext == ".cs":
		// src/Acme.Orders/Svc/Foo.cs -> tests/Acme.Orders.Tests/Svc/FooTests.cs
		out := []string{stem + "Tests.cs", stem + "Test.cs"}
		if proj, sub, ok := strings.Cut(strings.TrimPrefix(stem, "src/"), "/"); ok {
			for _, root := range []string{"tests/", "test/", ""} {
				out = append(out, root+proj+".Tests/"+sub+"Tests.cs")
			}
		}
		return out
	case ext == ".rs":
		// crate/src/a/foo.rs -> foo_tests.rs, foo/tests.rs, crate/tests/foo.rs
		out := []string{stem + "_tests.rs", path.Join(stem, "tests.rs")}
		if i := strings.LastIndex(stem, "src/"); i >= 0 {
			out = append(out, stem[:i]+"tests/"+base+".rs")
		}
		return out
	case ext == ".kt" || ext == ".scala":
		// src/main/kotlin/x/Foo.kt -> src/test/kotlin/x/FooTest.kt
		i := strings.LastIndex(stem, "src/main/")
		if i < 0 {
			return nil
		}
		t := stem[:i] + "src/test/" + stem[i+len("src/main/"):]
		return []string{t + "Test" + ext, t + "Tests" + ext, t + "Spec" + ext, t + "Suite" + ext}
	case ext == ".rb":
		// lib/shop/foo.rb -> spec/shop/foo_spec.rb; app/models/foo.rb -> spec/models/foo_spec.rb
		rel := stem
		for _, pre := range []string{"lib/", "app/"} {
			if i := strings.Index(stem, pre); i >= 0 && (i == 0 || stem[i-1] == '/') {
				rel = stem[i+len(pre):]
				break
			}
		}
		return []string{"spec/" + rel + "_spec.rb", "test/" + rel + "_test.rb", stem + "_spec.rb", stem + "_test.rb"}
	case ext == ".swift":
		// Sources/Orders/Foo.swift -> Tests/OrdersTests/FooTests.swift
		if mod, sub, ok := strings.Cut(strings.TrimPrefix(stem, "Sources/"), "/"); ok && strings.HasPrefix(stem, "Sources/") {
			return []string{"Tests/" + mod + "Tests/" + sub + "Tests.swift", "Tests/" + mod + "Tests/" + sub + "Test.swift"}
		}
		return []string{stem + "Tests.swift"}
	case ext == ".dart":
		// lib/src/foo.dart -> test/src/foo_test.dart, test/foo_test.dart
		rel := strings.TrimPrefix(stem, "lib/")
		return []string{"test/" + rel + "_test.dart", path.Join("test", base+"_test.dart"), stem + "_test.dart"}
	case ext == ".php":
		// src/Orders/Foo.php -> tests/Orders/FooTest.php, tests/Unit/Orders/FooTest.php
		rel := strings.TrimPrefix(strings.TrimPrefix(stem, "src/"), "app/")
		return []string{"tests/" + rel + "Test.php", "tests/Unit/" + rel + "Test.php", stem + "Test.php"}
	case ext == ".ps1" || ext == ".psm1":
		return []string{stem + ".Tests.ps1", path.Join(dir, "tests", base+".Tests.ps1"), path.Join("tests", base+".Tests.ps1")}
	case decls.LangFor(p) != nil && (decls.LangFor(p).Family == "c"):
		return []string{stem + "_test" + ext, stem + "_unittest" + ext, path.Join(dir, "test", base+"_test"+ext),
			path.Join("tests", base+"_test"+ext), path.Join("test", base+"_test"+ext)}
	}
	return nil
}

func isTSSource(p string) bool {
	switch path.Ext(p) {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		return true
	}
	return false
}

func isTestPath(p string) bool {
	b := path.Base(p)
	switch path.Ext(b) {
	case ".java":
		if strings.Contains(p, "src/test/") || strings.HasSuffix(b, "Test.java") || strings.HasSuffix(b, "Tests.java") || strings.HasSuffix(b, "IT.java") {
			return true
		}
	case ".py":
		if strings.HasPrefix(b, "test_") || strings.HasSuffix(b, "_test.py") || b == "conftest.py" ||
			strings.HasPrefix(p, "tests/") || strings.Contains(p, "/tests/") {
			return true
		}
	case ".cs":
		if strings.HasSuffix(b, "Tests.cs") || strings.HasSuffix(b, "Test.cs") || strings.Contains(p, ".Tests/") ||
			strings.HasPrefix(p, "tests/") || strings.Contains(p, "/tests/") || strings.HasPrefix(p, "test/") || strings.Contains(p, "/test/") {
			return true
		}
	case ".rs":
		if b == "tests.rs" || strings.HasSuffix(b, "_tests.rs") || strings.HasSuffix(b, "_test.rs") ||
			strings.HasPrefix(p, "tests/") || strings.Contains(p, "/tests/") ||
			strings.HasPrefix(p, "benches/") || strings.Contains(p, "/benches/") {
			return true
		}
	}
	if l := decls.LangFor(p); l != nil && l.IsTest(p) {
		return true
	}
	return strings.HasSuffix(b, "_test.go") || strings.Contains(b, ".test.") || strings.Contains(b, ".spec.") ||
		strings.Contains(p, "__tests__/") || strings.Contains(p, "/testdata/") || strings.HasPrefix(p, "testdata/")
}

// scoreLevel uses the map's level cut-offs, or the defaults without a map.
func scoreLevel(cm *CodeMap, v int) string {
	if cm != nil && len(cm.Map.Meta.Levels) > 0 {
		return cm.Map.Level(v)
	}
	switch {
	case v >= 75:
		return "critical"
	case v >= 55:
		return "high"
	case v >= 35:
		return "medium"
	}
	return "low"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// likelihoodContext is the prompt text explaining a unit's likelihood.
func likelihoodContext(l *Likelihood) string {
	if l == nil || len(l.Factors) == 0 {
		return ""
	}
	var parts []string
	for _, f := range l.Factors[:min(5, len(l.Factors))] {
		parts = append(parts, f.Detail)
	}
	return fmt.Sprintf("Defect likelihood %d/100 (%s): %s.\n", l.Score, l.Level, strings.Join(parts, "; "))
}

// Risk orders units inside a bucket: impact times likelihood, with an
// unknown impact counted as medium.
func (u *Unit) Risk() int {
	imp := 50
	if u.Impact.Known() {
		imp = u.Impact.Score
	}
	lk := 0
	if u.Likelihood != nil {
		lk = u.Likelihood.Score
	}
	return imp * lk
}
