package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

// saveRun writes a cached result for one head of one PR.
func saveRun(t *testing.T, tr *triager, key, base, head string, age time.Duration, units ...*triage.Unit) *PRResult {
	t.Helper()
	r := &PRResult{
		Key: key, CreatedAt: time.Now().Add(-age),
		PR: &triage.PRInfo{PRRef: triage.PRRef{Owner: "acme", Repo: "web", Number: 7}, BaseOid: base, HeadOid: head},
	}
	for _, u := range units {
		r.Files = append(r.Files, resultFile{FileDiff: triage.FileDiff{Path: u.File}, Units: []resultUnit{{Unit: u, Hunks: u.Hunks}}})
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tr.results, key+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return r
}

func reviewedUnit(id, file string, lines ...string) *triage.Unit {
	return &triage.Unit{
		ID: id, File: file, Status: triage.StatusModified, Reviewed: true, Summary: "reviewed " + id,
		Hunks:    []triage.Hunk{{Header: "@@ -1,2 +1,2 @@", NewStart: 1, OldStart: 1, Lines: lines}},
		Decision: triage.Decision{Bucket: triage.BucketSkim, Source: "llm"},
	}
}

func TestSplitKeyHandlesBothKeyLayouts(t *testing.T) {
	for key, want := range map[string][3]string{
		"acme__web__7__abc1234567__1a2b3c4d":        {"acme__web__7", "abc1234567", "1a2b3c4d"},
		"ghe.example.com__acme__web__7__abc__1a2b":  {"ghe.example.com__acme__web__7", "abc", "1a2b"},
		"local__aabbccddee__112233445566__99887766": {"local__aabbccddee", "112233445566", "99887766"},
	} {
		p, c, s, ok := splitKey(key)
		if !ok || p != want[0] || c != want[1] || s != want[2] {
			t.Errorf("splitKey(%q) = %q %q %q %v", key, p, c, s, ok)
		}
	}
	if _, _, _, ok := splitKey("nope"); ok {
		t.Error("a key with too few parts should not split")
	}
}

func TestPreviousRunPicksTheNewestCompatibleRun(t *testing.T) {
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	u := reviewedUnit("a.go:F", "a.go", "+\tx := 1")
	saveRun(t, tr, "acme__web__7__oldhead111__cfg", "base1", "oldhead111", 2*time.Hour, u)
	newest := saveRun(t, tr, "acme__web__7__midhead222__cfg", "base1", "midhead222", time.Hour, u)
	saveRun(t, tr, "acme__web__7__rebased333__cfg", "base2", "rebased333", time.Minute, u) // other merge base
	saveRun(t, tr, "acme__web__7__othercfg44__zzz", "base1", "othercfg44", time.Minute, u) // other settings
	saveRun(t, tr, "acme__api__9__unrelated5__cfg", "base1", "unrelated5", time.Minute, u) // other PR

	got := tr.previousRun("acme__web__7__newhead999__cfg", "base1")
	if got == nil || got.Key != newest.Key {
		t.Fatalf("previousRun = %v, want %s", got, newest.Key)
	}
	// The run being made is never its own previous run.
	if got := tr.previousRun("acme__web__7__midhead222__cfg", "base1"); got != nil && got.Key == "acme__web__7__midhead222__cfg" {
		t.Error("a run matched itself")
	}
	// A rebase leaves nothing to carry.
	if got := tr.previousRun("acme__web__7__newhead999__cfg", "base3"); got != nil {
		t.Errorf("a different merge base should carry nothing, got %s", got.Key)
	}
}

func TestCarryFromIsOffForForceAndWhenDisabled(t *testing.T) {
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	u := reviewedUnit("a.go:F", "a.go", "+\tx := 1")
	saveRun(t, tr, "acme__web__7__oldhead111__cfg", "base1", "oldhead111", time.Hour, u)
	ctx, on := context.Background(), options{incremental: true}

	if p := tr.carryFrom(ctx, "acme__web__7__newhead999__cfg", "base1", on, false); p == nil {
		t.Fatal("want a plan from the earlier run")
	}
	if p := tr.carryFrom(ctx, "acme__web__7__newhead999__cfg", "base1", on, true); p != nil {
		t.Error("-force asks for a re-run, so it should re-review everything")
	}
	if p := tr.carryFrom(ctx, "acme__web__7__newhead999__cfg", "base1", options{}, false); p != nil {
		t.Error("carry-over turned off should carry nothing")
	}
	if p := tr.carryFrom(ctx, "acme__web__7__newhead999__cfg", "base1", on, false).carryFrom(); p == nil {
		t.Error("a plan should hand the pipeline a hook")
	}
	if (*carryPlan)(nil).carryFrom() != nil {
		t.Error("no plan means no hook")
	}
}

func TestCarriedThreadsDropDuplicateLinksOnReReviewedUnits(t *testing.T) {
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	dup := 0
	kept := reviewedUnit("a.go:F", "a.go", "+\tx := 1")
	kept.Threads = []triage.Thread{{ID: "T1", Status: triage.ThreadValid, DuplicateOf: &dup}}
	redone := reviewedUnit("b.go:G", "b.go", "+\ty := 2")
	redone.Threads = []triage.Thread{{ID: "T2", Status: triage.ThreadValid, DuplicateOf: &dup}}
	saveRun(t, tr, "acme__web__7__oldhead111__cfg", "base1", "oldhead111", time.Hour, kept, redone)

	plan := tr.carryFrom(context.Background(), "acme__web__7__newhead999__cfg", "base1", options{incremental: true}, false)
	// As the run ends: one unit kept its review, the other was redone.
	fresh := []*triage.Unit{
		{ID: "a.go:F", File: "a.go", CarriedFrom: "oldhead111"},
		{ID: "b.go:G", File: "b.go"},
	}
	by := map[string]*int{}
	for _, th := range plan.threads(fresh) {
		by[th.ID] = th.DuplicateOf
	}
	if by["T1"] == nil {
		t.Error("a unit that kept its review kept its issues, so the duplicate link still points at one")
	}
	if by["T2"] != nil {
		t.Error("a unit reviewed again has different issues, so the carried index is stale and must go")
	}
	if (*carryPlan)(nil).threads(fresh) != nil {
		t.Error("no plan means no threads")
	}
}

// A result from another prompt version is not reused for the same head;
// one from the current version is, whatever model made it.
func TestLatestCachedSkipsOlderPromptVersions(t *testing.T) {
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ref := triage.PRRef{Owner: "acme", Repo: "web", Number: 7}
	saveRun(t, tr, "acme__web__7__head000000__old", "base", "head000000", time.Minute)
	if _, err := tr.latestCached(ref, "head000000", ""); err == nil {
		t.Fatal("a result from before prompt versions was reused")
	}
	cur := saveRun(t, tr, "acme__web__7__head000000__cur", "base", "head000000", time.Hour)
	cur.PromptVersion, cur.Classifier = triage.PromptVersion, "codex/gpt"
	if err := tr.saveResult(cur); err != nil {
		t.Fatal(err)
	}
	got, err := tr.latestCached(ref, "head000000", "")
	if err != nil || got.Key != cur.Key {
		t.Fatalf("latestCached = %v, %v; want %s", got, err, cur.Key)
	}

	j := &job{Started: time.Now()}
	j.markCached(got)
	j.noteModel(got, "claude-code/sonnet", "")
	if j.CachedBy != "codex/gpt" || j.RunsWith != "claude-code/sonnet" {
		t.Errorf("job says cached by %q, runs with %q", j.CachedBy, j.RunsWith)
	}
	j = &job{Started: time.Now()}
	j.markCached(got)
	j.noteModel(got, "codex/gpt", "")
	if j.CachedBy != "" || j.RunsWith != "" {
		t.Errorf("same model named as different: %q, %q", j.CachedBy, j.RunsWith)
	}
}
