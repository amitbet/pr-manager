package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

// pad keeps a.go's F and its end far enough apart that fixes of each
// merge cleanly.
const pad = "\n// 1\n// 2\n// 3\n// 4\n// 5\n// 6\n// 7\n"

func TestFixedIssuesSkipsWhatCameBack(t *testing.T) {
	fixedOne := triage.Issue{Severity: "high", Title: "Leaks the body", Evidence: "http.Get(u)"}
	cameBack := triage.Issue{Severity: "medium", Title: "Ignores the error", Evidence: "_ :="}
	u := &triage.Unit{ID: "a.go:F", File: "a.go", Issues: []triage.Issue{cameBack},
		Threads: []triage.Thread{{ID: "T1", URL: "https://x/1", Fixed: true, Issue: &triage.Issue{Severity: "low", Title: "Rename it"}}, {ID: "T2", Fixed: true}}}
	next := &PRResult{Files: []resultFile{{FileDiff: triage.FileDiff{Path: "a.go"}, Units: []resultUnit{{Unit: u}}}}}
	tried := map[string]targetedIssue{
		issueScope(u.ID, fixedOne): {UnitID: u.ID, File: "a.go", Issue: fixedOne},
		issueScope(u.ID, cameBack): {UnitID: u.ID, File: "a.go", Issue: cameBack},
	}
	found := map[string]bool{issueScope(u.ID, fixedOne): true, issueScope(u.ID, cameBack): true}
	carried := []fixedIssue{{Scope: "earlier", Title: "From the fix this one continued"}}
	got := fixedIssues(carried, next, tried, found, map[string]bool{"T1": true})
	var titles []string
	for _, f := range got {
		titles = append(titles, f.Title)
	}
	// T2 was fixed, but not by this fix: it wasn't a target.
	if want := "From the fix this one continued|Leaks the body|Rename it"; strings.Join(titles, "|") != want {
		t.Errorf("fixed = %q, want %q", strings.Join(titles, "|"), want)
	}
	if got[2].Scope != threadScope("T1") || got[2].URL != "https://x/1" {
		t.Errorf("thread record = %+v", got[2])
	}
}

// fixFixture is a triager whose PR clone has one commit, a review of it
// with one issue, and a fix worktree that changed a.go and added b.go.
func fixFixture(t *testing.T) (tr *triager, review, fix *PRResult, git func(dir string, args ...string) string) {
	t.Helper()
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// The env, not -c, so git run by the code under test (cherry-picks in
	// combineFixes) has an identity too; CI runners have no global one.
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "Test")
		t.Setenv(k+"_EMAIL", "test@example.com")
	}
	git = func(dir string, args ...string) string {
		t.Helper()
		return gitTest(t, dir, args...)
	}
	ref := triage.PRRef{Owner: "acme", Repo: "web", Number: 7}
	repo := tr.fetcher.RepoDir(ref)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n\nfunc F() {}\n"+pad), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repo, "add", "a.go")
	git(repo, "commit", "-qm", "base")
	head := git(repo, "rev-parse", "HEAD")
	pr := &triage.PRInfo{PRRef: ref, State: "OPEN", HeadRef: "feature", HeadOid: head}

	issue := triage.Issue{Severity: "high", Title: "F does nothing", Evidence: "func F() {}"}
	u := &triage.Unit{ID: "a.go:F", File: "a.go", Issues: []triage.Issue{issue}}
	review = &PRResult{Key: "acme__web__7__x", PR: pr, Files: []resultFile{{FileDiff: triage.FileDiff{Path: "a.go"}, Units: []resultUnit{{Unit: u}}}}}
	fix = newFix(t, tr, review, "job1", "func F() { println() }\n"+pad, "b.go")
	return tr, review, fix, git
}

