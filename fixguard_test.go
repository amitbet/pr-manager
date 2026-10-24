package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

func TestFixGuardBreakers(t *testing.T) {
	dir := t.TempDir()
	files := map[string]bool{"a.go": true}
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Oscillation: round 2 puts back what round 1 replaced.
	write("v0")
	g := newFixGuard(&PRResult{}, fixRequest{})
	g.initial(dir, files)
	write("v1")
	if why := g.afterRound(1, dir, files, 5, 1, 2); why != "" {
		t.Fatal(why)
	}
	write("v0")
	if why := g.afterRound(2, dir, files, 5, 1, 1); !strings.Contains(why, "before the fix") {
		t.Fatalf("oscillation: %q", why)
	}

	// Growth: past 3× round 1 and the floor.
	g = newFixGuard(&PRResult{}, fixRequest{})
	g.initial(dir, files) // the files as they were before round 1, as fixRounds records them
	write("g1")
	if why := g.afterRound(1, dir, files, 100, 1, 1); why != "" {
		t.Fatal(why)
	}
	write("g2")
	if why := g.afterRound(2, dir, files, 150, 1, 1); why != "" {
		t.Fatalf("under the limit: %q", why)
	}
	write("g3")
	if why := g.afterRound(3, dir, files, 100, 1, 1); !strings.Contains(why, "grew to 350") {
		t.Fatalf("growth: %q", why)
	}
	// Nothing left to do: growth doesn't stop a fix that is done.
	g = newFixGuard(&PRResult{}, fixRequest{})
	g.initial(dir, files)
	write("d1")
	g.afterRound(1, dir, files, 10, 1, 1)
	write("d2")
	if why := g.afterRound(2, dir, files, 500, 1, 0); why != "" {
		t.Fatalf("done: %q", why)
	}

	// No progress: two rounds in a row that fix nothing.
	g = newFixGuard(&PRResult{}, fixRequest{})
	g.initial(dir, files)
	for i, want := range []string{"", "", "two rounds in a row"} {
		write("s" + string(rune('a'+i)))
		why := g.afterRound(i+1, dir, files, 1, 0, 3)
		if !strings.Contains(why, want) || (want == "" && why != "") {
			t.Fatalf("round %d: %q", i+1, why)
		}
	}

	// Rounds across fixes, and the time budget.
	g = newFixGuard(&PRResult{FixRounds: 9}, fixRequest{})
	if why := g.beforeRound(1); why != "" {
		t.Fatal(why)
	}
	if why := g.beforeRound(2); !strings.Contains(why, "run 10 rounds") {
		t.Fatalf("total: %q", why)
	}
	if why := newFixGuard(&PRResult{FixRounds: 9}, fixRequest{MoreRounds: true}).beforeRound(5); why != "" {
		t.Fatalf("more rounds asked for: %q", why)
	}
	g = newFixGuard(&PRResult{}, fixRequest{})
	g.start = time.Now().Add(-fixTimeBudget - time.Minute)
	if why := g.beforeRound(1); why != "" {
		t.Fatalf("round 1 always runs: %q", why)
	}
	if why := g.beforeRound(2); !strings.Contains(why, "budget") {
		t.Fatalf("budget: %q", why)
	}
}

func TestSaveFilesRestore(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := saveFiles(dir, map[string]bool{"a.go": true, "new/b.go": true})
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new", "b.go"), []byte("added"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.restore(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a.go"))
	st, _ := os.Stat(filepath.Join(dir, "a.go"))
	// Windows keeps no Unix permissions: only read-only, which 0o600 isn't.
	if string(b) != "old" || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) {
		t.Fatalf("a.go: %q %v", b, st.Mode())
	}
	if _, err := os.Stat(filepath.Join(dir, "new", "b.go")); !os.IsNotExist(err) {
		t.Fatalf("added file kept: %v", err)
	}
}

func fixLinesOf(t *testing.T, patch string) *fixLines {
	t.Helper()
	diffs, err := triage.ParseDiff(patch)
	if err != nil {
		t.Fatal(err)
	}
	fl := newFixLines()
	fl.add(diffs)
	return fl
}

const fixLinesPatch = `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -10,2 +10,3 @@ func F() {
 	x := load()
+	if x == nil { return errMissing }
 	use(x)
`

