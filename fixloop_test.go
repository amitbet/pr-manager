package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

const divSrc = "package p\n\nfunc Div(a, b int) int {\n\treturn a / b\n}\n"

// The fixer's answers, round by round: one that breaks a test, one that
// fixes the issue and adds a helper, and the trim that takes it out.
const (
	brokenPatch = `diff --git a/p.go b/p.go
--- a/p.go
+++ b/p.go
@@ -3,3 +3,3 @@
 func Div(a, b int) int {
-	return a / b
+	return a * b
 }
`
	helperPatch = `diff --git a/p.go b/p.go
--- a/p.go
+++ b/p.go
@@ -3,3 +3,10 @@
 func Div(a, b int) int {
-	return a / b
+	if b == 0 {
+		return 0
+	}
+	return SafeDiv(a, b)
+}
+
+func SafeDiv(a, b int) int {
+	return a / b
 }
`
	trimPatch = `diff --git a/p.go b/p.go
--- a/p.go
+++ b/p.go
@@ -6,7 +6,3 @@
 	}
-	return SafeDiv(a, b)
-}
-
-func SafeDiv(a, b int) int {
-	return a / b
+	return a / b
 }
`
)

// TestFixLoopTestsAuditAndTrim runs a fix's rounds against fake models: a
// round that breaks a test is undone and the next one is told what broke;
// the audit flags a helper the fix didn't need; the trim takes it out;
// and the result records it all.
func TestFixLoopTestsAuditAndTrim(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	dir, old := divFixture(t)
	var mu sync.Mutex
	var fixPrompts []string
	oldFix, oldReview, oldAudit, oldSub := newFixLLM, newReviewLLM, newAuditLLM, hasSubscription
	defer func() { newFixLLM, newReviewLLM, newAuditLLM, hasSubscription = oldFix, oldReview, oldAudit, oldSub }()
	newFixLLM = func(provider, model string) (llm.LLMTool, error) {
		return &reviewerFake{model: "fixer", fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			p := req.Messages[len(req.Messages)-1].Content
			fixPrompts = append(fixPrompts, p)
			switch {
			case strings.Contains(p, "This is a trim round"):
				return toolAnswer("submit_fix", map[string]any{"patch": trimPatch, "reason": "inlined it", "extra": ""}), nil
			case strings.Contains(p, "broke tests"):
				return toolAnswer("submit_fix", map[string]any{"patch": helperPatch, "reason": "guard b", "extra": "SafeDiv: keeps the division in one place"}), nil
			}
			return toolAnswer("submit_fix", map[string]any{"patch": brokenPatch, "reason": "oops"}), nil
		}}, nil
	}
	newReviewLLM = cleanReviewer
	hasSubscription = func(p string) bool { return p == "codex" }
	audits := 0
	newAuditLLM = func(provider, model string) (llm.LLMTool, error) {
		return &reviewerFake{model: "auditor", fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
			mu.Lock()
			audits++
			mu.Unlock()
			return toolAnswer("submit_fix_audit", map[string]any{
				"fixed":   []any{map[string]any{"issue": float64(1), "verdict": "yes", "reason": "b == 0 returns 0"}},
				"extra":   []any{map[string]any{"file": "p.go", "line": float64(10), "kind": "new_helper", "evidence": "func SafeDiv(a, b int) int {", "why": "Div can divide itself"}},
				"summary": "1 of 1 fixed; 1 addition not needed",
			}), nil
		}}, nil
	}

	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	o := fixLoopOptions()
	req := fixRequest{Key: "local", All: true, MaxRounds: 1}
	issues := fixTargets(old, req)
	if len(issues) != 1 {
		t.Fatalf("targets: %+v", issues)
	}
	next, applied, err := tr.fixRounds(context.Background(), "job1", old, req, o, fixRun{dir: dir, branch: "main", location: "branch", base: old.PR.HeadOid}, issues, func(string, int, int) {})
	if err != nil {
		t.Fatal(err)
	}

	src, _ := os.ReadFile(filepath.Join(dir, "p.go"))
	// Git on Windows may check the file out with CRLF line ends.
	if want := "package p\n\nfunc Div(a, b int) int {\n\tif b == 0 {\n\t\treturn 0\n\t}\n\treturn a / b\n}\n"; strings.ReplaceAll(string(src), "\r\n", "\n") != want {
		t.Fatalf("final code:\n%s", src)
	}
	if applied != 2 {
		t.Fatalf("applied %d rounds, want the fix and the trim", applied)
	}
	// The round after the undone one was told what broke.
	if len(fixPrompts) != 3 || !strings.Contains(fixPrompts[1], "TestDiv") || !strings.Contains(fixPrompts[1], "was undone") {
		t.Fatalf("fixer prompts: %d", len(fixPrompts))
	}
	if audits != 1 {
		t.Fatalf("audits: %d", audits)
	}
	if len(next.FixAudit) != 2 || next.FixAudit[0].Extra[0].State != "trimmed" || !next.FixAudit[1].Trim || next.FixAudit[1].Undone != "" {
		t.Fatalf("audit record: %+v", next.FixAudit)
	}
	if !next.FixAudit[0].Independent || next.FixAudit[0].Added != 7 {
		t.Fatalf("round record: %+v", next.FixAudit[0])
	}
	if !strings.Contains(next.FixAuditNote, "1 trimmed") {
		t.Fatalf("note: %q", next.FixAuditNote)
	}
	if next.Tests == nil || !next.Tests.OK() {
		t.Fatalf("final tests: %+v", next.Tests)
	}
	fixedIt := false
	for _, f := range next.FixedIssues {
		fixedIt = fixedIt || f.Title == "division by zero"
	}
	if !fixedIt {
		t.Fatalf("fixed issues: %+v", next.FixedIssues)
	}
	// Nothing was kept, so no scope note is left on the result.
	for _, f := range next.Files {
		for _, u := range f.Units {
			for _, is := range u.Issues {
				if is.Source == triage.ScopeIssueSource {
					t.Fatalf("scope note left: %+v", is)
				}
			}
		}
	}
}

