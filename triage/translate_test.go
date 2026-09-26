package triage

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/amitbet/pr-manager/llm"
)

// prefixLLM "translates" by prefixing each text; drop leaves one item out.
func prefixLLM(calls *int, drop bool) *fakeLLM {
	var mu sync.Mutex
	return &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		mu.Lock()
		*calls++
		mu.Unlock()
		var in []translateItem
		if err := json.Unmarshal([]byte(req.Messages[1].Content), &in); err != nil {
			return nil, err
		}
		var out []any
		for i, it := range in {
			if drop && i == 0 {
				continue
			}
			out = append(out, map[string]any{"id": float64(it.ID), "text": "he:" + it.Text})
		}
		return toolResp("submit_translation", map[string]any{"items": out}), nil
	}}
}

func TestTranslate(t *testing.T) {
	texts := map[string]UnitText{
		"a.go": {Headline: "Adds retries", Summary: "Retries `Fetch`.", Focus: []string{"unverified: timeouts"},
			Issues: []IssueText{{Title: "Leak", Scenario: "ctx cancelled -> goroutine stays"}}},
		"b.go": {Headline: "Renames a var"},
	}
	for _, lang := range []string{"", "English", " english "} {
		calls := 0
		got, err := Translate(context.Background(), prefixLLM(&calls, false), lang, texts)
		if err != nil || calls != 0 || got["a.go"].Summary != "Retries `Fetch`." {
			t.Errorf("%q: calls=%d err=%v got=%+v, want no translation", lang, calls, err, got)
		}
	}

	calls := 0
	got, err := Translate(context.Background(), prefixLLM(&calls, false), "Hebrew", texts)
	if err != nil {
		t.Fatal(err)
	}
	a := got["a.go"]
	if a.Headline != "he:Adds retries" || a.Summary != "he:Retries `Fetch`." || a.Focus[0] != "he:unverified: timeouts" ||
		a.Issues[0].Title != "he:Leak" || a.Issues[0].Detail != "" || a.Issues[0].Scenario != "he:ctx cancelled -> goroutine stays" {
		t.Errorf("a.go = %+v", a)
	}
	if got["b.go"].Headline != "he:Renames a var" || got["b.go"].Summary != "" {
		t.Errorf("b.go = %+v", got["b.go"])
	}
	if texts["a.go"].Focus[0] != "unverified: timeouts" || texts["a.go"].Issues[0].Title != "Leak" {
		t.Errorf("input changed: %+v", texts["a.go"])
	}

	if _, err := Translate(context.Background(), prefixLLM(new(int), true), "Hebrew", texts); err == nil {
		t.Error("a missing item should fail the translation")
	}
}

func TestTranslateBatches(t *testing.T) {
	long := strings.Repeat("x", translateBatchChars/2+1)
	texts := map[string]UnitText{}
	for _, id := range []string{"a", "b", "c", "d"} {
		texts[id] = UnitText{Summary: id + long}
	}
	calls := 0
	got, err := Translate(context.Background(), prefixLLM(&calls, false), "Hebrew", texts)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Errorf("calls = %d, want one per over-half-size text", calls)
	}
	for id, u := range got {
		if u.Summary != "he:"+id+long {
			t.Errorf("%s: wrong text back", id)
		}
	}
}

func TestApplyText(t *testing.T) {
	u := &Unit{Decision: Decision{Headline: "classifier line"}, Summary: "s", Focus: []string{"f1", "f2"},
		Issues: []Issue{{Title: "t", Detail: "d", Evidence: "x := 1", Severity: "high"}}}
	tx := TextOf(u)
	if tx.Headline != "classifier line" {
		t.Errorf("headline falls back to the classifier's: %q", tx.Headline)
	}
	ApplyText(u, UnitText{Headline: "H", Summary: "S", Focus: []string{"F1"}, Issues: []IssueText{{Title: "T"}}})
	if u.Headline != "H" || u.Summary != "S" || u.Focus[0] != "f1" || u.Issues[0].Title != "T" || u.Issues[0].Detail != "d" || u.Issues[0].Evidence != "x := 1" {
		t.Errorf("applied: %+v", u)
	}
	u = &Unit{Decision: Decision{Reason: "r", Escalated: []string{"e"}}, Score: &Score{PinWhy: "p"}}
	tx = TextOf(u)
	if tx.Reason != "r" || tx.Escalated[0] != "e" || tx.PinWhy != "p" {
		t.Errorf("bucket reasons: %+v", tx)
	}
	ApplyText(u, UnitText{Reason: "R", Escalated: []string{"E"}, PinWhy: "P"})
	if u.Decision.Reason != "R" || u.Decision.Escalated[0] != "E" || u.Score.PinWhy != "P" || u.Score.FloorWhy != "" {
		t.Errorf("bucket reasons applied: %+v %+v", u.Decision, u.Score)
	}
}
