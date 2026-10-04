package triage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amitbet/pr-manager/llm"
)

func TestCriticFiltersAndRatesReviewIssues(t *testing.T) {
	var mu sync.Mutex
	called := 0
	critic := &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		mu.Lock()
		called++
		mu.Unlock()
		if req.Tools[0].Name != "judge_issue" || !strings.Contains(req.Messages[0].Content, "independent code review critic") || !strings.Contains(req.Messages[1].Content, "Reported issue to assess:") {
			t.Errorf("critic request = %+v", req)
		}
		// The issues are judged at once, so answer by which one is asked.
		switch claim := req.Messages[1].Content; {
		case strings.Contains(claim, "false alarm"):
			return toolResp("judge_issue", map[string]any{"valid": false, "reason": "covered by another change"}), nil
		case strings.Contains(claim, "real bug"):
			return toolResp("judge_issue", map[string]any{"valid": true, "severity": "high", "reason": "fails on empty input"}), nil
		default:
			return nil, errors.New("critic unavailable")
		}
	}}
	s := &Summarizer{Critic: critic, Policy: DefaultPolicy()}
	u := &Unit{File: "a.go"}
	s.setIssues(context.Background(), u, map[string]any{"issues": []any{
		map[string]any{"title": "false alarm", "severity": "critical", "failure_scenario": "x"},
		map[string]any{"title": "real bug", "severity": "low", "failure_scenario": "empty input fails"},
		map[string]any{"title": "unverified by critic", "severity": "medium", "failure_scenario": "x"},
	}})
	if called != 3 || !u.Reviewed || len(u.Issues) != 3 {
		t.Fatalf("calls=%d reviewed=%v issues=%+v", called, u.Reviewed, u.Issues)
	}
	// A rejected critical claim stays visible, at low, with the reason.
	if is := u.Issues[0]; is.Title != "false alarm" || is.Severity != "low" || is.Claimed != "critical" || is.CriticRejected != "covered by another change" {
		t.Errorf("rejected issue = %+v", is)
	}
	if u.Issues[1].Title != "real bug" || u.Issues[1].Severity != "high" || u.Issues[2].Title != "unverified by critic" || u.Issues[2].Severity != "medium" {
		t.Errorf("issues = %+v", u.Issues)
	}
}

// The critic calls for one unit's issues are in flight together, and the
// verdicts still land on their own issues in the review's order.
func TestCriticJudgesIssuesConcurrently(t *testing.T) {
	const n = 4
	var mu sync.Mutex
	inFlight, peak := 0, 0
	release := make(chan struct{})
	critic := &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		if inFlight == n {
			close(release)
		}
		mu.Unlock()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
		sev := "low"
		if strings.Contains(req.Messages[1].Content, "bug 2") {
			sev = "high"
		}
		return toolResp("judge_issue", map[string]any{"valid": true, "severity": sev, "reason": "r"}), nil
	}}
	s := &Summarizer{Critic: critic, Policy: DefaultPolicy()}
	var issues []Issue
	for i := range n {
		issues = append(issues, Issue{Title: fmt.Sprintf("bug %d", i), Severity: "medium"})
	}
	got := s.criticize(context.Background(), &Unit{File: "a.go"}, issues, "")
	if peak != n {
		t.Errorf("peak in-flight critic calls = %d, want %d", peak, n)
	}
	for i, is := range got {
		want := "low"
		if i == 2 {
			want = "high"
		}
		if is.Title != fmt.Sprintf("bug %d", i) || is.Severity != want {
			t.Errorf("issue %d = %q at %s, want bug %d at %s", i, is.Title, is.Severity, i, want)
		}
	}
}

// The summarizer's call cap holds across a group's members and their
// critics, which now run side by side.
func TestSummarizerCallCap(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	f := &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		if req.Tools[0].Name == "judge_issue" {
			return toolResp("judge_issue", map[string]any{"valid": true, "severity": "medium", "reason": "r"}), nil
		}
		var units []any
		for _, id := range idsIn(req.Messages[len(req.Messages)-1].Content) {
			units = append(units, map[string]any{"id": id, "bucket": "human", "change_kind": "behavior", "confidence": 0.9, "reason": "r",
				"summary": "s", "issues": []any{
					map[string]any{"title": "a", "severity": "medium", "evidence": "x", "failure_scenario": "y", "introduced_by_pr": true, "depends_on_unseen_code": false},
					map[string]any{"title": "b", "severity": "medium", "evidence": "x", "failure_scenario": "y", "introduced_by_pr": true, "depends_on_unseen_code": false},
				}})
		}
		return toolResp(req.Tools[0].Name, map[string]any{"units": units}), nil
	}}
	s := &Summarizer{LLM: f, Policy: DefaultPolicy(), calls: make(chan struct{}, 2)}
	g := &ReviewGroup{Members: []*Unit{
		unit("a.go:A", "a.go", "A", "func A() {"),
		unit("b.go:B", "b.go", "B", "func B() {"),
		unit("c.go:C", "c.go", "C", "func C() {"),
	}}
	s.AnalyzeGroup(context.Background(), g)
	if peak > 2 {
		t.Errorf("peak in-flight calls = %d, want at most the cap of 2", peak)
	}
	for _, u := range g.Members {
		if !u.Reviewed || len(u.Issues) != 2 {
			t.Errorf("%s: reviewed=%v issues=%d", u.ID, u.Reviewed, len(u.Issues))
		}
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
