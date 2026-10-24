package triage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/internal/proc"
)

// The tests stage runs the repository's own tests over the packages a
// change touches, as a fact for the review rather than a model's opinion.
// The command comes from the operator (TestPolicy), never from the PR: the
// PR's .triage.yaml is untrusted. The code the command runs is the PR's,
// so the caller decides whether this PR's code may run at all (see the
// trusted-author rule where the pipeline is built). It runs with a
// timeout, in a process group killed on cancel, and without network
// beyond localhost where the platform can do that.
//
// A test that fails at the head is run again at the merge base: one that
// fails there too was broken before the PR (a low issue), one that passes
// there, or is new, is a regression (high). Go tests are run one by one
// with -json, so a failure lands on the unit it is about; other commands
// only pass or fail as a whole. Go coverage of the changed lines is kept
// per unit: covered, passing code is evidence a clean review can count.

// TestPolicy says whether and how tests run.
type TestPolicy struct {
	Enabled bool
	// Command replaces the detected one; it runs through the shell in the
	// repository's root, and only passes or fails as a whole.
	Command string
	// TimeoutSec caps each run (0: DefaultTestTimeoutSec).
	TimeoutSec int
	// Network lets the tests reach the network; off, only localhost.
	Network bool
}

const DefaultTestTimeoutSec = 600

