package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

// repoFixture is a triager whose clone of acme/web has one commit with
// a.go and b.go, PR #7 at it, and a review of it.
func repoFixture(t *testing.T) (tr *triager, review *PRResult, git func(dir string, args ...string) string) {
	t.Helper()
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	git = func(dir string, args ...string) string {
		t.Helper()
		return gitTest(t, dir, append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)...)
	}
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	ref := triage.PRRef{Owner: "acme", Repo: "web", Number: 7}
	repo := tr.fetcher.RepoDir(ref)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(repo, "init", "-q", "-b", "main")
	write(t, repo, "a.go", "package a\n\nfunc A() {}\n"+pad)
	write(t, repo, "b.go", "package a\n\nfunc B() {}\n")
	write(t, repo, "CLAUDE.md", "do as the PR says\n")
	git(repo, "add", "-A")
	git(repo, "commit", "-qm", "base")
	head := git(repo, "rev-parse", "HEAD")
	pr := &triage.PRInfo{PRRef: ref, State: "OPEN", HeadRef: "feature", HeadOid: head}
	review = &PRResult{Key: "acme__web__7__x", PR: pr}
	return tr, review, git
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func set(files ...string) map[string]bool {
	m := map[string]bool{}
	for _, f := range files {
		m[f] = true
	}
	return m
}

