package main

import (
	"context"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

func TestParseActions(t *testing.T) {
	got := parseActions([]any{
		map[string]any{"name": "fix", "args": `{"targets":[{"unit":"a.go","issue":0}]}`, "why": "the reader asked"},
		map[string]any{"name": "rm_rf", "args": "{}", "why": "not in the catalog"},
		map[string]any{"name": "open_unit", "args": "not json", "why": "bad arguments"},
		map[string]any{"name": "refresh_comments", "args": "", "why": "no arguments"},
	})
	if len(got) != 2 || got[0].Name != "fix" || got[1].Name != "refresh_comments" {
		t.Fatalf("got %+v", got)
	}
	if ts, _ := got[0].Args["targets"].([]any); len(ts) != 1 {
		t.Errorf("fix args %+v", got[0].Args)
	}
}

// A re-review keeps every unit's review but the ones asked for, and any
// whose diff moved.
func TestRereviewPlan(t *testing.T) {
	hunk := func(l string) []triage.Hunk { return []triage.Hunk{{NewStart: 1, NewLines: 1, Lines: []string{l}}} }
	unit := func(id string) *triage.Unit { return &triage.Unit{ID: id, File: id, Reviewed: true} }
	cur := &PRResult{Key: "k", PR: &triage.PRInfo{HeadOid: "abc"}, Files: []resultFile{{Units: []resultUnit{
		{Unit: unit("a.go"), Hunks: hunk("+a")}, {Unit: unit("b.go"), Hunks: hunk("+b")}, {Unit: unit("c.go"), Hunks: hunk("+c")},
	}}}}
	fresh := []*triage.Unit{{ID: "a.go", File: "a.go", Hunks: hunk("+a")}, {ID: "b.go", File: "b.go", Hunks: hunk("+b")}, {ID: "c.go", File: "c.go", Hunks: hunk("+c changed")}}
	c := rereviewPlan(cur, []string{"b.go"}).carryFrom()(fresh)
	if c.Reuse["a.go"] == nil || c.Reuse["b.go"] != nil || c.Reuse["c.go"] != nil {
		t.Errorf("reuse %v", c.Reuse)
	}
	if c.Why["b.go"] != "asked to review again" || c.Why["c.go"] != "its own diff changed" {
		t.Errorf("why %v", c.Why)
	}
}

func TestReanalyzeAndThreadChecks(t *testing.T) {
	tr, err := newTriager(options{cache: t.TempDir(), reviewDryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	u := &triage.Unit{ID: "a.go", File: "a.go", Threads: []triage.Thread{{ID: "T1", URL: "https://github.com/acme/web/pull/7#r1"}}}
	r := &PRResult{Key: "k", PR: &triage.PRInfo{URL: "https://github.com/acme/web/pull/7"}, Files: []resultFile{{Units: []resultUnit{{Unit: u}}}}}
	if _, err := tr.reanalyze(r, reanalyzeRequest{What: "units", Units: []string{"nope.go"}}); err == nil || !strings.Contains(err.Error(), "nope.go") {
		t.Errorf("an unknown unit: %v", err)
	}
	if _, err := tr.reanalyze(r, reanalyzeRequest{What: "everything"}); err == nil {
		t.Error("an unknown part was analyzed")
	}
	fixed := *r
	fixed.LocalFixDir = "/tmp/fix"
	if _, err := tr.reanalyze(&fixed, reanalyzeRequest{What: "all"}); err == nil {
		t.Error("a fix's result was re-triaged as the PR")
	}
	if _, err := tr.threadAction(context.Background(), r, "reply", "T9", "hi"); err == nil {
		t.Error("replied on a thread the PR doesn't have")
	}
	if _, err := tr.threadAction(context.Background(), r, "reply", "T1", " "); err == nil {
		t.Error("posted an empty reply")
	}
	out, err := tr.threadAction(context.Background(), r, "resolve", "T1", "")
	if err != nil || out["dry_run"] != true {
		t.Errorf("dry run: %v %v", out, err)
	}
}

// The agent is told about the places of a result that exist, and may
// read them but not write to them or to the cache.
func TestChatPlaces(t *testing.T) {
	cache := t.TempDir()
	tr, err := newTriager(options{cache: cache, codemapDir: "off"})
	if err != nil {
		t.Fatal(err)
	}
	r := &PRResult{Key: "k1", PR: &triage.PRInfo{LocalPath: t.TempDir()}, LocalFixDir: "/nowhere"}
	places := tr.chatPlaces(r, r.PR.LocalPath)
	text := chatPlacesText(places)
	for _, want := range []string{r.PR.LocalPath, tr.results, "k1.json"} {
		if !strings.Contains(text, want) {
			t.Errorf("places lack %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "/nowhere") {
		t.Error("a missing directory was listed")
	}
	ws := &llm.Workspace{Dir: r.PR.LocalPath}
	shellWorkspace(ws, places, cache)
	if !ws.Shell || len(ws.NoWrite) != 1 || ws.NoWrite[0] != cache {
		t.Errorf("workspace %+v", ws)
	}
	for _, d := range ws.ReadDirs {
		if d == ws.Dir {
			t.Error("the working directory is also a read directory")
		}
	}
}
