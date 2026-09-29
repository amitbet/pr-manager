package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

// sequenceLLM answers submit_sequence once release is closed.
type sequenceLLM struct {
	calls   int
	prompt  string
	started chan struct{}
	release chan struct{}
}

func (c *sequenceLLM) Call(_ context.Context, req llm.LLMRequest) (*llm.LLMResponse, error) {
	c.calls++
	c.prompt = req.Messages[len(req.Messages)-1].Content
	close(c.started)
	<-c.release
	return &llm.LLMResponse{ToolCalls: []llm.ToolCall{{Name: "submit_sequence", Arguments: map[string]any{
		"title": "Fetch with retries",
		"participants": []any{
			map[string]any{"id": "Client", "label": "Client"},
			map[string]any{"id": "Server", "label": "Server"},
		},
		"steps": []any{
			map[string]any{"from": "Client", "to": "Server", "text": "fetch once", "change": "removed"},
			map[string]any{"from": "Client", "to": "Server", "text": "fetch with retries", "change": "added", "unit": "a.go"},
			map[string]any{"from": "Server", "to": "Client", "text": "body", "kind": "return", "change": "none"},
		},
	}}}}, nil
}
func (c *sequenceLLM) ModelID() string { return "fake" }
func (c *sequenceLLM) Name() string    { return "fake" }

// A triage writes the diagram after saving the result; opening the tab
// meanwhile waits for that call instead of making another, and a save made
// while the model was writing is kept.
func TestSequenceWrittenAfterTriage(t *testing.T) {
	fake := &sequenceLLM{started: make(chan struct{}), release: make(chan struct{})}
	oldO := newOverviewer
	newOverviewer = func(options) (llm.LLMTool, error) { return fake, nil }
	defer func() { newOverviewer = oldO }()

	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// Saved as a triage saves it: the hunks next to the unit, not in it.
	u := &triage.Unit{ID: "a.go", File: "a.go", Headline: "Adds retries"}
	r := &PRResult{Key: "k1", PR: &triage.PRInfo{Title: "Retry"}, Files: []resultFile{{Units: []resultUnit{{Unit: u, Hunks: []triage.Hunk{{NewStart: 1, NewLines: 3}}}}}}}
	b, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(tr.results, "k1.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	tr.sequenceAfter("k1", options{})
	<-fake.started
	// A fix round saves the result while the diagram is being written.
	saved, _ := tr.Load("k1")
	saved.FixRounds = 1
	if err := tr.saveFixResult(saved); err != nil {
		t.Fatal(err)
	}
	opened := make(chan *triage.Sequence)
	go func() {
		sq, err := tr.sequence(context.Background(), "k1", jobOptions{})
		if err != nil {
			t.Error(err)
		}
		opened <- sq
	}()
	time.Sleep(20 * time.Millisecond) // the tab's request is waiting on the lock
	close(fake.release)

	sq := <-opened
	if sq == nil || len(sq.Steps) != 3 || sq.Version != triage.SequenceVersion {
		t.Fatalf("tab got %+v", sq)
	}
	if fake.calls != 1 {
		t.Errorf("calls = %d, want the tab to reuse the triage's call", fake.calls)
	}
	if !strings.Contains(fake.prompt, "id: a.go") {
		t.Errorf("the prompt left the saved unit out:\n%s", fake.prompt)
	}
	got, err := tr.Load("k1")
	if err != nil || got.Sequence == nil || got.FixRounds != 1 {
		t.Errorf("saved: sequence=%v fix_rounds=%d err=%v", got.Sequence != nil, got.FixRounds, err)
	}
}

func TestCapEffort(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", "low"}, {"medium", "low"}, {"xhigh", "low"}, {"low", "low"}, {"minimal", "minimal"}, {"none", "none"}, {"odd", "low"},
	} {
		if got := capEffort(c.in, "low"); got != c.want {
			t.Errorf("capEffort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// blockingLLM answers only once its context is done.
type blockingLLM struct{ started chan struct{} }

func (b *blockingLLM) Call(ctx context.Context, _ llm.LLMRequest) (*llm.LLMResponse, error) {
	b.started <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (b *blockingLLM) ModelID() string { return "fake" }
func (b *blockingLLM) Name() string    { return "fake" }

// The overview, sequence and translation calls outlive the request that
// asked for them, but not shutdown: it cancels them, and waits for the
// sequence written in the background.
func TestShutdownCancelsBackgroundLLMCalls(t *testing.T) {
	fake := &blockingLLM{started: make(chan struct{}, 4)}
	oldO, oldT := newOverviewer, newTranslator
	newOverviewer = func(options) (llm.LLMTool, error) { return fake, nil }
	newTranslator = func(options) (llm.LLMTool, error) { return fake, nil }
	defer func() { newOverviewer, newTranslator = oldO, oldT }()

	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	u := &triage.Unit{ID: "a.go", File: "a.go", Headline: "Adds retries", Summary: "Retries Fetch."}
	r := &PRResult{Key: "k1", PR: &triage.PRInfo{Title: "Retry", Commits: []string{"retry"}}, Files: []resultFile{{Units: []resultUnit{{Unit: u, Hunks: []triage.Hunk{{NewStart: 1, NewLines: 3}}}}}}}
	if err := tr.saveResult(r); err != nil {
		t.Fatal(err)
	}

	// Each request's context is already done: the calls must not stop
	// for that.
	reqCtx, cancelReq := context.WithCancel(context.Background())
	cancelReq()
	errs := make(chan error, 2)
	go func() { _, err := tr.overview(reqCtx, "k1", jobOptions{}); errs <- err }()
	lang := "Hebrew"
	go func() { _, err := tr.translation(reqCtx, r, jobOptions{SummaryLang: &lang}); errs <- err }()
	sequenceDone := make(chan struct{})
	tr.sequenceAfter("k1", options{})
	go func() {
		tr.running.Wait()
		close(sequenceDone)
	}()
	for range 3 {
		select {
		case <-fake.started:
		case <-time.After(5 * time.Second):
			t.Fatal("a call did not start")
		}
	}

	// Returning well before the timeout means shutdown's wait saw the
	// background sequence stop.
	start := time.Now()
	tr.shutdown(5 * time.Second)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("shutdown took %s", d)
	}
	select {
	case <-sequenceDone:
	case <-time.After(time.Second):
		t.Fatal("the background sequence call is still running")
	}
	for range 2 {
		select {
		case err := <-errs:
			if err == nil {
				t.Error("a cancelled call succeeded")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("shutdown left an overview or translation call running")
		}
	}
}
