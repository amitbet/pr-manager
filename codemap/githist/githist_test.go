package githist

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSummarize(t *testing.T) {
	ref := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return ref.AddDate(0, 0, -n) }
	ch := func(s byte, ps ...string) []Change {
		var out []Change
		for _, p := range ps {
			out = append(out, Change{Status: s, Path: p})
		}
		return out
	}
	bulk := make([]string, 50)
	for i := range bulk {
		bulk[i] = filepath.Join("x", string(rune('a'+i%26)), "f.go")
	}
	commits := []Commit{
		{Time: day(300), Email: "ann@x", Subject: "add sync", Files: ch('A', "sync.go", "sync_test.go")},
		{Time: day(200), Email: "bob@x", Subject: "fix: retry on 503", Files: ch('M', "sync.go", "sync_test.go")},
		{Time: day(10), Email: "ann@x", Subject: "Fix panic in sync loop", Files: ch('M', "sync.go", "go.sum")},
		{Time: day(5), Email: "cy@x", Subject: "fix typo in comment", Files: ch('M', "sync.go")},
		{Time: day(3), Email: "bob@x", Subject: `Revert "speed up sync"`, Files: ch('M', "sync.go", "sync_test.go")},
		{Time: day(2), Email: "ann@x", Subject: "gofmt everything", Files: ch('M', append([]string{"sync.go"}, bulk...)...)},
		{Time: day(400), Email: "old@x", Subject: "fix ancient bug", Files: ch('M', "sync.go")},
	}
	st := Summarize(commits, ref, DefaultConfig())
	fs := st.Files["sync.go"]
	if fs == nil {
		t.Fatal("no stats for sync.go")
	}
	// 6 commits in the window; the bulk one does not count as churn.
	if fs.Commits != 5 || fs.Fixes != 2 || fs.Reverts != 1 || len(fs.Authors) != 3 || fs.Created != "ann@x" {
		t.Errorf("sync.go = %+v", fs)
	}
	if fs.RecentFixes < 1.40 || fs.RecentFixes > 1.45 { // 0.46 (200d) + 0.96 (10d)
		t.Errorf("recent fixes = %.2f", fs.RecentFixes)
	}
	if st.AgeDays(fs) != 2 {
		t.Errorf("age = %d", st.AgeDays(fs))
	}
	if _, ok := st.Files["go.sum"]; ok {
		t.Error("lockfiles are skipped")
	}
	ps := st.Partners("sync.go", 3, 0.3, 5)
	if len(ps) != 1 || ps[0].Path != "sync_test.go" || ps[0].N != 3 || ps[0].Conf != 0.6 {
		t.Errorf("partners = %+v", ps)
	}
}

func TestLog(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=A", "GIT_AUTHOR_EMAIL=A@X.io", "GIT_COMMITTER_NAME=A", "GIT_COMMITTER_EMAIL=a@x.io")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "add a")
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\n// x\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package a\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "fix: b")
	cs, err := Log(dir, "HEAD", time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Subject != "fix: b" || cs[0].Email != "a@x.io" || len(cs[0].Files) != 2 || cs[1].Files[0] != (Change{'A', "a.go"}) {
		t.Fatalf("log = %+v", cs)
	}
}

// A deleted and re-added file starts its co-changes over: confidence is
// never above 1.
func TestSummarizeDeleteDropsPairs(t *testing.T) {
	ref := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return ref.AddDate(0, 0, -n) }
	commits := []Commit{
		{Time: day(9), Email: "a@x", Subject: "one", Files: []Change{{'A', "a.go"}, {'A', "b.go"}}},
		{Time: day(8), Email: "a@x", Subject: "two", Files: []Change{{'M', "a.go"}, {'M', "b.go"}}},
		{Time: day(7), Email: "a@x", Subject: "three", Files: []Change{{'M', "a.go"}, {'M', "b.go"}}},
		{Time: day(6), Email: "a@x", Subject: "drop a", Files: []Change{{'D', "a.go"}, {'M', "b.go"}}},
		{Time: day(5), Email: "a@x", Subject: "readd a", Files: []Change{{'A', "a.go"}, {'M', "b.go"}}},
	}
	st := Summarize(commits, ref, DefaultConfig())
	if n := st.Pairs["a.go"]["b.go"]; n != 1 {
		t.Errorf("pairs a->b = %d, want 1", n)
	}
	if n := st.Pairs["b.go"]["a.go"]; n != 1 {
		t.Errorf("pairs b->a = %d, want 1", n)
	}
	for _, p := range st.Partners("a.go", 1, 0, 5) {
		if p.Conf > 1 {
			t.Errorf("partner %+v: confidence above 1", p)
		}
	}
}