// divFixture is a local repository with an origin, whose branch adds Div
// and a test of it, and a result with a division-by-zero issue on Div.
func divFixture(t *testing.T) (string, *PRResult) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q", "-b", "main")
	gitTest(t, dir, "config", "user.name", "Test")
	gitTest(t, dir, "config", "user.email", "test@example.com")
	for name, body := range map[string]string{
		"go.mod":    "module example.com/m\n\ngo 1.21\n",
		"p.go":      "package p\n",
		"p_test.go": "package p\n\nimport \"testing\"\n\nfunc TestDiv(t *testing.T) {\n\tif Div(4, 2) != 2 {\n\t\tt.Fatal(\"Div(4, 2) != 2\")\n\t}\n}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, dir, "add", "-A")
	gitTest(t, dir, "commit", "-q", "-m", "base")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitTest(t, dir, "init", "-q", "--bare", "-b", "main", origin)
	gitTest(t, dir, "remote", "add", "origin", origin)
	gitTest(t, dir, "push", "-q", "origin", "HEAD:main")
	gitTest(t, dir, "fetch", "-q", "origin")
	gitTest(t, dir, "remote", "set-head", "origin", "main")
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(divSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "commit", "-q", "-am", "add Div")
	head := gitTest(t, dir, "rev-parse", "HEAD")

	unit := &triage.Unit{ID: "p.go:Div", File: "p.go", Symbol: "Div", Reviewed: true, Line: 3,
		Decision: triage.Decision{Bucket: triage.BucketHuman, Source: "reviewer"},
		Issues:   []triage.Issue{{Severity: "high", Line: 4, Title: "division by zero", Evidence: "return a / b", Scenario: "Div(1, 0) panics"}}}
	hunks := []triage.Hunk{{NewStart: 3, NewLines: 3, Lines: []string{"+func Div(a, b int) int {", "+\treturn a / b", "+}"}}}
	old := &PRResult{Key: "local", PR: &triage.PRInfo{LocalPath: dir, HeadOid: head, BaseOid: base, HeadRef: "main"},
		Files: []resultFile{{FileDiff: triage.FileDiff{Path: "p.go", Status: triage.StatusModified}, Units: []resultUnit{{Unit: unit, Hunks: hunks}}}}}
	return dir, old
}

func fixLoopOptions() options {
	return options{summarizer: "fake", summaryModel: "m", fixAuditor: "auto", tests: "on", lint: "off", lintSet: true, codemapDir: "off", groupReview: false, groupReviewSet: true}
}

