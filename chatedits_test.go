package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

// The reader's own agent edits the conversation's worktree; its edits stay
// there while the head stays, are dropped once the head has them, carried
// to a head that doesn't, and saved when they don't apply.
func TestInstalledWorktreeKeepsEdits(t *testing.T) {
	ctx := context.Background()
	repo, git := gitRepo(t)
	write(t, repo, "a.go", "package a\n\nfunc f() int { return 0 }\n")
	write(t, repo, "c.go", "package a\n")
	write(t, repo, "CLAUDE.md", "follow me\n")
	git("add", ".")
	git("commit", "-qm", "base")
	head := strings.TrimSpace(git("rev-parse", "HEAD"))
	dir := filepath.Join(t.TempDir(), "conv")
	code := filepath.Join(dir, "code")
	if _, err := installedWorktree(ctx, repo, dir, head); err != nil {
		t.Fatal(err)
	}
	if err := triage.StripAgentFiles(code); err != nil {
		t.Fatal(err)
	}
	// The agent edits, adds a file and stages one of them.
	write(t, code, "a.go", "package a\n\nfunc f() int { return 1 }\n")
	write(t, code, "b.go", "package a\n")
	gitIn(t, code, "add", "a.go")
	if _, err := installedWorktree(ctx, repo, dir, head); err != nil {
		t.Fatal(err)
	}
	patch, err := editsPatch(ctx, code, head)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(patch, "return 1") || !strings.Contains(patch, "b/b.go") || strings.Contains(patch, "CLAUDE.md") {
		t.Fatalf("edits patch:\n%s", patch)
	}
	if staged := strings.TrimSpace(gitIn(t, code, "diff", "--cached", "--name-only")); staged != "a.go" {
		t.Errorf("the agent's staging changed: %q", staged)
	}

	// A head with an unrelated commit: the edits come along.
	write(t, repo, "c.go", "package a\n\n// more\n")
	git("commit", "-qam", "other")
	moved := strings.TrimSpace(git("rev-parse", "HEAD"))
	note, err := installedWorktree(ctx, repo, dir, moved)
	if err != nil || !strings.Contains(note, "carried over") {
		t.Fatalf("moved: %q, %v", note, err)
	}
	if b, _ := os.ReadFile(filepath.Join(code, "a.go")); !strings.Contains(string(b), "return 1") {
		t.Errorf("edit lost: %s", b)
	}

	// A head that has them (committed as a change): a clean worktree.
	if err := gitApply(ctx, repo, patch); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "the change")
	committed := strings.TrimSpace(git("rev-parse", "HEAD"))
	if note, err := installedWorktree(ctx, repo, dir, committed); err != nil || note != "" {
		t.Fatalf("committed: %q, %v", note, err)
	}
	if st := gitIn(t, code, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Errorf("edits applied twice: %s", st)
	}
	if readBase(dir) != committed {
		t.Errorf("base %q, want %q", readBase(dir), committed)
	}

	// Edits that clash with the new head are saved, not lost.
	write(t, code, "a.go", "package a\n\nfunc f() int { return 2 }\n")
	write(t, repo, "a.go", "package a\n\nfunc f() int { return 3 }\n")
	git("commit", "-qam", "clash")
	clash := strings.TrimSpace(git("rev-parse", "HEAD"))
	note, err = installedWorktree(ctx, repo, dir, clash)
	if err != nil || !strings.Contains(note, "saved in") {
		t.Fatalf("clash: %q, %v", note, err)
	}
	saved, _ := filepath.Glob(filepath.Join(dir, "edits-*.patch"))
	if len(saved) != 1 {
		t.Fatalf("saved %q", saved)
	}
	if b, _ := os.ReadFile(saved[0]); !strings.Contains(string(b), "return 2") {
		t.Errorf("saved patch: %s", b)
	}
}

