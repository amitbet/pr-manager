package triage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A snapshot's worktree sees the checkout's ignored node_modules, at the
// root and in a workspace package, through links; a PR head's does not,
// and removing the worktree leaves the user's node_modules in place.
func TestReviewWorkspaceLinksNodeModules(t *testing.T) {
	repo := t.TempDir()
	write := func(p, s string) {
		t.Helper()
		full := filepath.Join(repo, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "init", "-q")
	write(".gitignore", "node_modules/\n")
	write("index.js", "require('foo')\n")
	write("packages/a/index.js", "require('bar')\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-qm", "c")
	head := strings.TrimSpace(gitRun(t, repo, "rev-parse", "HEAD"))
	write("node_modules/foo/index.js", "module.exports = 1\n")
	write("packages/a/node_modules/bar/index.js", "module.exports = 2\n")
	write("gone/node_modules/baz/index.js", "module.exports = 3\n") // no package in the snapshot

	ws, cleanup, err := reviewWorkspace(&Source{Dir: repo, Head: head, Snapshot: head}, func(s string) { t.Log(s) })
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"node_modules/foo/index.js", "packages/a/node_modules/bar/index.js"} {
		if _, err := os.Stat(filepath.Join(ws.Dir, filepath.FromSlash(p))); err != nil {
			t.Errorf("%s not seen in the snapshot: %v", p, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(ws.Dir, "gone")); err == nil {
		t.Error("created a directory the snapshot does not have")
	}
	cleanup()
	if _, err := os.Stat(ws.Dir); !os.IsNotExist(err) {
		t.Errorf("worktree left behind: %v", err)
	}
	for _, p := range []string{"node_modules/foo/index.js", "packages/a/node_modules/bar/index.js"} {
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(p))); err != nil {
			t.Errorf("removal reached the user's %s: %v", p, err)
		}
	}

	pr, cleanupPR, err := reviewWorkspace(&Source{Dir: repo, Head: head}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupPR()
	if _, err := os.Lstat(filepath.Join(pr.Dir, "node_modules")); err == nil {
		t.Error("PR head worktree got the checkout's node_modules")
	}
}
