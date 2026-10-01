package main

import (
	"testing"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

func TestResolvedSinceAnEarlierReview(t *testing.T) {
	hunk := func(l string) []triage.Hunk { return []triage.Hunk{{Lines: []string{l}}} }
	unit := func(id string, reviewed bool, carried string, lines string, issues ...triage.Issue) resultFile {
		return resultFile{FileDiff: triage.FileDiff{Path: id}, Units: []resultUnit{{Unit: &triage.Unit{ID: id, File: id, Reviewed: reviewed, CarriedFrom: carried, Issues: issues}, Hunks: hunk(lines)}}}
	}
	fixed := triage.Issue{Severity: "high", Title: "Leaks the response body", Evidence: "http.Get(u)"}
	reworded := triage.Issue{Severity: "medium", Title: "Response body leaks on error", Evidence: "resp, err := http.Get(u)"}
	keptOld := triage.Issue{Severity: "low", Title: "Kept by a carried review", Evidence: "x"}
	gone := triage.Issue{Severity: "medium", Title: "In a file the PR dropped", Evidence: "y"}
	skipped := triage.Issue{Severity: "low", Title: "Unit not reviewed this time", Evidence: "z"}
	earlier := resolvedIssue{UnitID: "c.go", File: "c.go", Issue: triage.Issue{Severity: "low", Title: "Fixed two pushes ago", Evidence: "w"}, Since: "old"}
	prev := &PRResult{Key: "acme__web__7__aaaa__s1", CreatedAt: time.Now(), PR: &triage.PRInfo{HeadOid: "aaaa"},
		Files:    []resultFile{unit("a.go", true, "", "+a", fixed, reworded), unit("b.go", true, "", "+b", keptOld), unit("d.go", true, "", "+d", gone), unit("e.go", true, "", "+e", skipped)},
		Resolved: []resolvedIssue{earlier}}
	r := &PRResult{Key: "acme__web__7__bbbb__s1", PR: &triage.PRInfo{HeadOid: "bbbb"},
		Files: []resultFile{
			// a.go was reviewed afresh: fixed is not raised again, and the
			// reworded claim about the same code is.
			unit("a.go", true, "", "+a2", triage.Issue{Severity: "medium", Title: "Body of a failed response is never closed", Evidence: "resp, err := http.Get(u)"}),
			unit("b.go", true, "aaaa", "+b", keptOld),
			unit("c.go", true, "", "+c"),
			unit("e.go", false, "", "+e2"),
		}}
	got := resolvedSince(prev, r)
	var titles []string
	for _, x := range got {
		titles = append(titles, x.Issue.Title)
		if x.Issue.Title == gone.Title && !x.Gone {
			t.Errorf("%q should be gone with its unit", x.Issue.Title)
		}
		if x.Issue.Title == fixed.Title && x.Since != "aaaa" {
			t.Errorf("since = %q", x.Since)
		}
	}
	want := map[string]bool{fixed.Title: true, earlier.Issue.Title: true, gone.Title: true}
	if len(got) != len(want) {
		t.Fatalf("resolved = %q", titles)
	}
	for _, s := range titles {
		if !want[s] {
			t.Errorf("resolved %q", s)
		}
	}

	// Other settings: a unit whose diff did not move doesn't count.
	r.Key = "acme__web__7__bbbb__s2"
	r.Files[0].Units[0].Hunks = hunk("+a")
	for _, x := range resolvedSince(prev, r) {
		if x.Issue.Title == fixed.Title {
			t.Errorf("same diff under other settings counted as fixed")
		}
	}
}