// newFix makes a fix worktree of review's PR with a.go's F replaced by
// body and, when extra is set, an untracked file at that path, and saves
// its result.
func newFix(t *testing.T, tr *triager, review *PRResult, job, body, extra string) *PRResult {
	t.Helper()
	dir := filepath.Join(tr.opts.cache, "fixes", job)
	branch, _, err := checkoutFixBranch(tr.fetcher.RepoDir(review.PR.PRRef), dir, review.PR, job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	if extra != "" {
		if err := os.WriteFile(filepath.Join(dir, extra), []byte("package a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	u := review.Files[0].Units[0]
	fix := &PRResult{Key: "fix__acme__web__7__" + job, PR: review.PR, CreatedAt: time.Now(), LocalFixDir: dir, LocalFixBranch: branch, LocalFixLocation: "worktree",
		FixRounds: 1, FixBase: review.PR.HeadOid, FixedIssues: []fixedIssue{{Scope: issueScope(u.ID, u.Issues[0]), UnitID: u.ID, File: "a.go", Severity: "high", Title: u.Issues[0].Title}}}
	for _, r := range []*PRResult{review, fix} {
		b, _ := json.Marshal(r)
		if err := os.WriteFile(filepath.Join(tr.results, r.Key+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return fix
}

func TestPendingFixesMarksWhatTheyFixed(t *testing.T) {
	tr, review, fix, git := fixFixture(t)
	ctx := context.Background()
	fixes, marks, _, err := tr.pendingFixes(ctx, review)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixes) != 1 || fixes[0].State != "uncommitted" || fixes[0].NoPush != "" || fixes[0].Stale {
		t.Fatalf("fixes = %+v", fixes)
	}
	var paths []string
	for _, f := range fixes[0].Files {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, ",") != "a.go,b.go" {
		t.Errorf("files = %v, want a.go and the untracked b.go", paths)
	}
	if len(marks) != 1 || marks[0].Unit != "a.go:F" || marks[0].Issue == nil || *marks[0].Issue != 0 || marks[0].Key != fix.Key || marks[0].State != "uncommitted" {
		t.Errorf("marks = %+v", marks)
	}

	p, err := inspectFix(ctx, fix, true)
	if err != nil {
		t.Fatal(err)
	}
	if a := p.Files[0]; a.Adds != 1 || a.Dels != 1 || len(a.Hunks) != 1 {
		t.Errorf("a.go = %+v", a)
	}

	git(fix.LocalFixDir, "add", "-A")
	git(fix.LocalFixDir, "commit", "-qm", "fix")
	fixes, marks, _, _ = tr.pendingFixes(ctx, review)
	if len(fixes) != 1 || fixes[0].State != "committed" || fixes[0].Commits != 1 || len(fixes[0].Files) != 2 || len(marks) != 1 || marks[0].State != "committed" {
		t.Errorf("after commit: fixes = %+v, marks = %+v", fixes, marks)
	}

	// Pushed, the fix stays listed until the review is of what it pushed.
	head := git(fix.LocalFixDir, "rev-parse", "HEAD")
	if _, err := tr.updateResult(fix.Key, func(r *PRResult) { r.FixPushed, r.FixPushedAs = head, head }); err != nil {
		t.Fatal(err)
	}
	if fixes, _, _, _ = tr.pendingFixes(ctx, review); len(fixes) != 1 || fixes[0].State != "pushed" || fixes[0].NoPush == "" {
		t.Errorf("pushed: fixes = %+v", fixes)
	}
	newer := *review
	newer.PR = &triage.PRInfo{PRRef: review.PR.PRRef, State: "OPEN", HeadOid: head}
	if fixes, marks, _, _ = tr.pendingFixes(ctx, &newer); len(fixes) != 0 || len(marks) != 0 {
		t.Errorf("review of the pushed head still lists the fix: %+v", fixes)
	}
}

func TestPendingFixesFromAnotherHeadAreStale(t *testing.T) {
	tr, review, _, _ := fixFixture(t)
	moved := *review
	moved.PR = &triage.PRInfo{PRRef: review.PR.PRRef, State: "OPEN", HeadOid: strings.Repeat("1", 40)}
	fixes, marks, _, err := tr.pendingFixes(context.Background(), &moved)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixes) != 1 || !fixes[0].Stale || fixes[0].NoPush == "" || len(marks) != 0 {
		t.Errorf("fixes = %+v, marks = %+v", fixes, marks)
	}
}

func TestCombineFixesInOrder(t *testing.T) {
	tr, review, first, git := fixFixture(t)
	second := newFix(t, tr, review, "job2", "func F() {}\n"+pad+"\nfunc G() {}\n", "")
	base := review.PR.HeadOid
	var heads []string
	for _, r := range []*PRResult{first, second} {
		git(r.LocalFixDir, "add", "-A")
		git(r.LocalFixDir, "commit", "-qm", "fix "+r.Key)
		heads = append(heads, git(r.LocalFixDir, "rev-parse", "HEAD"))
	}
	dir, cleanup, err := tr.combineFixes(context.Background(), review.PR.PRRef, base, []*PRResult{first, second}, heads, []int{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a.go"))
	if want := "package a\n\nfunc F() { println() }\n" + pad + "\nfunc G() {}\n"; string(b) != want {
		t.Errorf("combined a.go = %q, want %q", b, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.go")); err != nil {
		t.Errorf("first fix's new file missing: %v", err)
	}
	if n := git(dir, "rev-list", "--count", base+"..HEAD"); n != "2" {
		t.Errorf("series has %s commits, want 2", n)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("scratch worktree left: %v", err)
	}
}

func TestCombineFixesConflict(t *testing.T) {
	tr, review, first, git := fixFixture(t)
	second := newFix(t, tr, review, "job2", "func F() { panic(1) }\n"+pad, "")
	var heads []string
	for _, r := range []*PRResult{first, second} {
		git(r.LocalFixDir, "add", "-A")
		git(r.LocalFixDir, "commit", "-qm", "fix")
		heads = append(heads, git(r.LocalFixDir, "rev-parse", "HEAD"))
	}
	_, _, err := tr.combineFixes(context.Background(), review.PR.PRRef, review.PR.HeadOid, []*PRResult{first, second}, heads, []int{0, 1})
	if err == nil || !strings.Contains(err.Error(), second.LocalFixDir) {
		t.Fatalf("err = %v, want a conflict naming the second fix", err)
	}
	if out := git(tr.fetcher.RepoDir(review.PR.PRRef), "worktree", "list"); strings.Contains(out, "push-") {
		t.Errorf("scratch worktree left after the conflict: %s", out)
	}
}

func TestDiscardFixRemovesCheckoutAndResults(t *testing.T) {
	tr, review, fix, git := fixFixture(t)
	// A fix that continued it shares the checkout.
	later := *fix
	later.Key, later.CreatedAt = "fix__acme__web__7__job1b", time.Now().Add(time.Minute)
	b, _ := json.Marshal(&later)
	if err := os.WriteFile(filepath.Join(tr.results, later.Key+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if fixes, _, _, _ := tr.pendingFixes(context.Background(), review); len(fixes) != 1 || fixes[0].Key != later.Key {
		t.Fatalf("want the newest result of the checkout only: %+v", fixes)
	}
	if err := tr.discardFix(later.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fix.LocalFixDir); !os.IsNotExist(err) {
		t.Errorf("worktree still there: %v", err)
	}
	repo := tr.fetcher.RepoDir(review.PR.PRRef)
	if git(repo, "branch", "--list", fix.LocalFixBranch) != "" {
		t.Errorf("branch %s left", fix.LocalFixBranch)
	}
	for _, k := range []string{fix.Key, later.Key} {
		if _, err := os.Stat(filepath.Join(tr.results, k+".json")); !os.IsNotExist(err) {
			t.Errorf("result %s left: %v", k, err)
		}
	}
	if _, err := os.Stat(filepath.Join(tr.results, review.Key+".json")); err != nil {
		t.Errorf("the PR's review went too: %v", err)
	}
}

func TestDiscardRefusesTheUsersCheckout(t *testing.T) {
	tr, _, fix, _ := fixFixture(t)
	if _, err := tr.updateResult(fix.Key, func(r *PRResult) { r.LocalFixLocation = "branch" }); err != nil {
		t.Fatal(err)
	}
	if err := tr.discardFix(fix.Key); err == nil {
		t.Fatal("discarded a fix in the user's checkout")
	}
	if _, err := os.Stat(fix.LocalFixDir); err != nil {
		t.Errorf("checkout touched: %v", err)
	}
}

func TestSideIssuesTheFixerSaysItResolved(t *testing.T) {
	hunk := func(l string) []triage.Hunk { return []triage.Hunk{{Lines: []string{l}}} }
	target := triage.Issue{Severity: "high", Title: "F leaks", Evidence: "http.Get"}
	already := triage.Issue{Severity: "high", Title: "Fixed before", Evidence: "x"}
	dismissed := triage.Issue{Severity: "low", Title: "Nobody cares", Dismissed: true}
	foundAgain := triage.Issue{Severity: "medium", Title: "F is exported", Evidence: "func F"}
	caller := triage.Issue{Severity: "high", Title: "G calls F with nil", Evidence: "F(nil)"}
	dup := 0
	old := &PRResult{Files: []resultFile{
		{FileDiff: triage.FileDiff{Path: "a.go"}, Units: []resultUnit{{Unit: &triage.Unit{ID: "a.go:F", File: "a.go", Issues: []triage.Issue{target, already, dismissed, foundAgain},
			Threads: []triage.Thread{{ID: "T1", Status: triage.ThreadValid}, {ID: "T2", Status: triage.ThreadValid}, {ID: "T3", Status: triage.ThreadValid, DuplicateOf: &dup}, {ID: "T4", Status: triage.ThreadRejected}}}, Hunks: hunk("+func F() {}")}}},
		{FileDiff: triage.FileDiff{Path: "g.go"}, Units: []resultUnit{{Unit: &triage.Unit{ID: "g.go:G", File: "g.go", Issues: []triage.Issue{caller}}, Hunks: hunk("+F(nil)")}}},
	}}
	targets := []targetedIssue{{UnitID: "a.go:F", File: "a.go", Issue: target}}
	have := []fixedIssue{{Scope: issueScope("a.go:F", already)}}
	side := sideIssues(old, targets, map[string]bool{"T1": true}, have)
	var titles []string
	for _, x := range side {
		titles = append(titles, fmt.Sprintf("%d:%s", x.ID, x.Title))
	}
	// T2 has no comment, so AsIssue gives it no title.
	if want := "1:F is exported|2:|3:G calls F with nil"; strings.Join(titles, "|") != want {
		t.Fatalf("side = %q, want %q", strings.Join(titles, "|"), want)
	}

	resolved := alsoResolves(map[string]any{"also_resolves": []any{
		map[string]any{"id": float64(1), "reason": "renamed"},
		map[string]any{"id": float64(3), "reason": "F takes nil now"},
		map[string]any{"id": float64(9), "reason": "out of range"},
		map[string]any{"id": 2.5, "reason": "not an id"},
	}}, len(side))
	if len(resolved) != 2 {
		t.Fatalf("resolved = %v", resolved)
	}
	also := map[string]fixedIssue{}
	side = takeResolved(side, resolved, also)
	if len(side) != 1 || side[0].ID != 1 || side[0].rec.Thread != "T2" {
		t.Errorf("left = %+v", side)
	}

	// The fix changed F's diff, and its review found foundAgain again: the
	// fixer was wrong about it. G's diff didn't change.
	next := &PRResult{Files: []resultFile{
		{FileDiff: triage.FileDiff{Path: "a.go"}, Units: []resultUnit{{Unit: &triage.Unit{ID: "a.go:F", File: "a.go", Issues: []triage.Issue{foundAgain}}, Hunks: hunk("+func F() { println() }")}}},
		{FileDiff: triage.FileDiff{Path: "g.go"}, Units: []resultUnit{{Unit: &triage.Unit{ID: "g.go:G", File: "g.go", Issues: []triage.Issue{caller}}, Hunks: hunk("+F(nil)")}}},
	}}
	got := sideFixed(also, old, next, nil)
	if len(got) != 1 || got[0].Title != caller.Title || !got[0].Swept || got[0].Reason != "F takes nil now" {
		t.Errorf("side fixed = %+v", got)
	}
}

func TestDiffFilesCutsUnitsLikeAReview(t *testing.T) {
	src := "package a\n\nfunc F() int {\n\treturn 2\n}\n\nfunc G() int {\n\treturn 4\n}\n"
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,9 +1,9 @@\n package a\n \n func F() int {\n-\treturn 1\n+\treturn 2\n }\n \n func G() int {\n-\treturn 3\n+\treturn 4\n }\n"
	files, err := diffFiles(diff, true, func(string) ([]byte, error) { return []byte(src), nil })
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, u := range files[0].Units {
		ids = append(ids, u.ID)
	}
	if strings.Join(ids, " ") != "a.go:F a.go:G" {
		t.Fatalf("units = %v, want a.go:F a.go:G", ids)
	}
	if h := files[0].Units[1].Hunks[0]; h.NewStart != 7 || !strings.Contains(strings.Join(h.Lines, "\n"), "+\treturn 4") {
		t.Fatalf("G's hunk = %+v", h)
	}
}