func TestFixLinesOwns(t *testing.T) {
	fl := fixLinesOf(t, fixLinesPatch)
	for _, c := range []struct {
		is   triage.Issue
		file string
		own  bool
	}{
		{triage.Issue{Line: 11}, "a.go", true},
		{triage.Issue{Line: 12}, "a.go", false},
		{triage.Issue{Evidence: "if x == nil { return errMissing }"}, "a.go", true},
		{triage.Issue{Evidence: "use(x)"}, "a.go", false},
		{triage.Issue{Line: 11}, "b.go", false},
		{triage.Issue{}, "a.go", false},
	} {
		if got := fl.owns(c.file, c.is); got != c.own {
			t.Errorf("%s %+v: owns %v", c.file, c.is, got)
		}
	}
	var none *fixLines
	if !none.owns("a.go", triage.Issue{}) {
		t.Fatal("no lines to go by: every issue counts")
	}
}

func TestRoundTrackerLeavesIssuesInOldCode(t *testing.T) {
	fl := fixLinesOf(t, fixLinesPatch)
	at := func(title string, line int) targetedIssue {
		return targetedIssue{UnitID: "a.go:F", File: "a.go", Issue: triage.Issue{Title: title, Severity: "high", Line: line, Evidence: title}}
	}
	ours, old := at("in the fix", 11), at("in the old code", 30)
	rt := newRoundTracker()
	got := rt.next(nil, map[string]bool{"a.go:F": true}, map[string]bool{"a.go:F": true}, []targetedIssue{ours, old}, fl)
	if len(got) != 1 || got[0].Issue.Title != "in the fix" || rt.preexisting != 1 {
		t.Fatalf("next: %+v, %+v", got, rt)
	}
	if !strings.Contains(rt.warning(), "code the fix didn't write") {
		t.Fatal(rt.warning())
	}

	// worseThan: a new high on the fix's line, against one issue fixed.
	worked := at("worked on", 40)
	bad, fixed := worseThan(newRoundTracker(), []targetedIssue{worked}, map[string]bool{"a.go:F": true}, []targetedIssue{ours, old}, fl)
	if bad != 1 || fixed != 1 {
		t.Fatalf("worse: bad %d fixed %d", bad, fixed)
	}
}

func TestOwnFixCommits(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q", "-b", "main")
	gitTest(t, dir, "config", "user.name", "Test")
	gitTest(t, dir, "config", "user.email", "test@example.com")
	commit := func(msg string) {
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		gitTest(t, dir, "add", "-A")
		gitTest(t, dir, "commit", "-q", "-m", msg)
	}
	commit("mine")
	if n := ownFixCommits(dir, 2); n != 0 {
		t.Fatal(n)
	}
	commit(withFixTrailer("Fix a thing"))
	commit(withFixTrailer("Fix another"))
	if n := ownFixCommits(dir, 2); n != 2 {
		t.Fatal(n)
	}
	commit("a person's commit")
	if n := ownFixCommits(dir, 2); n != 0 {
		t.Fatal(n)
	}
	if m := withFixTrailer(withFixTrailer("x")); strings.Count(m, fixTrailer) != 1 {
		t.Fatal(m)
	}
}

func TestRoundTestsOnlyNewFailures(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/m\n\ngo 1.21\n")
	write("p/p.go", "package p\n\nfunc One() int { return 1 }\n")
	write("p/p_test.go", "package p\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) {\n\tif One() != 1 {\n\t\tt.Fatal(\"One\")\n\t}\n}\n\nfunc TestBroken(t *testing.T) { t.Fatal(\"was broken\") }\n")
	rt := newRoundTests(options{tests: "on"}, &PRResult{PR: &triage.PRInfo{LocalPath: dir}}, dir)
	if rt == nil {
		t.Fatal("no tests")
	}
	ctx := context.Background()
	files := map[string]bool{"p/p.go": true}
	rt.before(ctx, files)
	changed := []triage.FileDiff{{Path: "p/p.go"}}
	if broke := rt.after(ctx, changed); len(broke) != 0 {
		t.Fatalf("nothing changed, but: %+v", broke)
	}
	write("p/p.go", "package p\n\nfunc One() int { return 2 }\n")
	broke := rt.after(ctx, changed)
	if len(broke) != 1 || broke[0].Test != "TestOne" {
		t.Fatalf("broke: %+v", broke)
	}
	if n := testsNote(1, broke); !strings.Contains(n, "TestOne in ./p") || !strings.Contains(n, "undone") {
		t.Fatal(n)
	}
	// Tests off: no round tests.
	if newRoundTests(options{tests: "off"}, &PRResult{PR: &triage.PRInfo{LocalPath: dir}}, dir) != nil {
		t.Fatal("tests off")
	}
}
