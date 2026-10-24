package triage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testsGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := Git(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// testsRepo is a Go module whose head commit breaks one test, adds a
// failing one, changes code a passing test covers, and keeps one test
// that was already failing.
func testsRepo(t *testing.T) (dir, base string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	dir = t.TempDir()
	testsGit(t, dir, "init", "-q", "-b", "main")
	testsGit(t, dir, "config", "user.name", "Test")
	testsGit(t, dir, "config", "user.email", "test@example.com")
	writeFiles(t, dir, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.21\n",
		"calc/calc.go": `package calc

func Add(a, b int) int {
	return a + b
}

func Mul(a, b int) int {
	return a * b
}
`,
		"calc/calc_test.go": `package calc

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 2) != 4 {
		t.Fatal("Add(2, 2) != 4")
	}
}

func TestMul(t *testing.T) {
	if Mul(2, 3) != 6 {
		t.Fatal("Mul")
	}
}

func TestOld(t *testing.T) {
	t.Fatal("broken before")
}
`,
	})
	testsGit(t, dir, "add", "-A")
	testsGit(t, dir, "commit", "-q", "-m", "base")
	base = testsGit(t, dir, "rev-parse", "HEAD")
	writeFiles(t, dir, map[string]string{
		"calc/calc.go": `package calc

func Add(a, b int) int {
	return a - b
}

func Mul(a, b int) int {
	n := a * b
	return n
}
`,
		"calc/calc_test.go": `package calc

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 2) != 4 {
		t.Fatal("Add(2, 2) != 4")
	}
}

func TestMul(t *testing.T) {
	if Mul(2, 3) != 6 {
		t.Fatal("Mul")
	}
}

func TestOld(t *testing.T) {
	t.Fatal("broken before")
}

func TestNew(t *testing.T) {
	t.Fatal("new and failing")
}
`,
	})
	testsGit(t, dir, "add", "-A")
	testsGit(t, dir, "commit", "-q", "-m", "head")
	return dir, base
}

