package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

// A job's JSON is read while its worker updates it; run with -race.
func TestJobEncodingWhileProgressing(t *testing.T) {
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	j, _, progress := tr.newJob("triage", "https://github.com/acme/web/pull/7")
	defer j.finish(nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			progress("classify", i, 100)
			tr.warn(t.Context(), j.ID, "warn")
		}
	}()
	for i := 0; i < 200; i++ {
		rec := httptest.NewRecorder()
		writeJSON(rec, 202, tr.snapshot(j))
		var got job
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ID != j.ID {
			t.Fatalf("job JSON %q: %v", rec.Body.String(), err)
		}
		writeJSON(httptest.NewRecorder(), 200, tr.jobList())
		if jj, ok := tr.job(j.ID); ok {
			writeJSON(httptest.NewRecorder(), 200, jj)
		}
	}
	close(stop)
	wg.Wait()
}

// A re-run that turns the reviewer's repo tools on or off, or reviews with
// another model, is named as a different model.
func TestNoteModelComparesReviewer(t *testing.T) {
	r := &PRResult{CreatedAt: time.Now().Add(-time.Hour), Classifier: "codex/gpt", Summarizer: "codex/gpt"}
	j := &job{Started: time.Now()}
	j.markCached(r)
	j.noteModel(r, "codex/gpt", "codex/gpt +repo tools")
	if j.CachedBy != "codex/gpt" || j.RunsWith != "codex/gpt +repo tools" {
		t.Errorf("repo tools: cached by %q, runs with %q", j.CachedBy, j.RunsWith)
	}
	r = &PRResult{CreatedAt: r.CreatedAt, Classifier: "ollama/qwen", Summarizer: "off"}
	j = &job{Started: time.Now()}
	j.markCached(r)
	j.noteModel(r, "ollama/qwen", "claude-code/sonnet")
	if j.CachedBy != "ollama/qwen, review off" || j.RunsWith != "ollama/qwen, review claude-code/sonnet" {
		t.Errorf("reviewer: cached by %q, runs with %q", j.CachedBy, j.RunsWith)
	}
	j = &job{Started: time.Now()}
	j.markCached(r)
	j.noteModel(r, "ollama/qwen", "off")
	if j.CachedBy != "" || j.RunsWith != "" {
		t.Errorf("same models named as different: %q, %q", j.CachedBy, j.RunsWith)
	}
}

// Restoring an issue drops every record that matches it, not only the one
// that matched first.
func TestRestoreDropsEveryMatchingRecord(t *testing.T) {
	tr, mux, r := dismissFixture(t)
	u := unitOf(r)
	keys := triage.IssueKeys(u.ID, u.Issues[0])
	for _, k := range keys[:2] {
		if err := tr.dismissed.add(r.PR.PRRef, Dismissal{Key: k, Kind: "issue", Unit: u.ID}); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := tr.Load(r.Key)
	if err != nil || !unitOf(loaded).Issues[0].Dismissed {
		t.Fatalf("setup: issue not dismissed: %v", err)
	}
	back := do(t, mux, "DELETE", "/api/results/"+r.Key+"/dismissals/"+unitOf(loaded).Issues[0].DismissKey, "")
	if unitOf(back).Issues[0].Dismissed {
		t.Errorf("restore left the issue dismissed under %s", unitOf(back).Issues[0].DismissKey)
	}
}

// Drafts are written through a temporary file that does not stay behind.
func TestDraftSaveLeavesNoTemp(t *testing.T) {
	rv, err := newReviews(options{cache: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref := triage.PRRef{Owner: "acme", Repo: "web", Number: 7}
	want := []Draft{{ID: "a", Path: "a.go", Line: 3, Side: "RIGHT", Body: "hi"}}
	if err := rv.save(ref, want); err != nil {
		t.Fatal(err)
	}
	got, err := rv.load(ref)
	if err != nil || len(got) != 1 || got[0].Body != "hi" {
		t.Fatalf("load = %v, %v", got, err)
	}
	left, _ := filepath.Glob(filepath.Join(rv.dir, "*.tmp"))
	if len(left) > 0 {
		t.Errorf("temporary files left: %v", left)
	}
	if _, err := os.Stat(rv.draftFile(ref)); err != nil {
		t.Errorf("draft file: %v", err)
	}
}
