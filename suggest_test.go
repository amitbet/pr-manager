package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/llm"
)

// A suggestion is made in a scratch copy, leaves the checkout as it was,
// is kept on the result, and a fix asked to start from it applies it
// without asking the fixer again.
func TestSuggestFixThenApply(t *testing.T) {
	dir, old := divFixture(t)
	fixerCalls := 0
	oldFix, oldReview, oldAudit, oldSub := newFixLLM, newReviewLLM, newAuditLLM, hasSubscription
	defer func() { newFixLLM, newReviewLLM, newAuditLLM, hasSubscription = oldFix, oldReview, oldAudit, oldSub }()
	newFixLLM = func(provider, model string) (llm.LLMTool, error) {
		return &reviewerFake{model: "fixer", fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
			fixerCalls++
			if fixerCalls > 1 {
				return nil, errors.New("the fixer was asked again")
			}
			return toolAnswer("submit_fix", map[string]any{"patch": divGuardPatch, "reason": "return 0 for b == 0"}), nil
		}}, nil
	}
	newReviewLLM = cleanReviewer
	hasSubscription = func(string) bool { return false }
	o := fixLoopOptions()
	o.fixAuditor, o.cache = "off", t.TempDir()
	tr, err := newTriager(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.saveResult(old); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "p.go"))

	sug, err := tr.suggestFix(context.Background(), old.Key, fixRequest{UnitID: "p.go:Div", Issue: 0, jobOptions: jobOptions{Summarizer: "fake", SummaryModel: "m"}}, func(string, int, int) {})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sug.Patch, "+\tif b == 0 {") || sug.Title != "division by zero" || sug.Scope != "" {
		t.Fatalf("suggestion: %+v", sug)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "p.go")); string(after) != string(before) {
		t.Fatalf("the checkout changed:\n%s", after)
	}
	saved, err := tr.Load(old.Key)
	if err != nil || len(saved.Suggestions) != 1 {
		t.Fatalf("kept: %+v %v", saved.Suggestions, err)
	}

	req := fixRequest{Key: old.Key, UnitID: "p.go:Div", Issue: 0, MaxRounds: 1, Suggested: true}
	next, applied, err := tr.fixRounds(context.Background(), "job3", saved, req, o, fixRun{dir: dir, branch: "main", location: "branch", base: old.PR.HeadOid}, fixTargets(saved, req), func(string, int, int) {})
	if err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile(filepath.Join(dir, "p.go"))
	if applied != 1 || fixerCalls != 1 || !strings.Contains(string(src), "if b == 0 {") {
		t.Fatalf("applied %d, fixer calls %d:\n%s", applied, fixerCalls, src)
	}
	if next.Tests == nil || !next.Tests.OK() {
		t.Fatalf("tests: %+v", next.Tests)
	}

	// Not one issue: no suggestion.
	if _, err := tr.suggestFix(context.Background(), old.Key, fixRequest{All: true}, func(string, int, int) {}); err == nil {
		t.Fatal("suggested for all issues")
	}
}
