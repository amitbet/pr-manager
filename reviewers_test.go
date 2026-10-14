package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

// reviewerFake answers as the extra reviewer it is named for, and as the
// critic and the dedupe call.
type reviewerFake struct {
	model string
	fn    func(req llm.LLMRequest) (*llm.LLMResponse, error)
}

func (f *reviewerFake) Call(_ context.Context, req llm.LLMRequest) (*llm.LLMResponse, error) {
	return f.fn(req)
}
func (f *reviewerFake) ModelID() string { return f.model }
func (f *reviewerFake) Name() string    { return "fake" }

func toolAnswer(name string, args map[string]any) *llm.LLMResponse {
	return &llm.LLMResponse{ToolCalls: []llm.ToolCall{{Name: name, Arguments: args}}}
}

// Two reviewers report the same bug: both findings are checked by the
// critic, kept with their reviewer's name, linked as one, and pin the
// unit. A third reviewer that fails keeps what its last run found.
func TestRunReviewersPoolsFindings(t *testing.T) {
	var mu sync.Mutex
	var prompts []string
	finding := func(title string) map[string]any {
		return map[string]any{"findings": []any{map[string]any{"file": "a.go", "line": float64(2), "severity": "high", "title": title, "failure_scenario": "nil map", "evidence": "m[k] = v"}}}
	}
	oldL, oldC := newReviewerLLM, newReviewerCritic
	defer func() { newReviewerLLM, newReviewerCritic = oldL, oldC }()
	newReviewerLLM = func(provider, model string) (llm.LLMTool, error) {
		return &reviewerFake{model: model, fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
			mu.Lock()
			prompts = append(prompts, req.Messages[len(req.Messages)-1].Content)
			mu.Unlock()
			switch model {
			case "one":
				return toolAnswer("submit_findings", finding("write to a nil map")), nil
			case "two":
				return toolAnswer("submit_findings", finding("nil map is written")), nil
			}
			return nil, errors.New("rate limited")
		}}, nil
	}
	critic := &reviewerFake{model: "critic", fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		switch req.Tools[0].Name {
		case "judge_issue":
			return toolAnswer("judge_issue", map[string]any{"valid": true, "severity": "high", "reason": "r"}), nil
		case "merge_duplicates":
			return toolAnswer("merge_duplicates", map[string]any{"sets": []any{map[string]any{"ids": []any{"f1", "f2"}}}}), nil
		}
		return nil, errors.New("unexpected " + req.Tools[0].Name)
	}}
	newReviewerCritic = func(options) (*triage.Summarizer, error) {
		return &triage.Summarizer{LLM: critic, Critic: critic, Policy: triage.DefaultPolicy()}, nil
	}

	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	u := &triage.Unit{ID: "a.go:F", File: "a.go", Reviewed: true, Decision: triage.Decision{Bucket: triage.BucketNone},
		Score:  &triage.Score{Base: 10, Kind: 1, Prior: 10, Clean: 1},
		Issues: []triage.Issue{{Title: "kept from last run", Severity: "low", Source: "three", Scenario: "x"}}}
	r := &PRResult{Key: "k1", PR: &triage.PRInfo{LocalPath: t.TempDir(), BaseOid: "base1"},
		Reviewers: []extraRun{{Name: "three", Model: "fake/three", Found: 1, Kept: 1}},
		Files:     []resultFile{{Units: []resultUnit{{Unit: u, Hunks: []triage.Hunk{{NewStart: 1, NewLines: 5}}}}}}}
	b, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(tr.results, "k1.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	revs := []extraReviewer{
		{Name: "one", Provider: "codex", Model: "one", Skill: "/code-review"},
		{Name: "two", Provider: "codex", Model: "two", Prompt: "look for nil maps"},
		{Name: "three", Provider: "codex", Model: "three", Prompt: "x"},
	}
	if err := checkReviewers(revs); err != nil {
		t.Fatal(err)
	}
	if err := tr.runReviewers(context.Background(), r, revs, options{}, func(string, int, int) {}); err != nil {
		t.Fatal(err)
	}
	for _, p := range prompts {
		if !strings.Contains(p, "git diff base1") || !strings.Contains(p, "- a.go") {
			t.Errorf("prompt:\n%s", p)
		}
	}

	got, err := tr.Load("k1")
	if err != nil {
		t.Fatal(err)
	}
	gu := got.Files[0].Units[0].Unit
	sources := map[string]triage.Issue{}
	for _, is := range gu.Issues {
		sources[is.Source] = is
	}
	if len(gu.Issues) != 3 || sources["one"].Title == "" || sources["two"].Title == "" || sources["three"].Title != "kept from last run" {
		t.Fatalf("issues = %+v", gu.Issues)
	}
	if (sources["one"].SameAs == nil) == (sources["two"].SameAs == nil) {
		t.Errorf("want one finding linked to the other: %+v / %+v", sources["one"], sources["two"])
	}
	if gu.Decision.Bucket != triage.BucketHuman {
		t.Errorf("bucket = %s, want human", gu.Decision.Bucket)
	}
	runs := map[string]extraRun{}
	for _, run := range got.Reviewers {
		runs[run.Name] = run
	}
	if runs["one"].Kept != 1 || runs["one"].Model != "fake/one" || runs["two"].Found != 1 {
		t.Errorf("runs = %+v", got.Reviewers)
	}
	if run := runs["three"]; run.Error == "" || run.Kept != 1 {
		t.Errorf("failed run = %+v, want the error and the last run's counts", run)
	}
}

func TestCheckReviewers(t *testing.T) {
	for _, c := range []struct {
		revs []extraReviewer
		want string
	}{
		{nil, "no reviewers"},
		{[]extraReviewer{{Provider: "codex", Skill: "s"}}, "needs a name"},
		{[]extraReviewer{{Name: "a", Provider: "codex"}}, "skill or a prompt"},
		{[]extraReviewer{{Name: "a", Provider: "openai-api", Skill: "/review"}}, "skills need codex or claude-code"},
		{[]extraReviewer{{Name: "a", Provider: "codex", Skill: "s"}, {Name: "a", Provider: "claude-code", Skill: "s"}}, "two reviewers"},
		{[]extraReviewer{{Name: "a", Provider: "openai-api", Prompt: "review it"}}, ""},
	} {
		err := checkReviewers(c.revs)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%+v: %v, want %q", c.revs, err, c.want)
		}
	}
}
