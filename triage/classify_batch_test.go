package triage

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/amitbet/pr-manager/llm"
)

func triageArgs(bucket string) map[string]any {
	return map[string]any{"bucket": bucket, "change_kind": "behavior", "headline": "h", "confidence": 0.95, "reason": "r"}
}

func TestClassifyBatches(t *testing.T) {
	u := func(id string, n int) *Unit {
		return &Unit{ID: id, Hunks: []Hunk{{Lines: []string{"+" + strings.Repeat("x", n)}}}}
	}
	units := []*Unit{u("a", 10), u("b", 10), u("c", 10), u("big", 500), u("d", 10)}
	got := classifyBatches(units, BatchPolicy{MaxUnits: 2, MaxChars: 400})
	var ids []string
	for _, b := range got {
		ids = append(ids, batchID(b))
	}
	want := "a + b|c|big|d"
	if strings.Join(ids, "|") != want {
		t.Errorf("batches = %q, want %q", strings.Join(ids, "|"), want)
	}
}

func TestClassifyBatchMapsLabelsAndRetriesMissing(t *testing.T) {
	var mu sync.Mutex
	var solo []string
	fake := &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		if req.Tools[0].Name == "submit_triage" {
			mu.Lock()
			solo = append(solo, req.Messages[1].Content)
			mu.Unlock()
			return toolResp("submit_triage", triageArgs("skim")), nil
		}
		// U3 is left out, U1 is answered twice (the first wins), and a
		// label out of range is ignored.
		return toolResp("submit_triage_batch", map[string]any{"decisions": []any{
			merge(triageArgs("none"), "unit", "U1"),
			merge(triageArgs("human"), "unit", "U1"),
			merge(triageArgs("human"), "unit", "U2"),
			merge(triageArgs("human"), "unit", "U9"),
		}}), nil
	}}
	c := &LLMClassifier{LLM: fake, Policy: DefaultPolicy()}
	units := []*Unit{{ID: "a", File: "a.go"}, {ID: "b", File: "b.go"}, {ID: "c", File: "c.go"}}
	ds := c.ClassifyBatch(context.Background(), units)
	if ds[0].Bucket != BucketNone || ds[1].Bucket != BucketHuman || ds[2].Bucket != BucketSkim {
		t.Errorf("buckets = %s %s %s", ds[0].Bucket, ds[1].Bucket, ds[2].Bucket)
	}
	if len(solo) != 1 || !strings.Contains(solo[0], "File: c.go") {
		t.Errorf("solo retries = %q, want only c.go", solo)
	}
}

func merge(m map[string]any, k string, v any) map[string]any {
	m[k] = v
	return m
}

type memStore struct {
	mu sync.Mutex
	m  map[string]Decision
}

func (s *memStore) Load(k string) (Decision, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.m[k]
	return d, ok
}

func (s *memStore) Save(k string, d Decision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = d
}

func TestPipelineBatchesAndReusesDecisions(t *testing.T) {
	files, err := ParseDiff(fileDiff("a.go") + fileDiff("b.go") + fileDiff("c.go"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := 0
	fail := false
	fake := &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if fail {
			return nil, context.DeadlineExceeded
		}
		var ds []any
		for i := 1; strings.Contains(req.Messages[1].Content, "## Unit U"+string(rune('0'+i))); i++ {
			ds = append(ds, merge(triageArgs("skim"), "unit", "U"+string(rune('0'+i))))
		}
		return toolResp("submit_triage_batch", map[string]any{"decisions": ds}), nil
	}}
	store := &memStore{m: map[string]Decision{}}
	p := &Pipeline{
		Presorter:   &Presorter{Policy: DefaultPolicy()},
		Classifier:  &LLMClassifier{LLM: fake, Policy: DefaultPolicy()},
		Decisions:   store,
		ClassifyKey: "fake",
		Concurrency: 4,
	}
	for _, u := range p.Run(context.Background(), &Source{Files: files}) {
		if u.Decision.Bucket != BucketSkim {
			t.Errorf("%s: %+v", u.ID, u.Decision)
		}
	}
	if calls != 1 || len(store.m) != 3 {
		t.Fatalf("first run: calls=%d stored=%d, want 1 call for 3 units", calls, len(store.m))
	}
	// Unchanged units come from the store, even with the model down.
	fail = true
	for _, u := range p.Run(context.Background(), &Source{Files: files}) {
		if u.Decision.Bucket != BucketSkim || u.Decision.Failed {
			t.Errorf("second run %s: %+v", u.ID, u.Decision)
		}
	}
	if calls != 1 {
		t.Errorf("second run called the model: calls=%d", calls)
	}
	// A failed decision is not kept.
	p.ClassifyKey = "other"
	p.Run(context.Background(), &Source{Files: files})
	if len(store.m) != 3 {
		t.Errorf("failed decisions were stored: %d entries", len(store.m))
	}
}
