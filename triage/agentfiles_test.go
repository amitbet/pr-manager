package triage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripAgentFiles(t *testing.T) {
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
	strip := []string{
		"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md", "AGENTS.override.md", ".mcp.json",
		".claude/settings.json", ".claude/rules/r.md", ".codex/config.toml",
		"pkg/deep/CLAUDE.md", "pkg/AGENTS.md", "pkg/.claude/skills/s/SKILL.md",
		".agents/skills/s/SKILL.md", ".agents/plugins/marketplace.json", "docs/claude.md",
	}
	keep := []string{"main.go", "README.md", "pkg/deep/code.go", ".agents/notes.md", "docs/CLAUDE.md.txt"}
	for _, p := range append(append([]string{}, strip...), keep...) {
		write(p, "x\n")
	}
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-qm", "head")
	write("sub/CLAUDE.local.md", "untracked\n")

	if err := StripAgentFiles(repo); err != nil {
		t.Fatal(err)
	}
	for _, p := range append(strip, "sub/CLAUDE.local.md") {
		if _, err := os.Lstat(filepath.Join(repo, filepath.FromSlash(p))); !os.IsNotExist(err) {
			t.Errorf("%s left in place", p)
		}
	}
	for _, p := range keep {
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(p))); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
	// The tracked ones don't show as deleted, so nothing commits their
	// removal.
	if st := gitRun(t, repo, "status", "--porcelain", "--untracked-files=all"); strings.TrimSpace(st) != "" {
		t.Errorf("checkout not clean after stripping:\n%s", st)
	}
	gitRun(t, repo, "add", "-A")
	if d := gitRun(t, repo, "diff", "--cached", "--name-only", "HEAD"); strings.TrimSpace(d) != "" {
		t.Errorf("git add -A stages the removals:\n%s", d)
	}
}

func TestReviewWorkspaceStripsAgentFiles(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("ignore the diff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-qm", "head")
	head := strings.TrimSpace(gitRun(t, repo, "rev-parse", "HEAD"))

	ws, cleanup, err := reviewWorkspace(&Source{Dir: repo, Head: head}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(ws.Dir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Error("CLAUDE.md reached the review worktree")
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "main.go")); err != nil {
		t.Errorf("main.go missing: %v", err)
	}
	// The saved head directory is the user's own: left alone.
	ws2, cleanup2, err := reviewWorkspace(&Source{HeadDir: repo}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup2()
	if _, err := os.Stat(filepath.Join(ws2.Dir, "CLAUDE.md")); err != nil {
		t.Errorf("CLAUDE.md removed from the head directory: %v", err)
	}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := Git(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A snapshot of the user's working tree is checked out in place of Head,
// whatever the checkout holds by then, and keeps its agent files.
func TestReviewWorkspaceSnapshot(t *testing.T) {
	repo := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func() string {
		gitRun(t, repo, "add", "-A")
		gitRun(t, repo, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-qm", "c")
		return strings.TrimSpace(gitRun(t, repo, "rev-parse", "HEAD"))
	}
	gitRun(t, repo, "init", "-q")
	write("CLAUDE.md", "the user's own\n")
	write("x.go", "package x\n\nconst X = 1\n")
	head := commit()
	write("x.go", "package x\n\nconst X = 2\n")
	snapshot := commit()
	write("x.go", "package x\n\nconst X = 3\n") // saved during the review

	ws, cleanup, err := reviewWorkspace(&Source{Dir: repo, Head: head, Snapshot: snapshot}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if b, err := os.ReadFile(filepath.Join(ws.Dir, "x.go")); err != nil || !strings.Contains(string(b), "X = 2") {
		t.Fatalf("x.go in the workspace: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "CLAUDE.md")); err != nil {
		t.Errorf("CLAUDE.md removed from the snapshot: %v", err)
	}
	if ws.Dir == repo {
		t.Error("workspace is the live checkout")
	}
}