// The read-only agent checks the worktree out again, after saving what
// the reader's agent left in it.
func TestKeepEdits(t *testing.T) {
	ctx := context.Background()
	repo, git := gitRepo(t)
	write(t, repo, "a.go", "package a\n")
	git("add", ".")
	git("commit", "-qm", "base")
	head := strings.TrimSpace(git("rev-parse", "HEAD"))
	dir := filepath.Join(t.TempDir(), "conv")
	if note := keepEdits(ctx, dir); note != "" {
		t.Errorf("no edits: %q", note)
	}
	if _, err := installedWorktree(ctx, repo, dir, head); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "code"), "a.go", "package b\n")
	if note := keepEdits(ctx, dir); !strings.Contains(note, "saved in") {
		t.Errorf("note %q", note)
	}
	if readBase(dir) != "" {
		t.Error("base kept: the next Installed answer would carry the edits again")
	}
}

// commit_edits takes the edits and their files, and the first round of
// the change applies them as its patch.
func TestCommitEdits(t *testing.T) {
	ctx := context.Background()
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	repo, git := gitRepo(t)
	write(t, repo, "a.go", "package a\n\nfunc f() int { return 0 }\n")
	git("add", ".")
	git("commit", "-qm", "base")
	head := strings.TrimSpace(git("rev-parse", "HEAD"))
	if _, _, err := tr.chatEdits(ctx, "github.com/o/r#1"); err == nil {
		t.Error("edits without a worktree")
	}
	dir := filepath.Join(tr.chatsDir(), convHash("github.com/o/r#1"))
	if _, err := installedWorktree(ctx, repo, dir, head); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.chatEdits(ctx, "github.com/o/r#1"); err == nil || !strings.Contains(err.Error(), "no edits") {
		t.Errorf("no edits: %v", err)
	}
	write(t, filepath.Join(dir, "code"), "a.go", "package a\n\nfunc f() int { return 1 }\n")
	write(t, filepath.Join(dir, "code"), "b.go", "package a\n")
	patch, files, err := tr.chatEdits(ctx, "github.com/o/r#1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(files, ",") != "a.go,b.go" {
		t.Errorf("files %q", files)
	}
	c := &fixChange{Instructions: "Return one", Files: files, patch: patch}
	if err := c.check(); err != nil {
		t.Fatal(err)
	}

	// The fix checkout, at the same commit.
	fixDir := filepath.Join(t.TempDir(), "fix")
	git("worktree", "add", "-q", "--detach", fixDir, head)
	changed, _, err := fixRound(ctx, options{}, fixDir, nil, []targetedIssue{changeTarget(c)}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 || c.patch != "" {
		t.Errorf("changed %d files, patch left %v", len(changed), c.patch != "")
	}
	if b, _ := os.ReadFile(filepath.Join(fixDir, "a.go")); !strings.Contains(string(b), "return 1") {
		t.Errorf("not applied: %s", b)
	}
}

func TestChatActionsDocByAgent(t *testing.T) {
	ro, own := chatActionsDoc(false), chatActionsDoc(true)
	if !strings.Contains(ro, "- change ") || strings.Contains(ro, "commit_edits") {
		t.Errorf("read-only actions:\n%s", ro)
	}
	if strings.Contains(own, "- change ") || !strings.Contains(own, "- commit_edits ") {
		t.Errorf("installed actions:\n%s", own)
	}
	found := false
	for _, n := range chatActionNames() {
		found = found || n == "commit_edits"
	}
	if !found {
		t.Error("parseActions would drop commit_edits")
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := triage.GitCtx(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

// Switching between the read-only agent and the reader's own starts a new
// session: tools and sandbox are fixed per session.
func TestChatSessionByAgent(t *testing.T) {
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	fake := &chatLLM{}
	ws := &llm.Workspace{Dir: "/w", Installed: true, Session: &llm.Session{ID: "s1"}}
	first := []chatTurn{{Role: "user", Content: "edit it", At: "2026-10-07T10:00:00Z"}}
	cv := tr.openChat(chatRequest{Conversation: "c", Messages: first})
	cv.saveSession(fake, ws)
	cv.close()
	next := append(append([]chatTurn{}, first...), chatTurn{Role: "assistant", At: cv.at}, chatTurn{Role: "user", Content: "more", At: "2026-10-07T10:01:00Z"})
	cv = tr.openChat(chatRequest{Conversation: "c", Messages: next})
	defer cv.close()
	if cv.resumable(fake, ws) == nil {
		t.Error("the same agent didn't carry on")
	}
	if cv.resumable(fake, &llm.Workspace{Dir: "/w"}) != nil {
		t.Error("the read-only agent carried on the reader's agent's session")
	}
}
