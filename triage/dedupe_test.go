package triage

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/llm"
)

var findingLine = regexp.MustCompile(`(?m)^- (f\d+) \[[^\]]*\] [^:]*: (.*)$`)

// sameWhen answers the dedupe call with one set: every finding whose
// title contains word. calls counts the calls made.
func sameWhen(word string, calls *int) *fakeLLM {
	return &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		*calls++
		var ids []any
		for _, m := range findingLine.FindAllStringSubmatch(req.Messages[len(req.Messages)-1].Content, -1) {
			if strings.Contains(strings.ToLower(m[2]), word) {
				ids = append(ids, m[1])
			}
		}
		return toolResp("merge_duplicates", map[string]any{"sets": []any{map[string]any{"ids": ids}}}), nil
	}}
}

func dedupeUnits() []*Unit {
	score := func() *Score { return &Score{Base: 30, Kind: 1, Prior: 30} }
	a := &Unit{ID: "a.go:Register", File: "a.go", Reviewed: true, Score: score(), Issues: []Issue{
		{Severity: "high", Line: 10, Title: "Race on the global callback registry"},
	}}
	b := &Unit{ID: "b.go:Fire", File: "b.go", Reviewed: true, Score: score(), Issues: []Issue{
		{Severity: "medium", Line: 40, Title: "Global callback registry race when firing"},
		{Severity: "low", Line: 90, Title: "Unused parameter in helper"},
	}}
	c := &Unit{ID: "c.go:Load", File: "c.go", Reviewed: true, Score: score(), Issues: []Issue{
		{Severity: "medium", Line: 5, Title: "Config file read without size limit"},
	}}
	b.Threads = []Thread{{ID: "T1", Path: "b.go", Line: 41, Author: "alice", Status: ThreadValid,
		Issue: &Issue{Severity: "medium", Line: 41, Title: "Firing callbacks races with registry writes"}}}
	return []*Unit{a, b, c}
}

func TestDedupeLinksRepeatsAcrossUnits(t *testing.T) {
	units := dedupeUnits()
	a, b, c := units[0], units[1], units[2]
	calls := 0
	s := &Summarizer{LLM: sameWhen("regist", &calls)}
	s.Dedupe(context.Background(), units, DefaultTierPolicy())

	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if got := b.Issues[0].SameAs; got == nil || got.Unit != a.ID || got.Title != a.Issues[0].Title {
		t.Errorf("b's race issue SameAs = %+v, want a's", got)
	}
	if b.Issues[0].Live() || !a.Issues[0].Live() || !b.Issues[1].Live() {
		t.Errorf("live: a=%v b0=%v b1=%v", a.Issues[0].Live(), b.Issues[0].Live(), b.Issues[1].Live())
	}
	th := b.Threads[0]
	if th.DuplicateOf == nil || *th.DuplicateOf != 0 || th.DuplicateUnit != a.ID || th.DuplicateTitle != a.Issues[0].Title {
		t.Errorf("comment link = %v %q %q", th.DuplicateOf, th.DuplicateUnit, th.DuplicateTitle)
	}
	if c.Issues[0].SameAs != nil {
		t.Errorf("unrelated issue linked: %+v", c.Issues[0].SameAs)
	}
	// b counts only its low issue now: the medium repeat and the comment
	// count on a.
	if b.Attention != severityWeight["low"] {
		t.Errorf("b attention = %d, want %d", b.Attention, severityWeight["low"])
	}
	for _, u := range units {
		for _, is := range u.Issues {
			if !is.Compared && is.SameAs == nil {
				t.Errorf("%s %q not marked compared", u.ID, is.Title)
			}
		}
	}

	// Nothing new: no call.
	s.Dedupe(context.Background(), units, DefaultTierPolicy())
	if calls != 1 {
		t.Errorf("second pass made a call (calls = %d)", calls)
	}
}

