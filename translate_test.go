package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

type countingLLM struct{ calls int }

func (c *countingLLM) Call(_ context.Context, req llm.LLMRequest) (*llm.LLMResponse, error) {
	c.calls++
	var in []struct {
		ID   int    `json:"id"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal([]byte(req.Messages[1].Content), &in)
	var out []any
	for _, it := range in {
		out = append(out, map[string]any{"id": float64(it.ID), "text": "he:" + it.Text})
	}
	return &llm.LLMResponse{ToolCalls: []llm.ToolCall{{Name: "submit_translation", Arguments: map[string]any{"items": out}}}}, nil
}
func (c *countingLLM) ModelID() string { return "fake" }
func (c *countingLLM) Name() string    { return "fake" }

func TestTranslationCache(t *testing.T) {
	fake := &countingLLM{}
	old := newTranslator
	newTranslator = func(options) (llm.LLMTool, error) { return fake, nil }
	defer func() { newTranslator = old }()

	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	u := &triage.Unit{ID: "a.go", File: "a.go", Headline: "Adds retries", Summary: "Retries Fetch."}
	r := &PRResult{Key: "k1", Files: []resultFile{{Units: []resultUnit{{Unit: u}}}}}
	lang := func(s string) jobOptions { return jobOptions{SummaryLang: &s} }

	for _, l := range []string{"", "English", "english"} {
		got, err := tr.translation(context.Background(), r, lang(l))
		if err != nil || got.Lang != "" || len(got.Units) != 0 {
			t.Errorf("%q: %+v %v, want no translation", l, got, err)
		}
	}
	if fake.calls != 0 {
		t.Fatalf("English made %d calls", fake.calls)
	}

	for range 2 {
		got, err := tr.translation(context.Background(), r, lang("Hebrew"))
		if err != nil || got.Lang != "Hebrew" || got.Units["a.go"].Summary != "he:Retries Fetch." {
			t.Fatalf("Hebrew: %+v %v", got, err)
		}
	}
	if fake.calls != 1 {
		t.Errorf("calls = %d, want the second from the cache", fake.calls)
	}

	u.Summary = "Retries Fetch twice."
	if got, _ := tr.translation(context.Background(), r, lang("Hebrew")); got.Units["a.go"].Summary != "he:Retries Fetch twice." || fake.calls != 2 {
		t.Errorf("changed text should be translated again: %+v calls=%d", got, fake.calls)
	}

	r.SummaryLang = "Hebrew" // reviewed in Hebrew before translation
	if got, _ := tr.translation(context.Background(), r, lang("Hebrew")); got.Lang != "" || fake.calls != 2 {
		t.Errorf("a result already in the language is shown as it is: %+v", got)
	}
}

type overviewLLM struct{ calls int }

func (c *overviewLLM) Call(context.Context, llm.LLMRequest) (*llm.LLMResponse, error) {
	c.calls++
	return &llm.LLMResponse{ToolCalls: []llm.ToolCall{{Name: "submit_overview", Arguments: map[string]any{
		"why": "Fetch fails on flaky links.", "how": []any{"Retries Fetch"}, "issues": []any{"Slower failures"}}}}}, nil
}
func (c *overviewLLM) ModelID() string { return "fake" }
func (c *overviewLLM) Name() string    { return "fake" }

func TestOverviewWrittenOnceAndTranslated(t *testing.T) {
	fake := &overviewLLM{}
	oldO := newOverviewer
	newOverviewer = func(options) (llm.LLMTool, error) { return fake, nil }
	defer func() { newOverviewer = oldO }()
	tfake := &countingLLM{}
	oldT := newTranslator
	newTranslator = func(options) (llm.LLMTool, error) { return tfake, nil }
	defer func() { newTranslator = oldT }()

	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	u := &triage.Unit{ID: "a.go", File: "a.go", Headline: "Adds retries", Summary: "Retries Fetch."}
	r := &PRResult{Key: "k1", PR: &triage.PRInfo{Title: "Retry"}, Budgets: triage.DefaultTierPolicy().OrderedBudgets(),
		Files: []resultFile{{Units: []resultUnit{{Unit: u}}}}}
	b, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(tr.results, "k1.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		ov, err := tr.overview(context.Background(), "k1", jobOptions{})
		if err != nil || ov.Why != "Fetch fails on flaky links." {
			t.Fatalf("overview: %+v %v", ov, err)
		}
	}
	if fake.calls != 1 {
		t.Errorf("calls = %d, want the overview saved with the result", fake.calls)
	}

	saved, err := tr.Load("k1")
	if err != nil || saved.Overview == nil {
		t.Fatalf("saved: %+v %v", saved, err)
	}
	s := "Hebrew"
	got, err := tr.translation(context.Background(), saved, jobOptions{SummaryLang: &s})
	if err != nil || got.Overview == nil || got.Overview.Why != "he:Fetch fails on flaky links." || got.Overview.Issues[0] != "he:Slower failures" || got.Units["a.go"].Summary != "he:Retries Fetch." {
		t.Errorf("translation: %+v %v", got, err)
	}
}

// A triage translates the result it just made; opening the saved result
// then finds that translation instead of making another.
func TestTranslationAtTriageIsFoundOnOpen(t *testing.T) {
	fake := &countingLLM{}
	old := newTranslator
	newTranslator = func(options) (llm.LLMTool, error) { return fake, nil }
	defer func() { newTranslator = old }()

	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	u := &triage.Unit{ID: "a.go", File: "a.go", Headline: "Adds retries", Summary: "Retries Fetch.",
		Focus: []string{"backoff"}, Issues: []triage.Issue{{Title: "No cap", Detail: "Retries forever."}}}
	r := &PRResult{Key: "k1", PR: &triage.PRInfo{Title: "Retry"}, Budgets: triage.DefaultTierPolicy().OrderedBudgets(),
		Overview: &triage.Overview{Why: "Flaky links.", How: []string{"Retries Fetch"}},
		Files: []resultFile{{Units: []resultUnit{{Unit: u}}}}}
	b, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(tr.results, "k1.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.translate(context.Background(), r, options{summaryLang: "Hebrew"}); err != nil {
		t.Fatal(err)
	}
	saved, err := tr.Load("k1")
	if err != nil {
		t.Fatal(err)
	}
	s := "Hebrew"
	got, err := tr.translation(context.Background(), saved, jobOptions{SummaryLang: &s})
	if err != nil || got.Units["a.go"].Summary != "he:Retries Fetch." || got.Overview.Why != "he:Flaky links." {
		t.Fatalf("translation: %+v %v", got, err)
	}
	if fake.calls != 1 {
		t.Errorf("calls = %d, want the open to reuse the triage's translation", fake.calls)
	}
}
