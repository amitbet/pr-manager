package triage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A user's diff.noprefix, mnemonicPrefix and quotePath must not change
// the diff ParseDiff reads.
func TestGitDiffIgnoresUserConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=A", "GIT_AUTHOR_EMAIL=a@x", "GIT_COMMITTER_NAME=A", "GIT_COMMITTER_EMAIL=a@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	for _, kv := range [][2]string{{"diff.noprefix", "true"}, {"diff.mnemonicPrefix", "true"}, {"core.quotePath", "true"}, {"color.ui", "always"}} {
		git("config", kv[0], kv[1])
	}
	name := "café menu.go"
	os.WriteFile(filepath.Join(dir, name), []byte("package a\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "one")
	os.WriteFile(filepath.Join(dir, name), []byte("package a\n\nvar x = 1\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "tab\there.go"), []byte("package a\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "two")
	raw, err := GitDiff(dir, "HEAD~1", "HEAD", false)
	if err != nil {
		t.Fatal(err)
	}
	files, err := ParseDiff(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]FileStatus{}
	for _, f := range files {
		got[f.Path] = f.Status
	}
	if len(files) != 2 || got[name] != StatusModified || got["tab\there.go"] != StatusAdded {
		t.Fatalf("files = %+v\n%s", files, raw)
	}
}

func TestSplitDiffGitPathsQuoted(t *testing.T) {
	for _, c := range []struct{ in, a, b string }{
		{`a/x.go b/x.go`, "x.go", "x.go"},
		{`"a/caf\303\251.go" "b/caf\303\251.go"`, "café.go", "café.go"},
		{`"a/t\tq\".go" "b/t\tq\".go"`, "t\tq\".go", "t\tq\".go"},
		{`a/old.go "b/n\tew.go"`, "old.go", "n\tew.go"},
	} {
		if a, b := splitDiffGitPaths(c.in); a != c.a || b != c.b {
			t.Errorf("splitDiffGitPaths(%q) = %q, %q; want %q, %q", c.in, a, b, c.a, c.b)
		}
	}
	if got := unquotePath(`"r\303\251name.go"`); got != "réname.go" {
		t.Errorf("unquotePath = %q", got)
	}
}

// One line longer than any scanner buffer must not fail the diff.
func TestParseDiffLongLine(t *testing.T) {
	long := strings.Repeat("x", 20<<20)
	raw := "diff --git a/min.js b/min.js\n--- a/min.js\n+++ b/min.js\n@@ -1 +1 @@\n-" + long + "\n+" + long + "y\n" +
		"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-a\n+b\n"
	files, err := ParseDiff(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || len(files[0].Hunks) != 1 || len(files[0].Hunks[0].Lines) != 2 || files[1].Path != "b.go" {
		t.Fatalf("files = %d", len(files))
	}
}