// A new, worse repeat becomes the issue that stands, and the old links
// follow it.
func TestDedupeWorseRepeatTakesOver(t *testing.T) {
	units := dedupeUnits()
	a, b := units[0], units[1]
	calls := 0
	s := &Summarizer{LLM: sameWhen("regist", &calls)}
	s.Dedupe(context.Background(), units, DefaultTierPolicy())

	d := &Unit{ID: "d.go:Init", File: "d.go", Reviewed: true, Score: &Score{Base: 30, Kind: 1, Prior: 30}, Issues: []Issue{
		{Severity: "critical", Line: 3, Title: "Callback registry written without a lock"},
	}}
	units = append(units, d)
	s.Dedupe(context.Background(), units, DefaultTierPolicy())
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	want := IssueRef{Unit: d.ID, Title: d.Issues[0].Title}
	for _, got := range []*IssueRef{a.Issues[0].SameAs, b.Issues[0].SameAs} {
		if got == nil || *got != want {
			t.Errorf("SameAs = %+v, want %+v", got, want)
		}
	}
	if th := b.Threads[0]; th.DuplicateUnit != d.ID || *th.DuplicateOf != 0 {
		t.Errorf("comment points at %q %v, want d", th.DuplicateUnit, *th.DuplicateOf)
	}
}

// A link whose issue was not raised again is dropped, and the repeat is
// compared afresh.
func TestDedupeDropsLostLinks(t *testing.T) {
	units := dedupeUnits()
	a, b := units[0], units[1]
	b.Issues[0].SameAs, b.Issues[0].Compared = &IssueRef{Unit: a.ID, Title: "an issue a's new review did not raise"}, true
	resolveSameAs(unitIndex(units))
	if b.Issues[0].SameAs != nil || b.Issues[0].Compared {
		t.Errorf("lost link kept: %+v compared=%v", b.Issues[0].SameAs, b.Issues[0].Compared)
	}
}

func TestDedupeFailedCallComparesNothing(t *testing.T) {
	units := dedupeUnits()
	boom := &fakeLLM{fn: func(llm.LLMRequest) (*llm.LLMResponse, error) { return nil, errors.New("boom") }}
	(&Summarizer{LLM: boom}).Dedupe(context.Background(), units, DefaultTierPolicy())
	for _, u := range units {
		for _, is := range u.Issues {
			if is.Compared || is.SameAs != nil {
				t.Errorf("%s %q: compared=%v same_as=%+v", u.ID, is.Title, is.Compared, is.SameAs)
			}
		}
	}
}

// Comments alone cannot be merged (a comment only repeats an issue), so
// a PR whose look-alikes are all comments makes no call.
func TestDedupeCommentsOnlyNoCall(t *testing.T) {
	u := &Unit{ID: "a.go:f", File: "a.go", Reviewed: true, Threads: []Thread{
		{ID: "T1", Path: "a.go", Line: 3, Status: ThreadValid, Issue: &Issue{Severity: "low", Line: 3, Title: "Missing nil check"}},
		{ID: "T2", Path: "a.go", Line: 4, Status: ThreadValid, Issue: &Issue{Severity: "low", Line: 4, Title: "Missing nil check"}},
	}}
	calls := 0
	(&Summarizer{LLM: sameWhen("nil", &calls)}).Dedupe(context.Background(), []*Unit{u}, DefaultTierPolicy())
	if calls != 0 {
		t.Errorf("calls = %d, want 0", calls)
	}
}

// A comment linked to another unit's issue is found there again after the
// threads are re-placed.
func TestAssignThreadsCrossUnitDuplicate(t *testing.T) {
	units := dedupeUnits()
	a, b := units[0], units[1]
	b.Hunks = []Hunk{{NewStart: 30, NewLines: 20}}
	i := 5 // stale index; the title finds it
	th := b.Threads[0]
	th.DuplicateOf, th.DuplicateUnit, th.DuplicateTitle = &i, a.ID, a.Issues[0].Title
	b.Threads = nil
	AssignThreads(units, []Thread{th})
	got := b.Threads[0]
	if got.DuplicateOf == nil || *got.DuplicateOf != 0 || got.DuplicateUnit != a.ID {
		t.Errorf("link = %v %q", got.DuplicateOf, got.DuplicateUnit)
	}

	th.DuplicateTitle = "gone"
	b.Threads = nil
	AssignThreads(units, []Thread{th})
	if got := b.Threads[0]; got.DuplicateOf != nil || got.DuplicateUnit != "" {
		t.Errorf("lost link kept: %v %q", got.DuplicateOf, got.DuplicateUnit)
	}
}