// cleanReviewer is a review model that finds nothing.
func cleanReviewer(provider, model string) (llm.LLMTool, error) {
	return &reviewerFake{model: "reviewer", fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		name := req.Tools[0].Name
		answer := map[string]any{"bucket": "skim", "change_kind": "behavior", "confidence": 0.9, "reason": "a change", "headline": "h", "summary": "s", "issues": []any{}}
		if name == "submit_group_analysis" {
			var units []any
			for _, line := range strings.Split(req.Messages[len(req.Messages)-1].Content, "\n") {
				if id, ok := strings.CutPrefix(line, "#### unit id: "); ok {
					a := map[string]any{"id": id}
					for k, v := range answer {
						a[k] = v
					}
					units = append(units, a)
				}
			}
			return toolAnswer(name, map[string]any{"units": units}), nil
		}
		return toolAnswer(name, answer), nil
	}}, nil
}

const divGuardPatch = `diff --git a/p.go b/p.go
--- a/p.go
+++ b/p.go
@@ -3,3 +3,6 @@
 func Div(a, b int) int {
+	if b == 0 {
+		return 0
+	}
 	return a / b
 }
`

// TestFixLoopAuditSendsIssueBack: the check finds the issue fixed and the
// audit says the patch only hides the symptom, so the issue goes into the
// next round.
func TestFixLoopAuditSendsIssueBack(t *testing.T) {
	dir, old := divFixture(t)
	var mu sync.Mutex
	var fixPrompts []string
	oldFix, oldReview, oldAudit, oldSub := newFixLLM, newReviewLLM, newAuditLLM, hasSubscription
	defer func() { newFixLLM, newReviewLLM, newAuditLLM, hasSubscription = oldFix, oldReview, oldAudit, oldSub }()
	newFixLLM = func(provider, model string) (llm.LLMTool, error) {
		return &reviewerFake{model: "fixer", fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			fixPrompts = append(fixPrompts, req.Messages[len(req.Messages)-1].Content)
			if len(fixPrompts) == 1 {
				return toolAnswer("submit_fix", map[string]any{"patch": divGuardPatch, "reason": "guard"}), nil
			}
			return toolAnswer("submit_fix", map[string]any{"patch": "", "reason": "the callers must not pass 0; that is outside these files"}), nil
		}}, nil
	}
	newReviewLLM = cleanReviewer
	hasSubscription = func(p string) bool { return p == "codex" }
	audits := 0
	newAuditLLM = func(provider, model string) (llm.LLMTool, error) {
		return &reviewerFake{model: "auditor", fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
			mu.Lock()
			audits++
			mu.Unlock()
			return toolAnswer("submit_fix_audit", map[string]any{
				"fixed":   []any{map[string]any{"issue": float64(1), "verdict": "symptom", "reason": "returns 0 for b == 0, so callers get a wrong mean instead of an error"}},
				"extra":   []any{},
				"summary": "0 of 1 fixed, 1 symptom only",
			}), nil
		}}, nil
	}
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	req := fixRequest{Key: "local", All: true, Recursive: true, MaxRounds: 2}
	next, applied, err := tr.fixRounds(context.Background(), "job2", old, req, fixLoopOptions(), fixRun{dir: dir, branch: "main", location: "branch", base: old.PR.HeadOid}, fixTargets(old, req), func(string, int, int) {})
	if err != nil {
		t.Fatal(err)
	}
	if applied != 1 || audits != 1 {
		t.Fatalf("applied %d, audits %d", applied, audits)
	}
	// Round 2 was asked to fix the same issue again.
	if len(fixPrompts) != 2 || !strings.Contains(fixPrompts[1], "division by zero") {
		t.Fatalf("fixer calls: %d", len(fixPrompts))
	}
	if r := next.FixAudit[0]; r.NotFixed != 1 || r.Verdicts[0].Verdict != "symptom" {
		t.Fatalf("audit: %+v", r)
	}
	if !strings.Contains(next.FixAuditNote, "1 symptom only") || !strings.Contains(next.FixWarning, "fix round 2 failed") {
		t.Fatalf("note %q, warning %q", next.FixAuditNote, next.FixWarning)
	}
	for _, f := range next.FixedIssues {
		if f.Title == "division by zero" {
			t.Fatalf("listed as fixed though the audit says not: %+v", f)
		}
	}
}
