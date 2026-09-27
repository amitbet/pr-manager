package triage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/llm"
)

func TestCriticFiltersAndRatesReviewIssues(t *testing.T) {
	called := 0
	critic := &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		called++
		if req.Tools[0].Name != "judge_issue" || !strings.Contains(req.Messages[0].Content, "independent code review critic") || !strings.Contains(req.Messages[1].Content, "Reported issue to assess:") {
			t.Errorf("critic request = %+v", req)
		}
		switch called {
		case 1:
			return toolResp("judge_issue", map[string]any{"valid": false, "reason": "covered by another change"}), nil
		case 2:
			return toolResp("judge_issue", map[string]any{"valid": true, "severity": "high", "reason": "fails on empty input"}), nil
		case 3:
			return nil, errors.New("critic unavailable")
		default:
			t.Fatal("unexpected critic call")
			return nil, nil
		}
	}}
	s := &Summarizer{Critic: critic, Policy: DefaultPolicy()}
	u := &Unit{File: "a.go"}
	s.setIssues(context.Background(), u, map[string]any{"issues": []any{
		map[string]any{"title": "false alarm", "severity": "critical", "failure_scenario": "x"},
		map[string]any{"title": "real bug", "severity": "low", "failure_scenario": "empty input fails"},
		map[string]any{"title": "unverified by critic", "severity": "medium", "failure_scenario": "x"},
	}})
	if called != 3 || !u.Reviewed || len(u.Issues) != 2 {
		t.Fatalf("calls=%d reviewed=%v issues=%+v", called, u.Reviewed, u.Issues)
	}
	if u.Issues[0].Title != "real bug" || u.Issues[0].Severity != "high" || u.Issues[1].Title != "unverified by critic" || u.Issues[1].Severity != "medium" {
		t.Errorf("issues = %+v", u.Issues)
	}
}

func TestCriticKeepsIssueOnMalformedVerdict(t *testing.T) {
	critic := &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		return toolResp("judge_issue", map[string]any{"valid": true, "severity": "urgent"}), nil
	}}
	s := &Summarizer{Critic: critic, Policy: DefaultPolicy()}
	got := s.criticize(context.Background(), &Unit{File: "a.go"}, []Issue{{Title: "bug", Severity: "medium"}}, "")
	if len(got) != 1 || got[0].Severity != "medium" {
		t.Errorf("issues = %+v", got)
	}
}