func TestTesterGoFailuresAndCoverage(t *testing.T) {
	dir, base := testsRepo(t)
	src, err := FromGit(dir, base, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	src.Base = base
	units := BuildUnits(src.Files, src.Content, 0)
	tr := &Tester{Policy: TestPolicy{Enabled: true}}
	run := tr.Run(context.Background(), dir, base, src)
	if !run.Ran || run.Passed != 1 || run.Failed != 3 {
		t.Fatalf("run: %+v", run)
	}
	byTest := map[string]TestFailure{}
	for _, f := range run.Failures {
		byTest[f.Test] = f
	}
	if !byTest["TestOld"].PreExisting || byTest["TestAdd"].PreExisting || byTest["TestNew"].PreExisting {
		t.Fatalf("pre-existing: %+v", run.Failures)
	}
	if run.Regressions != 2 || byTest["TestAdd"].Package != "calc" {
		t.Fatalf("regressions: %+v", run)
	}
	if !strings.Contains(run.Summary(), "2 broken by this change") {
		t.Fatal(run.Summary())
	}

	for _, u := range units {
		u.Score = &Score{Prior: 50, Kind: 1}
	}
	tr.Apply(units, DefaultTierPolicy())
	byID := map[string]*Unit{}
	for _, u := range units {
		byID[u.ID] = u
	}
	add, mul := byID["calc/calc.go:Add"], byID["calc/calc.go:Mul"]
	newTest := byID["calc/calc_test.go:TestNew"]
	if add == nil || mul == nil || newTest == nil {
		t.Fatalf("units: %v", byID)
	}
	// TestNew is a changed test: its failure is on it. TestAdd's isn't
	// changed, so its failure goes on the package's changed code.
	if !hasTestIssue(newTest, "TestNew", "high") || !hasTestIssue(add, "TestAdd", "high") {
		t.Fatalf("issues: new %+v add %+v", newTest.Issues, add.Issues)
	}
	if add.Score.Pin != BucketHuman || !strings.Contains(add.Score.PinWhy, "a test fails") {
		t.Fatalf("pin: %+v", add.Score)
	}
	// The package fails, so its coverage is not evidence.
	if mul.Coverage != nil {
		t.Fatalf("coverage in a failing package: %+v", mul.Coverage)
	}
}

func hasTestIssue(u *Unit, test, sev string) bool {
	for _, is := range u.Issues {
		if is.Source == TestIssueSource && strings.Contains(is.Title, test) && is.Severity == sev {
			return true
		}
	}
	return false
}

func TestTesterCoverageIsEvidence(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	dir := t.TempDir()
	testsGit(t, dir, "init", "-q", "-b", "main")
	testsGit(t, dir, "config", "user.name", "Test")
	testsGit(t, dir, "config", "user.email", "test@example.com")
	writeFiles(t, dir, map[string]string{
		"go.mod":        "module example.com/m\n\ngo 1.21\n",
		"lib/lib.go":    "package lib\n\nfunc Twice(n int) int {\n\treturn n * 2\n}\n",
		"lib/x_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestTwice(t *testing.T) {\n\tif Twice(2) != 4 {\n\t\tt.Fatal()\n\t}\n}\n",
	})
	testsGit(t, dir, "add", "-A")
	testsGit(t, dir, "commit", "-q", "-m", "base")
	base := testsGit(t, dir, "rev-parse", "HEAD")
	writeFiles(t, dir, map[string]string{
		"lib/lib.go": "package lib\n\nfunc Twice(n int) int {\n\tm := n + n\n\treturn m\n}\n\nfunc Unused(n int) int {\n\treturn n\n}\n",
	})
	testsGit(t, dir, "commit", "-q", "-am", "head")
	src, err := FromGit(dir, base, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	units := BuildUnits(src.Files, src.Content, 0)
	tr := &Tester{Policy: TestPolicy{Enabled: true}}
	if run := tr.Run(context.Background(), dir, base, src); !run.OK() {
		t.Fatalf("run: %+v", run)
	}
	tp := DefaultTierPolicy()
	for _, u := range units {
		u.Reviewed = true
		u.Score = &Score{Base: 50, Prior: 50, Kind: 1, Clean: 1}
	}
	tr.Apply(units, tp)
	for _, u := range units {
		switch u.Symbol {
		case "Twice":
			if u.Coverage == nil || u.Coverage.Covered == 0 || u.Coverage.Covered != u.Coverage.Statements {
				t.Fatalf("Twice coverage: %+v", u.Coverage)
			}
			if u.Score.Evidence != 0.5 || !strings.Contains(u.Score.Why, "passing tests run") {
				t.Fatalf("Twice score: %+v", u.Score)
			}
		case "Unused":
			if u.Coverage == nil || u.Coverage.Covered != 0 || u.Score.Evidence != 0 {
				t.Fatalf("Unused: %+v %+v", u.Coverage, u.Score)
			}
		}
	}
	// Evidence takes more of the prior than the clean review alone.
	b, _, _ := tp.Budget("")
	plain := Score{Prior: 50, Clean: 1}
	withEv := Score{Prior: 50, Clean: 1, Evidence: 0.5}
	if withEv.TotalAt(b, 0) >= plain.TotalAt(b, 0) {
		t.Fatalf("evidence: %d vs %d", withEv.TotalAt(b, 0), plain.TotalAt(b, 0))
	}
	// Without a clean review it counts for nothing.
	unclean := Score{Prior: 50, Evidence: 0.5}
	if unclean.TotalAt(b, 0) != 50 {
		t.Fatal(unclean.TotalAt(b, 0))
	}
}

func TestTesterCommandRegression(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	testsGit(t, dir, "init", "-q", "-b", "main")
	testsGit(t, dir, "config", "user.name", "Test")
	testsGit(t, dir, "config", "user.email", "test@example.com")
	writeFiles(t, dir, map[string]string{"ok": "yes\n"})
	testsGit(t, dir, "add", "-A")
	testsGit(t, dir, "commit", "-q", "-m", "base")
	base := testsGit(t, dir, "rev-parse", "HEAD")
	writeFiles(t, dir, map[string]string{"ok": "no\n"})
	testsGit(t, dir, "commit", "-q", "-am", "head")
	src := &Source{}
	tr := &Tester{Policy: TestPolicy{Enabled: true, Command: `grep -q yes ok || { echo "ok is not yes"; exit 1; }`}}
	run := tr.Run(context.Background(), dir, base, src)
	if !run.Ran || run.Failed != 1 || run.Regressions != 1 || !strings.Contains(run.Output, "ok is not yes") {
		t.Fatalf("run: %+v", run)
	}
	// At a head that fails like the base, it isn't the change's.
	writeFiles(t, dir, map[string]string{"ok": "yes\n"})
	testsGit(t, dir, "commit", "-q", "-am", "fix")
	head := testsGit(t, dir, "rev-parse", "HEAD")
	tr.Policy.Command = "exit 1"
	if run := tr.Run(context.Background(), dir, head, src); run.Regressions != 0 || !run.Failures[0].PreExisting {
		t.Fatalf("pre-existing: %+v", run)
	}
}

func TestTesterTimeout(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	tr := &Tester{Policy: TestPolicy{Enabled: true, Command: "sleep 30", TimeoutSec: 1}}
	run := tr.Run(context.Background(), t.TempDir(), "", &Source{})
	if run.Ran || !strings.Contains(run.Skipped, "timed out") {
		t.Fatalf("run: %+v", run)
	}
}

func TestParseGoTestJSONSubtestsAndBuildFailure(t *testing.T) {
	out := strings.Join([]string{
		`{"Action":"run","Package":"m/a","Test":"TestX"}`,
		`{"Action":"output","Package":"m/a","Test":"TestX/sub","Output":"x_test.go:9: bad\n"}`,
		`{"Action":"fail","Package":"m/a","Test":"TestX/sub"}`,
		`{"Action":"fail","Package":"m/a","Test":"TestX"}`,
		`{"Action":"pass","Package":"m/a","Test":"TestY"}`,
		`{"Action":"fail","Package":"m/a"}`,
		`{"ImportPath":"m/b","Action":"build-output","Output":"b.go:3:1: syntax error\n"}`,
		`{"ImportPath":"m/b","Action":"build-fail"}`,
		`{"Action":"fail","Package":"m/b"}`,
	}, "\n")
	res := parseGoTestJSON([]byte(out), "m")
	if res.passed != 1 || len(res.failures) != 2 {
		t.Fatalf("%+v", res)
	}
	if f := res.failures[0]; f.Package != "a" || f.Test != "TestX/sub" || !strings.Contains(f.Output, "bad") {
		t.Fatalf("subtest: %+v", f)
	}
	if f := res.failures[1]; f.Package != "b" || f.Test != "" || !strings.Contains(f.Output, "syntax error") {
		t.Fatalf("build: %+v", f)
	}
}
