package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// guardPatch fixes Div; notePatch only adds a comment, which leaves the
// divide by zero in place.
const (
	guardPatch = "diff --git a/calc.go b/calc.go\n--- a/calc.go\n+++ b/calc.go\n@@ -3,4 +3,7 @@\n // Div divides a by b.\n func Div(a, b int) int {\n+\tif b == 0 {\n+\t\treturn 0\n+\t}\n \treturn a / b\n }\n"
	notePatch  = "diff --git a/calc.go b/calc.go\n--- a/calc.go\n+++ b/calc.go\n@@ -3,4 +3,5 @@\n // Div divides a by b.\n func Div(a, b int) int {\n+\t// b is checked by callers.\n \treturn a / b\n }\n"
)

// fakeModel is an OpenAI-compatible server that plays reviewer, critic and
// fixer: the review finds a divide by zero in Div until the code guards
// it, and the fixer returns patch.
func fakeModel(t *testing.T, patch string) (calls func() map[string]int) {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
			ToolChoice struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_choice"`
		}
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("request: %v", err)
		}
		prompt := ""
		for _, m := range req.Messages {
			prompt += m.Content
		}
		tool := req.ToolChoice.Function.Name
		mu.Lock()
		seen[tool]++
		mu.Unlock()
		unit := map[string]any{"bucket": "human", "change_kind": "behavior", "risk_signals": []string{}, "confidence": 0.9, "headline": "Div divides", "reason": "Arithmetic on caller input.", "summary": "Div now divides.", "focus": []string{}}
		var args any
		switch tool {
		case "submit_analysis":
			unit["issues"] = []any{}
			if !strings.Contains(prompt, "if b == 0") {
				unit["issues"] = []any{map[string]any{"severity": "high", "line": 5, "title": "Div panics when b is zero", "evidence": "return a / b", "failure_scenario": "Div(1, 0) panics: integer divide by zero", "introduced_by_pr": true, "depends_on_unseen_code": false}}
			}
			args = unit
		case "submit_triage":
			args = unit
		case "judge_issue":
			args = map[string]any{"valid": true, "severity": "high", "reason": "Div(1, 0) panics."}
		case "submit_fix":
			args = map[string]any{"reason": "Guard b == 0.", "patch": patch}
		case "submit_overview":
			args = map[string]any{"why": "Div divides.", "how": []string{"Divide a by b."}}
		case "submit_commit_message":
			args = map[string]any{"message": "Guard Div against a zero divisor"}
		default:
			args = map[string]any{}
		}
		a, _ := json.Marshal(args)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message":       map[string]any{"tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": tool, "arguments": string(a)}}}},
		}}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("OPENAI_API_KEY", "test")
	return func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]int{}
		for k, v := range seen {
			out[k] = v
		}
		return out
	}
}

// The fix command reviews a local branch, fixes what the review found in
// place, sees the check come back clean and commits the fix.
func TestFixCommandFixesInPlace(t *testing.T) {
	calls := fakeModel(t, guardPatch)
	dir, git := divRepo(t)

	report := filepath.Join(t.TempDir(), "fix.json")
	summary := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)
	o := fixOptions(t, dir)
	o.fixCommit, o.outFile = true, report
	if err := runFixCmd(context.Background(), o); err != nil {
		t.Fatalf("fix: %v (calls %v)", err, calls())
	}
	b, err := os.ReadFile(filepath.Join(dir, "calc.go"))
	if err != nil || !strings.Contains(string(b), "if b == 0 {") {
		t.Fatalf("calc.go = %q, %v", b, err)
	}
	if log := git("log", "--format=%s", "main..feature"); log != "Guard Div against a zero divisor\ndivide\n" {
		t.Errorf("feature log = %q", log)
	}
	if st := git("status", "--porcelain"); st != "" {
		t.Errorf("left uncommitted: %q", st)
	}
	var sum fixSummary
	raw, _ := os.ReadFile(report)
	if err := json.Unmarshal(raw, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Rounds != 1 || sum.IssuesBefore != 1 || sum.IssuesLeft != 0 || sum.Failing != 0 || sum.Branch != "feature" || sum.Commit == "" {
		t.Errorf("summary = %+v", sum)
	}
	if md, _ := os.ReadFile(summary); !strings.Contains(string(md), "## PR fix") || !strings.Contains(string(md), "1 issue(s) before, 0 left") {
		t.Errorf("step summary:\n%s", md)
	}
	if c := calls(); c["submit_fix"] != 1 || c["judge_issue"] != 1 {
		t.Errorf("model calls = %v", c)
	}
}

// divRepo is a checkout on feature, whose last commit makes Div divide.
func divRepo(t *testing.T) (string, func(...string) string) {
	dir, git := gitRepo(t, "user.name", "Test", "user.email", "test@example.com")
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "calc.go"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package calc\n\n// Div divides a by b.\nfunc Div(a, b int) int {\n\treturn 0\n}\n")
	git("add", ".")
	git("commit", "-qm", "base")
	git("branch", "-M", "main")
	git("switch", "-qc", "feature")
	write("package calc\n\n// Div divides a by b.\nfunc Div(a, b int) int {\n\treturn a / b\n}\n")
	git("commit", "-qam", "divide")
	t.Cleanup(func() { baseOverride = "" })
	return dir, git
}

func fixOptions(t *testing.T, dir string) options {
	return options{
		dir: dir, base: "main", baseSet: true, cache: t.TempDir(), codemapDir: "off",
		classifier: "openai-api", classifyModel: "gpt-4.1", summarizer: "openai-api", summaryModel: "gpt-4.1",
		fallback: "off", translator: "auto", lint: "off", lintSet: true, concurrency: 2, reviewConcurrency: 2,
		fixRounds: 3, fixIn: "place", failOn: "medium", out: "json", outFile: filepath.Join(t.TempDir(), "fix.json"),
	}
}

// A fix that leaves the issue in place fails the run at -fail-on, and
// leaves its change uncommitted without -commit.
func TestFixCommandFailsOnWhatRemains(t *testing.T) {
	fakeModel(t, notePatch)
	dir, git := divRepo(t)
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	o := fixOptions(t, dir)
	o.fixRounds, o.failOn = 1, "high"
	if err := runFixCmd(context.Background(), o); err != errIssuesLeft {
		t.Fatalf("fix = %v, want errIssuesLeft", err)
	}
	if st := git("status", "--porcelain"); !strings.Contains(st, "calc.go") {
		t.Errorf("fix not left in the checkout: %q", st)
	}
	var sum fixSummary
	raw, _ := os.ReadFile(o.outFile)
	if err := json.Unmarshal(raw, &sum); err != nil || sum.Rounds != 1 || sum.IssuesLeft != 1 || sum.Failing != 1 || sum.Commit != "" {
		t.Errorf("summary = %+v, %v", sum, err)
	}
	// Off: the same result passes.
	o.failOn = "off"
	git("checkout", "--", "calc.go")
	if err := runFixCmd(context.Background(), o); err != nil {
		t.Errorf("-fail-on off: %v", err)
	}
}

// A worktree does not have the checkout's uncommitted changes, so fixing
// one there needs them committed first.
func TestFixCommandWorktreeNeedsCleanCheckout(t *testing.T) {
	fakeModel(t, guardPatch)
	dir, _ := divRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := fixOptions(t, dir)
	o.fixIn = "worktree"
	if err := runFixCmd(context.Background(), o); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("worktree fix of a dirty checkout: %v", err)
	}
}
