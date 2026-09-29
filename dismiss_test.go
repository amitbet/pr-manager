package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/triage"
)

// dismissFixture is a saved result with one unit that a medium issue and
// one lint finding put in human review: the shape the Issues tab works on.
func dismissFixture(t *testing.T) (*triager, *http.ServeMux, *PRResult) {
	t.Helper()
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tp := triage.DefaultTierPolicy()
	u := &triage.Unit{
		ID: "a.go:Fetch", File: "a.go", Symbol: "Fetch",
		Decision:   triage.Decision{Bucket: triage.BucketSkim, Source: "llm", Confidence: 1, ChangeKind: "behavior"},
		Impact:     &triage.Impact{Score: 40, Level: "medium"},
		Likelihood: &triage.Likelihood{Score: 40, Level: "medium"},
		Hunks:      []triage.Hunk{{NewStart: 10, Lines: []string{"+\tresp, _ := http.Get(u)"}}},
	}
	u.Reviewed = true
	u.Issues = []triage.Issue{{
		Severity: "medium", Line: 10, Title: "Response body is never closed",
		Evidence: "resp, _ := http.Get(u)", Scenario: "every call leaks a connection",
	}}
	u.Lint = []triage.LintFinding{{Tool: "golangci-lint", Rule: "errcheck", Severity: "error", Path: "a.go", Line: 10, Message: "Error return value is not checked"}}
	// Rescore runs the prior and, because the unit is marked reviewed,
	// what the review decided: the same path a cached result takes.
	triage.Rescore([]*triage.Unit{u}, tp)

	r := &PRResult{
		Key: "acme__web__7__abc", PR: &triage.PRInfo{PRRef: triage.PRRef{Owner: "acme", Repo: "web", Number: 7}},
		Files:   []resultFile{{FileDiff: triage.FileDiff{Path: "a.go"}, Units: []resultUnit{{Unit: u, Hunks: u.Hunks}}}},
		Budgets: tp.OrderedBudgets(), ReviewBudget: "balanced",
	}
	b, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(tr.results, r.Key+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	tr.dismissed.routes(mux, tr)
	return tr, mux, r
}

func do(t *testing.T, mux *http.ServeMux, method, path, body string) *PRResult {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
	}
	var out PRResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return &out
}

func unitOf(r *PRResult) *triage.Unit { return r.Files[0].Units[0].Unit }

func TestDismissAndRestoreOverHTTP(t *testing.T) {
	tr, mux, r := dismissFixture(t)
	base := "/api/results/" + r.Key + "/dismissals"

	loaded, err := tr.Load(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	if unitOf(loaded).Decision.Bucket != triage.BucketHuman {
		t.Fatalf("setup: want the unit in human review, got %s", unitOf(loaded).Decision.Bucket)
	}

	after := do(t, mux, "POST", base, `{"unit":"a.go:Fetch","kind":"issue","index":0,"reason":"the helper closes it"}`)
	is := unitOf(after).Issues[0]
	if !is.Dismissed || is.DismissedWhy != "the helper closes it" || is.DismissKey == "" {
		t.Fatalf("issue not marked dismissed: %+v", is)
	}
	if b := unitOf(after).Decision.Bucket; b == triage.BucketHuman {
		t.Errorf("the unit is still in human review on a dismissed issue: %s", unitOf(after).Score.Why)
	}
	if unitOf(after).Attention != 0 {
		t.Errorf("attention %d, want 0", unitOf(after).Attention)
	}

	// It survives a reload: the record is the repository's, not the page's.
	again, err := tr.Load(r.Key)
	if err != nil || !unitOf(again).Issues[0].Dismissed {
		t.Fatalf("dismissal did not survive a reload: %v", err)
	}

	back := do(t, mux, "DELETE", base+"/"+is.DismissKey, "")
	if unitOf(back).Issues[0].Dismissed || unitOf(back).Decision.Bucket != triage.BucketHuman {
		t.Errorf("restore left the issue at %+v, bucket %s", unitOf(back).Issues[0], unitOf(back).Decision.Bucket)
	}
}

func TestDismissLintFinding(t *testing.T) {
	_, mux, r := dismissFixture(t)
	base := "/api/results/" + r.Key + "/dismissals"
	after := do(t, mux, "POST", base, `{"unit":"a.go:Fetch","kind":"lint","index":0,"reason":"the error cannot happen here"}`)
	if f := unitOf(after).Lint[0]; !f.Dismissed || f.DismissKey == "" {
		t.Fatalf("lint finding not dismissed: %+v", f)
	}
	// The issue is untouched, so the unit stays where the review put it.
	if unitOf(after).Decision.Bucket != triage.BucketHuman {
		t.Error("dismissing a lint finding should not move a unit a review issue pinned")
	}
}

func TestDismissRejectsUnknownTargets(t *testing.T) {
	_, mux, r := dismissFixture(t)
	base := "/api/results/" + r.Key + "/dismissals"
	for _, body := range []string{
		`{"unit":"nope","kind":"issue","index":0}`,
		`{"unit":"a.go:Fetch","kind":"issue","index":9}`,
		`{"unit":"a.go:Fetch","kind":"weird","index":0}`,
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", base, strings.NewReader(body)))
		if rec.Code != 400 {
			t.Errorf("%s: got %d, want 400", body, rec.Code)
		}
	}
}

func TestRepoKeyRejectsPathTricks(t *testing.T) {
	if k := repoKey(triage.PRRef{Owner: "..", Repo: "x/y"}); k != "" && strings.ContainsAny(k, `/\`) {
		t.Errorf("repoKey let a path separator through: %q", k)
	}
	if got := repoKey(triage.PRRef{Host: "ghe.example.com", Owner: "acme", Repo: "web"}); got != "ghe.example.com__acme__web" {
		t.Errorf("repoKey = %q", got)
	}
	if got := repoKey(triage.PRRef{Owner: "acme", Repo: "web"}); got != "acme__web" {
		t.Errorf("repoKey = %q", got)
	}
	a := repoKey(triage.PRRef{Host: "gitlab.com", Owner: "g/sub", Repo: "a"})
	b := repoKey(triage.PRRef{Host: "gitlab.com", Owner: "g/sub", Repo: "b"})
	if a == b || strings.ContainsAny(a+b, `/\`) {
		t.Errorf("subgroup repos share or escape their file: %q %q", a, b)
	}
}