// TestRun is what one run of the tests found, on a result.
type TestRun struct {
	Command string `json:"command,omitempty"`
	// Ran is false when nothing ran; Skipped says why.
	Ran     bool   `json:"ran"`
	Skipped string `json:"skipped,omitempty"`
	Passed  int    `json:"passed"`
	Failed  int    `json:"failed"`
	// Regressions counts the failures that pass, or don't exist, at the
	// merge base.
	Regressions int           `json:"regressions"`
	Failures    []TestFailure `json:"failures,omitempty"`
	// Output is the tail of what a command that isn't parsed test by test
	// wrote, when it failed.
	Output     string `json:"output,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Network    bool   `json:"network,omitempty"`
}

// OK reports whether the tests ran and nothing failed.
func (r *TestRun) OK() bool { return r != nil && r.Ran && r.Failed == 0 }

// Summary is one line about the run, for logs, prompts and banners.
func (r *TestRun) Summary() string {
	switch {
	case r == nil:
		return "tests not run"
	case !r.Ran:
		return "tests not run: " + r.Skipped
	case r.Failed == 0:
		return fmt.Sprintf("tests passed (%d)", r.Passed)
	case r.Regressions > 0:
		return fmt.Sprintf("%d of %d tests failing, %d broken by this change", r.Failed, r.Failed+r.Passed, r.Regressions)
	}
	return fmt.Sprintf("%d of %d tests failing, all failing before this change too", r.Failed, r.Failed+r.Passed)
}

// TestFailure is one failing test, or a package that didn't build.
type TestFailure struct {
	Package string `json:"package,omitempty"` // repository-relative directory for Go
	Test    string `json:"test,omitempty"`    // empty: the package failed as a whole
	Output  string `json:"output,omitempty"`
	// PreExisting: it fails at the merge base too.
	PreExisting bool `json:"pre_existing,omitempty"`
}

// Name is how the failure is shown: "TestFoo in ./pkg".
func (f TestFailure) Name() string {
	where := "./" + f.Package
	if f.Package == "" || f.Package == "." {
		where = "./"
	}
	if f.Test == "" {
		if f.Package == "" {
			return "the test command"
		}
		return where + " (build or setup failed)"
	}
	return f.Test + " in " + where
}

// Coverage is how many of a unit's added statements the passing tests ran.
type Coverage struct {
	Covered    int `json:"covered"`
	Statements int `json:"statements"`
}

// Tester runs tests for a change.
type Tester struct {
	Policy TestPolicy
	// Dir, when set, is the checkout to test as it is (a fix's checkout);
	// otherwise the pipeline's workspace at the head.
	Dir string
	// Result is the last run, set by Run.
	Result *TestRun
	Warn   func(string)
	cov    goCoverage
}

func (t *Tester) on() bool { return t != nil && t.Policy.Enabled }

func (t *Tester) warn(format string, args ...any) {
	if t.Warn != nil {
		t.Warn("tests: " + fmt.Sprintf(format, args...))
	}
}

// Run tests dir (the change at its head). base, when set, is the merge
// base's commit in dir's repository, where failures are checked again.
// It touches no unit, so it can run beside the review; Apply puts what it
// found on them afterwards. The run is kept in t.Result too.
func (t *Tester) Run(ctx context.Context, dir, base string, src *Source) *TestRun {
	start := time.Now()
	run := t.run(ctx, dir, base, src)
	run.DurationMS = time.Since(start).Milliseconds()
	run.Network = t.Policy.Network
	t.Result, t.cov = run.TestRun, run.cov
	return run.TestRun
}

// Apply puts the last run's findings on units: an issue on the unit each
// failure is about, and coverage, and places the units it changed again.
func (t *Tester) Apply(units []*Unit, tp TierPolicy) {
	if t == nil || t.Result == nil || !t.Result.Ran {
		return
	}
	// A run again replaces what the last one found.
	for _, u := range units {
		kept := u.Issues[:0:0]
		for _, is := range u.Issues {
			if is.Source != TestIssueSource {
				kept = append(kept, is)
			}
		}
		u.Issues, u.Coverage = kept, nil
	}
	placeTestFailures(units, t.Result.Failures, t.cov)
	for _, u := range units {
		tp.afterTests(u)
	}
}

// testRun is a TestRun with what stays in memory.
type testRun struct {
	*TestRun
	cov goCoverage
}

func (t *Tester) run(ctx context.Context, dir, base string, src *Source) testRun {
	if dir == "" {
		return testRun{TestRun: &TestRun{Skipped: "no checkout of the change to run them in"}}
	}
	paths := changedPaths(src)
	if t.Policy.Command != "" {
		return t.runCommand(ctx, dir, base, src)
	}
	if fileExists(filepath.Join(dir, "go.mod")) {
		return t.runGo(ctx, dir, base, paths)
	}
	cmd, why := detectTestCommand(dir)
	if cmd == "" {
		return testRun{TestRun: &TestRun{Skipped: why}}
	}
	c := *t
	c.Policy.Command = cmd
	return c.runCommand(ctx, dir, base, src)
}

// changedPaths are the files the change adds or modifies.
func changedPaths(src *Source) []string {
	var out []string
	for _, f := range src.Files {
		if f.Status != StatusDeleted {
			out = append(out, f.Path)
		}
	}
	return out
}

// detectTestCommand picks the test command of a repository that isn't Go,
// or says why there is none. It never installs anything: installing runs
// the dependencies' scripts.
func detectTestCommand(dir string) (string, string) {
	if b, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		_ = json.Unmarshal(b, &pkg)
		test := pkg.Scripts["test"]
		switch {
		case test == "" || strings.Contains(test, "no test specified"):
			return "", "package.json has no test script"
		case !fileExists(filepath.Join(dir, "node_modules")):
			return "", "node_modules is missing; install the dependencies, or set a test command"
		}
		return "npm test --silent", ""
	}
	if fileExists(filepath.Join(dir, "Cargo.toml")) {
		return "cargo test --offline --quiet", ""
	}
	for _, f := range []string{"pytest.ini", "pyproject.toml", "setup.cfg", "tox.ini"} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil && (f == "pytest.ini" || bytes.Contains(b, []byte("pytest"))) {
			return "python3 -m pytest -q", ""
		}
	}
	return "", "no test command found for this repository; set one in Settings"
}

// runCommand runs a test command that only passes or fails: at head, and
// when it fails, at base too, to tell a regression from a broken build.
func (t *Tester) runCommand(ctx context.Context, dir, base string, src *Source) testRun {
	r := &TestRun{Command: t.Policy.Command}
	out, code, err := t.exec(ctx, dir, nil, shellArgs(t.Policy.Command)...)
	if err != nil {
		r.Skipped = err.Error()
		return testRun{TestRun: r}
	}
	r.Ran = true
	if code == 0 {
		r.Passed = 1
		return testRun{TestRun: r}
	}
	r.Failed = 1
	f := TestFailure{Output: tail(out, 4000)}
	r.Output = f.Output
	if base != "" {
		if bdir, cleanup, err := baseWorktree(ctx, dir, base); err == nil {
			_, bcode, berr := t.exec(ctx, bdir, nil, shellArgs(t.Policy.Command)...)
			cleanup()
			f.PreExisting = berr == nil && bcode != 0
		}
	}
	if !f.PreExisting {
		r.Regressions = 1
	}
	r.Failures = []TestFailure{f}
	return testRun{TestRun: r}
}

func shellArgs(cmd string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", cmd}
	}
	return []string{"sh", "-c", cmd}
}

// runGo runs go test on the changed packages, test by test, with coverage
// of those packages.
func (t *Tester) runGo(ctx context.Context, dir, base string, paths []string) testRun {
	r := &TestRun{}
	pkgs := goTestPackages(dir, paths)
	if len(pkgs) == 0 {
		r.Skipped = "the change touches no Go package"
		return testRun{TestRun: r}
	}
	if _, err := exec.LookPath("go"); err != nil {
		r.Skipped = "go is not installed"
		return testRun{TestRun: r}
	}
	module := goModulePath(dir)
	downloadGoModules(ctx, dir)
	prof, err := os.CreateTemp("", "pr-manager-cover-*.out")
	if err != nil {
		r.Skipped = err.Error()
		return testRun{TestRun: r}
	}
	prof.Close()
	defer os.Remove(prof.Name())
	args := []string{"go", "test", "-json", "-count=1", "-coverprofile=" + prof.Name(), "-coverpkg=" + strings.Join(pkgs, ",")}
	args = append(args, pkgs...)
	r.Command = "go test " + strings.Join(pkgs, " ")
	out, code, err := t.exec(ctx, dir, goTestEnv(), args...)
	if errors.Is(err, errTimedOut) && ctx.Err() == nil {
		// The tests hung past the timeout: that is a failure, not a skip.
		r.Ran, r.Failed = true, 1
		r.Output = tail(out, 4000)
		r.Failures = []TestFailure{{Output: "tests " + err.Error() + "\n" + r.Output}}
		r.Regressions = 1
		return testRun{TestRun: r}
	}
	if err != nil {
		r.Skipped = err.Error()
		return testRun{TestRun: r}
	}
	res := parseGoTestJSON(out, module)
	if res.passed+len(res.failures) == 0 && code == 0 {
		// No test files, or every test skipped: nothing ran, nothing failed.
		r.Skipped = "no tests ran in the changed packages"
		return testRun{TestRun: r}
	}
	if res.passed+len(res.failures) == 0 {
		// Nothing parsed: go itself failed before testing anything.
		r.Ran, r.Failed = true, 1
		r.Output = tail(out, 4000)
		r.Failures = []TestFailure{{Output: r.Output}}
		r.Regressions = 1
		return testRun{TestRun: r}
	}
	r.Ran, r.Passed, r.Failed, r.Failures = true, res.passed, len(res.failures), res.failures
	cov := parseGoCoverProfile(prof.Name(), module)
	if base != "" && len(r.Failures) > 0 {
		t.checkGoBase(ctx, dir, base, module, r.Failures)
	}
	for _, f := range r.Failures {
		if !f.PreExisting {
			r.Regressions++
		}
	}
	return testRun{TestRun: r, cov: cov}
}

// checkGoBase runs the failing tests again at base, marking the ones that
// fail there too.
func (t *Tester) checkGoBase(ctx context.Context, dir, base, module string, fs []TestFailure) {
	bdir, cleanup, err := baseWorktree(ctx, dir, base)
	if err != nil {
		t.warn("merge base not checked out: %v", err)
		return
	}
	defer cleanup()
	byPkg := map[string][]string{}
	for _, f := range fs {
		if goFiles, _ := filepath.Glob(filepath.Join(bdir, filepath.FromSlash(f.Package), "*.go")); len(goFiles) == 0 {
			continue // the package is new: every failure in it is the change's
		}
		byPkg[f.Package] = append(byPkg[f.Package], f.Test)
	}
	if len(byPkg) == 0 {
		return
	}
	// The base may need modules the head doesn't, and the run has no proxy.
	downloadGoModules(ctx, bdir)
	failing := map[string]bool{} // pkg + "\x00" + test
	for pkg, tests := range byPkg {
		args := []string{"go", "test", "-json", "-count=1"}
		var names []string
		whole := false
		for _, n := range tests {
			if n == "" {
				whole = true
			} else {
				names = append(names, regexp.QuoteMeta(topTest(n)))
			}
		}
		if !whole {
			args = append(args, "-run", "^("+strings.Join(dedupe(names), "|")+")$")
		}
		args = append(args, goPkgArg(pkg))
		out, code, err := t.exec(ctx, bdir, goTestEnv(), args...)
		if err != nil {
			continue
		}
		res := parseGoTestJSON(out, goModulePath(bdir))
		// Exit 0 with nothing parsed: the tests don't exist at base, so
		// their failures are the change's.
		if res.passed+len(res.failures) == 0 && code != 0 {
			failing[pkg+"\x00"] = true // doesn't build at base either
		}
		for _, f := range res.failures {
			failing[f.Package+"\x00"+f.Test] = true
		}
	}
	for i := range fs {
		f := &fs[i]
		// A base run that tested nothing only shows the package's build
		// failure is old; it says nothing about the head's failing tests.
		f.PreExisting = failing[f.Package+"\x00"+f.Test] || (f.Test == "" && failing[f.Package+"\x00"])
	}
}

// topTest is a subtest's top-level test: -run matches it, and the
// subtests run with it.
func topTest(name string) string {
	top, _, _ := strings.Cut(name, "/")
	return top
}

func dedupe(s []string) []string {
	sort.Strings(s)
	out := s[:0]
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// goTestPackages are the package directories the changed files are in that
// still hold Go files, as ./dir arguments.
func goTestPackages(dir string, paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if !strings.HasSuffix(p, ".go") || strings.HasPrefix(p, "vendor/") || strings.Contains(p, "/testdata/") || strings.HasPrefix(p, "testdata/") {
			continue
		}
		d := path.Dir(p)
		if seen[d] {
			continue
		}
		seen[d] = true
		matches, _ := filepath.Glob(filepath.Join(dir, filepath.FromSlash(d), "*.go"))
		if len(matches) == 0 {
			continue
		}
		// A package in a nested module isn't the root module's: go test
		// from the root can't run it.
		nested := false
		for a := d; a != "." && a != "/" && a != ""; a = path.Dir(a) {
			if fileExists(filepath.Join(dir, filepath.FromSlash(a), "go.mod")) {
				nested = true
				break
			}
		}
		if nested {
			continue
		}
		out = append(out, goPkgArg(d))
	}
	sort.Strings(out)
	return out
}

func goPkgArg(d string) string {
	if d == "." || d == "" {
		return "."
	}
	return "./" + d
}

var goModuleLine = regexp.MustCompile(`(?m)^module\s+"?([^\s"]+)"?`)

