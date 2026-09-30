package indexer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoFallbackHonoursGitignore(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	write := func(p, s string) {
		t.Helper()
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/sample\n")
	write(".gitignore", "gen/\n")
	write("keep.go", "package sample\nfunc Keep() {}\n")
	write("gen/skip.go", "package gen\nfunc Skip() {}\n")
	if out, err := exec.Command(gitPath, "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	// PATH with git but no go: packages.Load fails and the fallback runs.
	bin := t.TempDir()
	if err := os.Symlink(gitPath, filepath.Join(bin, "git")); err != nil {
		t.Skip("cannot symlink git:", err)
	}
	t.Setenv("PATH", bin)
	var g Graph
	mods := []Module{{Path: "example.com/sample", Dir: "."}}
	extractGo("sample", root, mods, mods, &g)
	var keys []string
	for _, n := range g.Nodes {
		keys = append(keys, n.Key)
	}
	all := strings.Join(keys, " ")
	if !strings.Contains(all, "sample:Keep") || strings.Contains(all, "Skip") {
		t.Errorf("nodes = %s", all)
	}
}
