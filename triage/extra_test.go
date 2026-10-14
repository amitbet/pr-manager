package triage

import (
	"context"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/llm"
)

func TestDecodeFindings(t *testing.T) {
	got, err := DecodeFindings(map[string]any{"findings": []any{
		map[string]any{"file": "./a.go", "line": float64(12), "severity": "high", "title": "nil map write", "failure_scenario": "first call panics"},
		map[string]any{"file": "b.go", "severity": "critical", "title": "old bug", "failure_scenario": "x", "introduced_by_pr": false},
		map[string]any{"file": "c.go", "severity": "high", "title": "no scenario"},
		"junk",
	}})
	if err != nil || len(got) != 3 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got[0].File != "a.go" || got[0].Issue.Line != 12 || got[0].Issue.Severity != "high" {
		t.Errorf("first = %+v", got[0])
	}
	if !got[1].Issue.PreExisting || got[1].Issue.Severity != "low" || got[2].Issue.Severity != "low" {
		t.Errorf("caps: %+v / %+v", got[1].Issue, got[2].Issue)
	}
	if _, err := DecodeFindings(map[string]any{"findings": "none"}); err == nil {
		t.Error("a reply without a list must not read as no findings")
	}
}

func TestPlaceFindings(t *testing.T) {
	a := &Unit{ID: "a.go:F", File: "a.go", Hunks: []Hunk{{NewStart: 1, NewLines: 10}}, Issues: []Issue{
		{Title: "dup", Severity: "low"},
		{Title: "stale", Severity: "high", Source: "cc"},
		{Title: "other's", Severity: "high", Source: "codex"},
	}}
	b := &Unit{ID: "a.go:G", File: "a.go", Hunks: []Hunk{{NewStart: 40, NewLines: 5}}}
	placed, lost := PlaceFindings([]*Unit{a, b}, "cc", []ExtraFinding{
		{File: "a.go", Issue: Issue{Title: "near G", Line: 43, Severity: "medium"}},
		{File: "a.go", Issue: Issue{Title: "dup", Line: 3, Severity: "medium"}},
		{File: "a.go", Issue: Issue{Title: "dup", Line: 4, Severity: "medium"}},
		{File: "a.go", Issue: Issue{Title: "no line", Severity: "low"}},
		{File: "elsewhere.go", Issue: Issue{Title: "lost", Severity: "high"}},
	})
	if lost != 1 {
		t.Errorf("lost = %d", lost)
	}
	// The reviewer's earlier findings go; another reviewer's stay.
	if len(a.Issues) != 2 || a.Issues[0].Title != "dup" || a.Issues[1].Source != "codex" {
		t.Errorf("a's issues = %+v", a.Issues)
	}
	var titles []string
	for _, is := range placed[a] {
		if is.Source != "cc" {
			t.Errorf("source = %q", is.Source)
		}
		titles = append(titles, is.Title)
	}
	if got := strings.Join(titles, "|"); got != "dup (cc)|dup (cc 2)|no line" {
		t.Errorf("a got %s", got)
	}
	if len(placed[b]) != 1 || placed[b][0].Title != "near G" {
		t.Errorf("b got %+v", placed[b])
	}
}

func TestAddExtraIssuesChecksThemAndPins(t *testing.T) {
	critic := &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		if strings.Contains(req.Messages[1].Content, "false alarm") {
			return toolResp("judge_issue", map[string]any{"valid": false, "reason": "handled by the caller"}), nil
		}
		return toolResp("judge_issue", map[string]any{"valid": true, "severity": "high", "reason": "r"}), nil
	}}
	s := &Summarizer{Critic: critic, Policy: DefaultPolicy()}
	u := &Unit{ID: "a.go:F", File: "a.go", Reviewed: true, Decision: Decision{Bucket: BucketNone}, Score: &Score{Base: 10, Kind: 1, Prior: 10, Clean: 1}}
	s.AddExtraIssues(context.Background(), nil, map[*Unit][]Issue{u: {
		{Title: "false alarm", Severity: "medium", Source: "cc"},
		{Title: "real", Severity: "low", Source: "cc"},
	}}, 2)
	if len(u.Issues) != 2 || u.Issues[0].CriticRejected == "" || u.Issues[0].Severity != "low" || u.Issues[1].Severity != "high" {
		t.Fatalf("issues = %+v", u.Issues)
	}
	DefaultTierPolicy().ApplyIssues(u)
	if u.Decision.Bucket != BucketHuman || !u.Score.PinIssue || u.Attention == 0 {
		t.Errorf("bucket=%s score=%+v attention=%d", u.Decision.Bucket, u.Score, u.Attention)
	}
	// Dismissing it gives the unit back its place.
	u.Issues[1].Dismissed = true
	DefaultTierPolicy().ApplyIssues(u)
	if u.Decision.Bucket == BucketHuman {
		t.Errorf("still human after the dismissal: %+v", u.Score)
	}
}

func TestExtraReviewPrompt(t *testing.T) {
	p := ExtraReviewPrompt("/code-review high", "focus on races", "abc123", []string{"a.go"})
	for _, want := range []string{"git diff abc123", "- a.go", "`/code-review high`", "focus on races", "submit_findings", "read-only"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
}