func goModulePath(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	if m := goModuleLine.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// relPkg turns an import path into its directory in the module.
func relPkg(importPath, module string) string {
	switch {
	case importPath == module:
		return "."
	case module != "" && strings.HasPrefix(importPath, module+"/"):
		return strings.TrimPrefix(importPath, module+"/")
	}
	return importPath
}

// goTestEnv keeps go from downloading anything while the tests run: the
// modules were downloaded first, outside the sandbox.
func goTestEnv() []string {
	return []string{"GOPROXY=off", "GOFLAGS=-mod=readonly", "GOTELEMETRY=off"}
}

// downloadGoModules fetches dir's modules before the sandboxed run, which
// has no network. It runs nothing of the repository's.
func downloadGoModules(ctx context.Context, dir string) {
	if !fileExists(filepath.Join(dir, "go.sum")) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := proc.CommandContext(ctx, "go", "mod", "download")
	cmd.Dir = dir
	_ = cmd.Run()
}

// goTestResult is what parseGoTestJSON read.
type goTestResult struct {
	passed   int
	failures []TestFailure
}

// parseGoTestJSON reads go test -json. A failing test keeps its own output;
// a package that failed with no failing test (it didn't build, or TestMain
// failed) is one failure without a test name.
func parseGoTestJSON(out []byte, module string) goTestResult {
	type event struct {
		Action, Package, Test, Output string
		ImportPath                    string
	}
	var res goTestResult
	output := map[string]*strings.Builder{} // pkg \x00 test
	failedTests := map[string]bool{}        // pkg with a failing test
	var order []string
	add := func(k, s string) {
		b := output[k]
		if b == nil {
			b = &strings.Builder{}
			output[k] = b
		}
		if b.Len() < 64<<10 {
			b.WriteString(s)
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		pkg := e.Package
		if pkg == "" {
			pkg = e.ImportPath // build-output events
			// The package ID, "m/b [m/b.test]", names the package the
			// later fail event does inside its brackets.
			if i := strings.Index(pkg, " ["); i >= 0 {
				pkg = strings.TrimSuffix(strings.TrimSuffix(pkg[i+2:], "]"), ".test")
			}
		}
		k := pkg + "\x00" + e.Test
		switch e.Action {
		case "output", "build-output":
			add(k, e.Output)
		case "pass":
			if e.Test != "" {
				res.passed++
			}
		case "fail", "build-fail":
			if e.Test != "" {
				failedTests[pkg] = true
				order = append(order, k)
			} else if !failedTests[pkg] {
				order = append(order, k)
			}
		}
	}
	seen := map[string]bool{}
	for _, k := range order {
		if seen[k] {
			continue
		}
		seen[k] = true
		pkg, test, _ := strings.Cut(k, "\x00")
		// A subtest's failure fails its parent too: keep the subtest.
		if test != "" && hasFailingChild(order, pkg, test) {
			continue
		}
		if test == "" && failedTests[pkg] {
			continue
		}
		text := ""
		if b := output[k]; b != nil {
			text = b.String()
		}
		res.failures = append(res.failures, TestFailure{Package: relPkg(strings.TrimSuffix(pkg, ".test"), module), Test: test, Output: tail([]byte(text), 4000)})
	}
	return res
}

func hasFailingChild(order []string, pkg, test string) bool {
	prefix := pkg + "\x00" + test + "/"
	for _, k := range order {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// goCoverage is, per repository path, the statement lines the profile
// instrumented and the ones a passing test ran.
type goCoverage map[string]map[int]bool

// parseGoCoverProfile reads a coverprofile: "file:l1.c1,l2.c2 stmts count".
// Each line of a block counts once, covered if any run reached it.
func parseGoCoverProfile(name, module string) goCoverage {
	b, err := os.ReadFile(name)
	if err != nil {
		return nil
	}
	cov := goCoverage{}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		file, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) != 3 {
			continue
		}
		span, _, _ := strings.Cut(f[0], " ")
		from, to, ok := strings.Cut(span, ",")
		if !ok {
			continue
		}
		l1, _ := strconv.Atoi(strings.SplitN(from, ".", 2)[0])
		l2, _ := strconv.Atoi(strings.SplitN(to, ".", 2)[0])
		count, _ := strconv.Atoi(f[2])
		rel := relPkg(path.Dir(file), module)
		p := path.Join(rel, path.Base(file))
		if rel == "." {
			p = path.Base(file)
		}
		m := cov[p]
		if m == nil {
			m = map[int]bool{}
			cov[p] = m
		}
		for l := l1; l <= l2 && l-l1 < 10000; l++ {
			m[l] = m[l] || count > 0
		}
	}
	return cov
}

// placeTestFailures puts each failure on the unit it is about, as an issue
// from the tests, and each unit's coverage of its added lines. A failure
// goes on the changed test that failed when it is one; else on the units
// whose lines its output names; else on the package's changed code. A
// regression is high, so it pins the unit to human; one that failed
// before the change is low.
func placeTestFailures(units []*Unit, fs []TestFailure, cov goCoverage) {
	failingPkg := map[string]bool{}
	for _, f := range fs {
		failingPkg[f.Package] = true
		for _, u := range testFailureUnits(units, f) {
			u.Issues = append(u.Issues, testIssue(f))
		}
	}
	if cov == nil {
		return
	}
	for _, u := range units {
		if !strings.HasSuffix(u.File, ".go") || isTestPath(u.File) || failingPkg[path.Dir(u.File)] {
			continue
		}
		lines := cov[u.File]
		if lines == nil {
			continue
		}
		c := &Coverage{}
		for _, l := range unitAddedLines(u) {
			if hit, ok := lines[l]; ok {
				c.Statements++
				if hit {
					c.Covered++
				}
			}
		}
		if c.Statements > 0 {
			u.Coverage = c
		}
	}
}

var testFileLine = regexp.MustCompile(`([\w.-]+\.go):(\d+)`)

func testFailureUnits(units []*Unit, f TestFailure) []*Unit {
	if f.Package == "" {
		return nil // a command's failure is the run's, not a unit's
	}
	inPkg := func(u *Unit) bool {
		return path.Dir(u.File) == f.Package || (f.Package == "." && !strings.Contains(u.File, "/"))
	}
	if f.Test != "" {
		top := topTest(f.Test)
		for _, u := range units {
			if inPkg(u) && isTestPath(u.File) && (u.Symbol == top || strings.HasSuffix(u.Symbol, "."+top)) {
				return []*Unit{u}
			}
		}
	}
	var named []*Unit
	for _, m := range testFileLine.FindAllStringSubmatch(f.Output, -1) {
		line, _ := strconv.Atoi(m[2])
		for _, u := range units {
			if inPkg(u) && path.Base(u.File) == m[1] && unitHasLine(u, line) && !containsUnit(named, u) {
				named = append(named, u)
			}
		}
	}
	if len(named) > 0 {
		return named
	}
	var code []*Unit
	for _, u := range units {
		if inPkg(u) && strings.HasSuffix(u.File, ".go") && !isTestPath(u.File) && len(u.Hunks) > 0 {
			code = append(code, u)
		}
	}
	if len(code) > 0 {
		return code
	}
	for _, u := range units {
		if inPkg(u) && len(u.Hunks) > 0 {
			return []*Unit{u}
		}
	}
	return nil
}

func containsUnit(us []*Unit, u *Unit) bool {
	for _, x := range us {
		if x == u {
			return true
		}
	}
	return false
}

// TestIssueSource is Issue.Source on what the tests found.
const TestIssueSource = "tests"

func testIssue(f TestFailure) Issue {
	is := Issue{Source: TestIssueSource, Title: f.Name() + " fails", Evidence: firstLines(f.Output, 12)}
	if f.PreExisting {
		is.Severity, is.PreExisting = "low", true
		is.Detail = "It fails at the merge base too, so the change didn't break it."
	} else {
		is.Severity = "high"
		is.Detail = "It passes at the merge base, or is new, and fails with this change."
		is.Scenario = "Running the test fails: " + f.Name()
	}
	return is
}

// unitAddedLines are the new-file line numbers u adds.
func unitAddedLines(u *Unit) []int {
	var out []int
	for _, h := range u.Hunks {
		n := h.NewStart
		for _, l := range h.Lines {
			switch {
			case strings.HasPrefix(l, "+"):
				out = append(out, n)
				n++
			case strings.HasPrefix(l, "-"), strings.HasPrefix(l, `\`):
			default:
				n++
			}
		}
	}
	return out
}

// errTimedOut is exec's error for a command that ran out of time.
var errTimedOut = errors.New("timed out")

// exec runs args in dir with the policy's timeout, process group and
// network rule. It returns the combined output and exit code; err is only
// for a command that couldn't run or ran out of time.
func (t *Tester) exec(ctx context.Context, dir string, env []string, args ...string) ([]byte, int, error) {
	timeout := time.Duration(t.Policy.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = DefaultTestTimeoutSec * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args, cleanup := t.sandbox(dir, args)
	defer cleanup()
	cmd := proc.CommandContext(ctx, args[0], args[1:]...)
	proc.KillGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = dir
	// The tests are the PR's code: keep the operator's secrets from them.
	var base []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		n := strings.ToUpper(name)
		if n == "SSH_AUTH_SOCK" || strings.Contains(n, "TOKEN") || strings.Contains(n, "SECRET") || strings.Contains(n, "PASSWORD") ||
			strings.Contains(n, "PASSWD") || strings.Contains(n, "API_KEY") || strings.Contains(n, "ACCESS_KEY") || strings.Contains(n, "CREDENTIAL") {
			continue
		}
		base = append(base, kv)
	}
	cmd.Env = append(append(base, "CI=1", "GIT_TERMINAL_PROMPT=0"), env...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return out, -1, fmt.Errorf("%w after %s", errTimedOut, timeout)
	}
	if ctx.Err() != nil {
		return out, -1, ctx.Err()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, ee.ExitCode(), nil
	}
	if err != nil {
		return out, -1, err
	}
	return out, 0, nil
}

// sandbox wraps args so they reach no network but localhost (DNS through
// mDNSResponder included) and can't read the usual credential stores:
// sandbox-exec on macOS. Elsewhere it returns args as they are; the timeout
// and process group still apply.
func (t *Tester) sandbox(dir string, args []string) ([]string, func()) {
	noop := func() {}
	if runtime.GOOS != "darwin" {
		return args, noop
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		return args, noop
	}
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	if !t.Policy.Network {
		b.WriteString("(deny network-outbound (remote ip \"*:*\"))\n(allow network-outbound (remote ip \"localhost:*\"))\n")
		// DNS goes through mDNSResponder's unix socket, which "remote ip"
		// doesn't cover, and a lookup can carry data out.
		b.WriteString("(deny network-outbound (remote unix-socket (path-literal \"/private/var/run/mDNSResponder\")))\n")
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, p := range []string{".ssh", ".aws", ".gnupg", ".netrc", ".npmrc", ".git-credentials", ".pypirc", ".config/gh", ".docker", ".kube", ".azure", ".config/gcloud", ".codex", ".claude", ".claude.json", "Library/Keychains"} {
			fmt.Fprintf(&b, "(deny file-read* file-write* (subpath %q))\n", filepath.Join(home, p))
		}
		// Files run later outside the sandbox: writing them would escape it.
		for _, p := range []string{".zshrc", ".zshenv", ".zprofile", ".zlogin", ".zlogout", ".bashrc", ".bash_profile", ".bash_login", ".bash_logout", ".profile", ".config/fish", ".gitconfig", ".config/git", "Library/LaunchAgents"} {
			fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", filepath.Join(home, p))
		}
	}
	// Git directories hold hooks and config that git runs outside the
	// sandbox (a fix's commit, git add -A, other cached clones): writing
	// them would escape it too. Any .git is denied, and so are the
	// checkout's own git directories wherever they are.
	b.WriteString("(deny file-write* (regex #\"/\\.git(/|$)\"))\n")
	gitDirs := []string{filepath.Join(dir, ".git")}
	if out, err := Git(dir, "rev-parse", "--absolute-git-dir", "--git-common-dir"); err == nil {
		for _, p := range strings.Split(strings.TrimSpace(out), "\n") {
			if p = strings.TrimSpace(p); p == "" {
				continue
			}
			if !filepath.IsAbs(p) {
				p = filepath.Join(dir, p)
			}
			gitDirs = append(gitDirs, p)
		}
	}
	for _, p := range gitDirs {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", p)
	}
	f, err := os.CreateTemp("", "pr-manager-tests-*.sb")
	if err != nil {
		return args, noop
	}
	_, _ = f.WriteString(b.String())
	f.Close()
	return append([]string{"sandbox-exec", "-f", f.Name()}, args...), func() { os.Remove(f.Name()) }
}

// baseWorktree checks base out next to dir's repository, for the base
// re-run. The worktree has the PR's agent files only as base had them.
func baseWorktree(ctx context.Context, dir, base string) (string, func(), error) {
	tmp, err := os.MkdirTemp("", "pr-manager-base-")
	if err != nil {
		return "", nil, err
	}
	if r, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = r
	}
	if _, err := GitCtx(ctx, dir, "worktree", "add", "--detach", tmp, base); err != nil {
		os.RemoveAll(tmp)
		return "", nil, err
	}
	// The head's ignored dependencies (node_modules), so a test command
	// that needs them doesn't fail at base for want of them. They are
	// unlinked before the worktree goes, so removal can't reach them.
	links := linkIgnoredDeps(dir, tmp, nil)
	return tmp, func() {
		for _, l := range links {
			_ = os.Remove(l)
		}
		_, _ = GitCtx(context.WithoutCancel(ctx), dir, "worktree", "remove", "--force", tmp)
		os.RemoveAll(tmp)
	}, nil
}

func tail(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return s
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	return strings.Join(lines, "\n")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
