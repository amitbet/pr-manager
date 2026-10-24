package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

func TestSecondOpinions(t *testing.T) {
	oldL, oldS := newAuditLLM, hasSubscription
	defer func() { newAuditLLM, hasSubscription = oldL, oldS }()
	hasSubscription = func(p string) bool { return p == "codex" }
	var mu sync.Mutex // the units are asked about concurrently
	asked := map[string]bool{}
	newAuditLLM = func(provider, model string) (llm.LLMTool, error) {
		return &reviewerFake{model: model, fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
			p := req.Messages[len(req.Messages)-1].Content
			needs := strings.Contains(p, "Unit b.go:B")
			mu.Lock()
			defer mu.Unlock()
			for _, id := range []string{"a.go:A", "b.go:B", "c.go:C"} {
				if strings.Contains(p, "Unit "+id) {
					asked[id] = true
				}
			}
			return toolAnswer("submit_second_opinion", map[string]any{"needs_review": needs, "reason": "the retry loop never ends at b.go:9"}), nil
		}}, nil
	}
	tp := triage.DefaultTierPolicy()
	b, _, _ := tp.Budget("")
	hunk := []triage.Hunk{{NewStart: 1, NewLines: 1, Lines: []string{"+x := 1"}}}
	unit := func(id string, bucket triage.Bucket, total int, clean float64) *triage.Unit {
		return &triage.Unit{ID: id, File: strings.Split(id, ":")[0], Reviewed: true, Hunks: hunk,
			Decision: triage.Decision{Bucket: bucket},
			// Prior is set so that, with the clean review's trust, the
			// total is what the case wants.
			Score: &triage.Score{Base: 100, Kind: 1, Prior: int(float64(total)/(1-b.Trust*clean) + 0.5), Clean: clean, Total: total}}
	}
	// A and B are just over the human cut-off, C far over it, D not clean.
	units := []*triage.Unit{
		unit("a.go:A", triage.BucketHuman, b.Human+2, 1),
		unit("b.go:B", triage.BucketHuman, b.Human+3, 1),
		unit("c.go:C", triage.BucketHuman, b.Human+40, 1),
		unit("d.go:D", triage.BucketHuman, b.Human+1, 0.5),
	}
	got := borderline(units, b)
	if len(got) != 2 || got[0].ID != "a.go:A" || got[1].ID != "b.go:B" {
		t.Fatalf("borderline: %v", got)
	}

	o := options{summarizer: "claude-code", summaryModel: "opus", fixAuditor: "auto", secondOpinion: true}
	n, agreed := secondOpinions(context.Background(), o, units, tp)
	if n != 2 || agreed != 1 || asked["c.go:C"] {
		t.Fatalf("asked %d agreed %d: %v", n, agreed, asked)
	}
	byID := map[string]*triage.Unit{}
	for _, u := range units {
		byID[u.ID] = u
	}
	a, bu := byID["a.go:A"], byID["b.go:B"]
	if a.Score.Evidence != agreeEvidence || !strings.Contains(a.Score.EvidenceWhy, "second model") || a.Decision.Bucket == triage.BucketHuman {
		t.Fatalf("agreed unit: %+v %s", a.Score, a.Decision.Bucket)
	}
	if bu.Score.Evidence != 0 || bu.Decision.Bucket != triage.BucketHuman || len(bu.Focus) != 1 || !strings.Contains(bu.Focus[0], "b.go:9") {
		t.Fatalf("disagreed unit: %+v %v", bu.Score, bu.Focus)
	}

	// The fixer's own family is no second opinion. Without the key the
	// other family's API isn't picked either.
	t.Setenv("OPENAI_API_KEY", "")
	hasSubscription = func(string) bool { return false }
	if n, _ := secondOpinions(context.Background(), o, units, tp); n != 0 {
		t.Fatal("same family asked")
	}
	o.secondOpinion = false
	hasSubscription = func(p string) bool { return p == "codex" }
	if n, _ := secondOpinions(context.Background(), o, units, tp); n != 0 {
		t.Fatal("asked when off")
	}
}
