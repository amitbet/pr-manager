package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/triage"
)

func TestApplyFixPatchAndSelectChangedUnit(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("package a\n\nfunc f() int { return 0 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -3 +3 @@\n-func f() int { return 0 }\n+func f() int { return 1 }\n"
	changed, err := applyFixPatch(context.Background(), dir, patch)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil || strings.ReplaceAll(string(content), "\r\n", "\n") != "package a\n\nfunc f() int { return 1 }\n" {
		t.Fatalf("file = %q, %v", content, err)
	}
	near := &triage.Unit{File: "a.go", Hunks: []triage.Hunk{{NewStart: 1, NewLines: 5}}}
	far := &triage.Unit{File: "a.go", Hunks: []triage.Hunk{{NewStart: 20, NewLines: 2}}}
	if !touchesPatch(near, changed) || touchesPatch(far, changed) {
		t.Errorf("patch hunk selection: near=%v far=%v", touchesPatch(near, changed), touchesPatch(far, changed))
	}
}

func TestFixTargets(t *testing.T) {
	r := &PRResult{Files: []resultFile{
		{FileDiff: triage.FileDiff{Path: "a.go"}, Units: []resultUnit{
			{Unit: &triage.Unit{ID: "a.go:f", Issues: []triage.Issue{{Title: "one"}, {Title: "two"}}}},
		}},
	}}
	one := fixTargets(r, fixRequest{UnitID: "a.go:f", Issue: 1})
	all := fixTargets(r, fixRequest{All: true})
	if len(one) != 1 || one[0].Issue.Title != "two" || len(all) != 2 {
		t.Errorf("one=%+v all=%+v", one, all)
	}
	if safeRepoPath("a/../../outside") {
		t.Error("accepted a path outside the repository")
	}
}

func TestFixable(t *testing.T) {
	pr := func(p triage.PRInfo) *PRResult { return &PRResult{PR: &p} }
	for _, c := range []struct {
		name string
		r    *PRResult
		loc  string
		ok   bool
	}{
		{"open PR", pr(triage.PRInfo{State: "OPEN"}), "", true},
		{"merged PR", pr(triage.PRInfo{State: "MERGED"}), "", false},
		{"closed PR", pr(triage.PRInfo{State: "CLOSED"}), "", false},
		{"local repo", pr(triage.PRInfo{State: "LOCAL", LocalPath: "/r"}), "worktree", true},
		{"local with a PR", pr(triage.PRInfo{State: "MERGED", LocalPath: "/r"}), "", true},
		{"local uncommitted", pr(triage.PRInfo{State: "LOCAL", LocalPath: "/r", Uncommitted: true}), "", false},
		{"local in the clone", pr(triage.PRInfo{State: "LOCAL", LocalPath: "/r"}), "clone", false},
	} {
		if err := fixable(c.r, c.loc); (err == nil) != c.ok {
			t.Errorf("%s: err=%v", c.name, err)
		}
	}
}

func TestCheckoutFixBranchAtPRHead(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.go")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
	head := git("rev-parse", "HEAD")
	pr := &triage.PRInfo{PRRef: triage.PRRef{Number: 7}, HeadRef: "feature/fix", HeadOid: head}
	first := filepath.Join(t.TempDir(), "first")
	branch, err := checkoutFixBranch(repo, first, pr, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "feature/fix" || strings.TrimSpace(git("-C", first, "branch", "--show-current")) != branch {
		t.Errorf("first worktree branch = %q", branch)
	}
	if got := strings.TrimSpace(git("-C", first, "rev-parse", "HEAD")); got != head {
		t.Errorf("head = %s, want %s", got, head)
	}
	second := filepath.Join(t.TempDir(), "second")
	branch, err = checkoutFixBranch(repo, second, pr, "def456")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "pr-manager/pr-7-def456" || strings.TrimSpace(git("-C", second, "branch", "--show-current")) != branch {
		t.Errorf("second worktree branch = %q", branch)
	}
}

func TestCheckoutCachedClonePreservesExistingChanges(t *testing.T) {
	root := t.TempDir()
	src, clone := filepath.Join(root, "source"), filepath.Join(root, "clone")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", src)
	if err := os.WriteFile(filepath.Join(src, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("-C", src, "add", "a.go")
	run("-C", src, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
	head := run("-C", src, "rev-parse", "HEAD")
	run("clone", "-q", "--no-checkout", src, clone)
	empty, err := emptyCloneCheckout(clone)
	if err != nil || !empty {
		t.Fatalf("no-checkout clone empty=%v err=%v", empty, err)
	}
	pr := &triage.PRInfo{PRRef: triage.PRRef{Number: 4}, HeadRef: "feature/fix", HeadOid: head}
	branch, err := checkoutCloneBranch(clone, pr, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "feature/fix" || run("-C", clone, "branch", "--show-current") != branch || run("-C", clone, "rev-parse", "HEAD") != head {
		t.Errorf("branch=%q head=%s", branch, run("-C", clone, "rev-parse", "HEAD"))
	}
	if err := os.WriteFile(filepath.Join(clone, "a.go"), []byte("package changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := checkoutCloneBranch(clone, pr, "def456"); err == nil {
		t.Error("dirty cached clone was overwritten")
	}
	content, err := os.ReadFile(filepath.Join(clone, "a.go"))
	if err != nil || string(content) != "package changed\n" {
		t.Errorf("local content = %q, %v", content, err)
	}
}