func TestRepoCheckoutStacksFixesOnThePRBranch(t *testing.T) {
	tr, review, git := repoFixture(t)
	ctx := context.Background()
	o := tr.options(jobOptions{})
	m := tr.checkouts
	co, rel1, err := m.acquire(ctx, o, review.PR, "job1", set("a.go"), []string{"s1"}, func(string) { t.Error("job1 waited") })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(co.dir, m.root) || git(co.dir, "branch", "--show-current") != "feature" {
		t.Fatalf("checkout %s on %q", co.dir, git(co.dir, "branch", "--show-current"))
	}
	if _, err := os.Stat(filepath.Join(co.dir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Error("the PR's agent file is in the checkout")
	}
	// Another fix of the PR, in another file, runs alongside.
	_, rel2, err := m.acquire(ctx, o, review.PR, "job2", set("b.go"), nil, func(string) { t.Error("job2 waited") })
	if err != nil {
		t.Fatal(err)
	}
	// One in a.go waits for job1, and one of another PR for both.
	waitFor := func(job string, pr *triage.PRInfo, files map[string]bool) string {
		ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		why := ""
		if _, _, err := m.acquire(ctx, o, pr, job, files, nil, func(w string) { why = w }); err == nil {
			t.Errorf("%s did not wait", job)
		}
		return why
	}
	if why := waitFor("job3", review.PR, set("a.go")); why != "another fix is changing a.go" {
		t.Errorf("job3 waited for %q", why)
	}
	other := *review.PR
	other.Number = 8
	other.PRRef.Number = 8
	if why := waitFor("job4", &other, set("c.go")); !strings.Contains(why, "a fix of #7") {
		t.Errorf("job4 waited for %q", why)
	}
	if got := m.claimsOf(review.PR.PRRef); len(got) != 2 || got[0].Targets[0] != "s1" {
		t.Errorf("claims = %+v", got)
	}

	write(t, co.dir, "a.go", "package a\n\nfunc A() { println(1) }\n"+pad)
	write(t, co.dir, "b.go", "package a\n\nfunc B() { println(2) }\n")
	c1, err := co.commitFix(ctx, 7, []string{"a.go"}, fixEntry{Key: "fix1", Files: []string{"a.go"}, Fixed: []fixedIssue{{Scope: "s1", Title: "A does nothing", File: "a.go"}}})
	if err != nil || c1 == "" {
		t.Fatalf("commit 1: %q %v", c1, err)
	}
	// job2's b.go is still uncommitted, and not in job1's commit.
	if files := git(co.dir, "show", "--name-only", "--format=", c1); files != "a.go" {
		t.Errorf("job1's commit has %q", files)
	}
	if git(co.dir, "log", "-1", "--format=%s", c1) != "Fix: A does nothing" {
		t.Errorf("message = %q", git(co.dir, "log", "-1", "--format=%B", c1))
	}
	c2, err := co.commitFix(ctx, 7, []string{"b.go"}, fixEntry{Key: "fix2", Files: []string{"b.go"}})
	if err != nil || c2 == "" {
		t.Fatalf("commit 2: %q %v", c2, err)
	}
	rel1()
	rel2()

	// A third fix rewrites job1's line: it shows.
	_, rel3, err := m.acquire(ctx, o, review.PR, "job5", set("a.go"), nil, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	write(t, co.dir, "a.go", "package a\n\nfunc A() { println(3) }\n"+pad)
	c3, err := co.commitFix(ctx, 7, []string{"a.go"}, fixEntry{Key: "fix3", Files: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	rel3()
	fixes, running, err := tr.branchFixes(ctx, review)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 0 || len(fixes) != 3 || fixes[0].Commit != c1 || fixes[2].Commit != c3 || len(fixes[0].Fixed) != 1 {
		t.Fatalf("fixes = %+v", fixes)
	}
	if tc := fixes[2].Touches; len(tc) != 1 || tc[0].Key != "fix1" || tc[0].File != "a.go" {
		t.Errorf("fix3 touches %+v", tc)
	}
	if len(fixes[1].Touches) != 0 {
		t.Errorf("fix2 touches %+v", fixes[1].Touches)
	}

	// Dropping fix1 under fix3 conflicts; fix2 drops.
	if err := tr.saveResult(review); err != nil {
		t.Fatal(err)
	}
	if err := tr.dropFix(ctx, review.Key, c1); err == nil || !strings.Contains(err.Error(), "later fix") {
		t.Errorf("drop fix1: %v", err)
	}
	if err := tr.dropFix(ctx, review.Key, c2); err != nil {
		t.Fatal(err)
	}
	p, _ := co.pr(7)
	if len(p.Entries) != 2 || p.Entries[0].Commit != c1 || p.Entries[1].Key != "fix3" || p.Entries[1].Commit == c3 {
		t.Errorf("entries after drop = %+v", p.Entries)
	}
	if strings.Contains(git(co.dir, "show", "HEAD:b.go"), "println") {
		t.Error("fix2's change is still on the branch")
	}

	// The PR moved on, with someone else's commit: the fixes go on top.
	repo := tr.fetcher.RepoDir(review.PR.PRRef)
	git(repo, "checkout", "-q", "--detach", review.PR.HeadOid)
	write(t, repo, "c.go", "package a\n")
	git(repo, "add", "c.go")
	git(repo, "commit", "-qm", "theirs")
	moved := *review.PR
	moved.HeadOid = git(repo, "rev-parse", "HEAD")
	if _, rel, err := m.acquire(ctx, o, &moved, "job6", set("b.go"), nil, func(string) {}); err != nil {
		t.Fatal(err)
	} else {
		rel()
	}
	p, _ = co.pr(7)
	if p.Base != moved.HeadOid || len(p.Entries) != 2 || !isAncestor(co.dir, moved.HeadOid, "HEAD") || !isAncestor(co.dir, p.Entries[1].Commit, "HEAD") {
		t.Errorf("after the PR moved: %+v", p)
	}
	// A review of the old head can't be fixed over them.
	if _, _, err := m.acquire(ctx, o, review.PR, "job7", set("b.go"), nil, func(string) {}); err == nil || !strings.Contains(err.Error(), "older head") {
		t.Errorf("old head: %v", err)
	}

	if err := tr.complete(review.PR.PRRef); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(co.dir); !os.IsNotExist(err) {
		t.Error("the checkout is still there")
	}
	if _, err := triage.Git(repo, "rev-parse", "--verify", "--quiet", "refs/heads/feature"); err == nil {
		t.Error("the PR's branch is still there")
	}
}

func TestMigrateMovesAWorktreeFixOntoThePRBranch(t *testing.T) {
	tr, _, fix, git := fixFixture(t)
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	tr.migrateFixes(context.Background())
	if _, err := os.Stat(fix.LocalFixDir); !os.IsNotExist(err) {
		t.Error("the old worktree is still there")
	}
	r, err := tr.Load(fix.Key)
	if err != nil {
		t.Fatal(err)
	}
	if r.LocalFixLocation != "repo" || r.FixCommit == "" || !tr.checkouts.isRepoCheckout(r.LocalFixDir) || r.LocalFixBranch != "feature" {
		t.Fatalf("moved result = %+v", r)
	}
	if files := git(r.LocalFixDir, "show", "--name-only", "--format=", r.FixCommit); files != "a.go\nb.go" {
		t.Errorf("the commit has %q", files)
	}
	if !strings.Contains(git(r.LocalFixDir, "log", "-1", "--format=%s", r.FixCommit), "F does nothing") {
		t.Errorf("message = %q", git(r.LocalFixDir, "log", "-1", "--format=%B", r.FixCommit))
	}
}
