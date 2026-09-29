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

type countingClassifier struct{ calls int }

func (c *countingClassifier) Classify(context.Context, *triage.Unit) triage.Decision {
	c.calls++
	return triage.Decision{Bucket: triage.BucketHuman, Source: "llm"}
}

func TestCarryClassifierOnlyClassifiesTouchedUnits(t *testing.T) {
	changed := []triage.FileDiff{{Path: "a.go", Hunks: []triage.Hunk{{NewStart: 3, NewLines: 1}}}}
	kept := triage.Decision{Bucket: triage.BucketSkim, Source: "llm", Reason: "earlier"}
	prior := map[string]*triage.Unit{
		"a.go:f": {ID: "a.go:f", Decision: kept},
		"a.go:g": {ID: "a.go:g", Decision: kept},
	}
	inner := &countingClassifier{}
	c := &carryClassifier{inner, prior, changed}
	ctx := context.Background()
	if d := c.Classify(ctx, &triage.Unit{ID: "a.go:g", File: "a.go", Hunks: []triage.Hunk{{NewStart: 20, NewLines: 2}}}); d.Reason != "earlier" || inner.calls != 0 {
		t.Errorf("untouched unit: decision %+v, %d calls", d, inner.calls)
	}
	c.Classify(ctx, &triage.Unit{ID: "a.go:f", File: "a.go", Hunks: []triage.Hunk{{NewStart: 1, NewLines: 5}}})
	c.Classify(ctx, &triage.Unit{ID: "a.go:new", File: "a.go", Hunks: []triage.Hunk{{NewStart: 40, NewLines: 2}}})
	if inner.calls != 2 {
		t.Errorf("touched and new units: %d calls, want 2", inner.calls)
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
		{"local uncommitted", pr(triage.PRInfo{State: "LOCAL", LocalPath: "/r", Uncommitted: true}), "", true},
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

func TestFixTargetsThreads(t *testing.T) {
	dup := 0
	u := &triage.Unit{ID: "a.go:f", Issues: []triage.Issue{{Title: "one"}}, Threads: []triage.Thread{
		{ID: "ok", Trusted: true, Status: triage.ThreadValid, Issue: &triage.Issue{Title: "confirmed"}, Comments: []triage.ThreadComment{{Author: "a", Body: "leak", Trusted: true}}},
		{ID: "dup", Trusted: true, Status: triage.ThreadValid, Issue: &triage.Issue{Title: "same as one"}, DuplicateOf: &dup},
		{ID: "no", Trusted: true, Status: triage.ThreadRejected, Comments: []triage.ThreadComment{{Author: "a", Body: "maybe nil?\nmore", Trusted: true}}},
		{ID: "fixed", Trusted: true, Status: triage.ThreadValid, Issue: &triage.Issue{Title: "done"}, Fixed: true},
		{ID: "evil", Trusted: false, Status: triage.ThreadUntrusted},
	}}
	r := &PRResult{Files: []resultFile{{FileDiff: triage.FileDiff{Path: "a.go"}, Units: []resultUnit{{Unit: u}}}}}
	titles := func(ts []targetedIssue) string {
		var s []string
		for _, x := range ts {
			s = append(s, x.Issue.Title)
		}
		return strings.Join(s, ",")
	}
	if got := titles(fixTargets(r, fixRequest{All: true})); got != "one" {
		t.Errorf("all without comments = %s", got)
	}
	if got := titles(fixTargets(r, fixRequest{All: true, Comments: true})); got != "one,confirmed" {
		t.Errorf("all with comments = %s", got)
	}
	anyway := fixTargets(r, fixRequest{UnitID: "a.go:f", Thread: "no"})
	if titles(anyway) != "maybe nil?" || anyway[0].Comment == nil || !strings.Contains(anyway[0].Comment.Text, "> maybe nil?") {
		t.Errorf("fix anyway = %+v", anyway)
	}
	if got := fixTargets(r, fixRequest{UnitID: "a.go:f", Thread: "fixed"}); len(got) != 0 {
		t.Errorf("fixed thread targeted: %+v", got)
	}
	if got := fixTargets(r, fixRequest{UnitID: "a.go:f", Thread: "evil"}); len(got) != 1 {
		t.Errorf("untrusted thread not fixable on request: %+v", got)
	}
}

func TestLocalChanges(t *testing.T) {
	triaged := &triage.PRInfo{HeadRef: "feature", BaseRef: "main", BaseOid: "b1", HeadOid: "h1", SnapshotHash: "s1"}
	committed := *triaged
	committed.HeadOid = "h2"
	if w := localChanges(triaged, &committed); w != "" {
		t.Errorf("a commit of the triaged code warned: %s", w)
	}
	edited := committed
	edited.SnapshotHash, edited.HeadRef = "s2", "other"
	w := localChanges(triaged, &edited)
	if !strings.Contains(w, "code changed") || !strings.Contains(w, "now other") || strings.Contains(w, "merge base") {
		t.Errorf("warning: %s", w)
	}
}

// Fixers mix Codex's apply_patch headers into git patches and miscount
// hunks; the patch still applies.
func TestApplyFixPatchMixedFormat(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	for name, body := range map[string]string{"a.go": "package a\n\nfunc f() int { return 0 }\n", "b.go": "package a\n\nfunc g() int { return 0 }\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	patch := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -3 +3 @@\n-func f() int { return 0 }\n+func f() int { return 1 }\n" +
		"*** Update File: b.go\n--- a/b.go\n+++ b/b.go\n@@ -3,7 +3,9 @@\n-func g() int { return 0 }\n+func g() int { return 1 }\n" +
		"*** Add File: c.go\n+package a\n+\n+func h() {}\n*** End Patch"
	changed, err := applyFixPatch(context.Background(), dir, patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 3 {
		t.Errorf("changed %d files, want 3", len(changed))
	}
	for name, want := range map[string]string{"a.go": "return 1", "b.go": "return 1", "c.go": "func h() {}"} {
		if b, _ := os.ReadFile(filepath.Join(dir, name)); !strings.Contains(string(b), want) {
			t.Errorf("%s = %q, want %q in it", name, b, want)
		}
	}
}
