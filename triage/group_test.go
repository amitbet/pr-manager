package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/llm"
)

// unit builds a unit whose diff is the given added lines.
func unit(id, file, symbol string, lines ...string) *Unit {
	h := Hunk{Header: "@@ -1,1 +1,1 @@ " + symbol, OldStart: 1, NewStart: 1}
	for _, l := range lines {
		h.Lines = append(h.Lines, "+"+l)
	}
	return &Unit{ID: id, File: file, Symbol: symbol, Status: StatusModified, Hunks: []Hunk{h}}
}

func groupIDs(gs []*ReviewGroup) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = g.ID()
	}
	return out
}

func TestBuildReviewGroupsLinksCallerCalleeAndTest(t *testing.T) {
	units := []*Unit{
		unit("a.go:retryCaps", "a.go", "retryCaps", "func retryCaps(n int) int {", "return n"),
		unit("b.go:(*C).Do", "b.go", "(*C).Do", "func (c *C) Do() {", "retryCaps(3)"),
		unit("a_test.go:TestRetryCaps", "a_test.go", "TestRetryCaps", "func TestRetryCaps(t *testing.T) {"),
		unit("z.go:unrelated", "z.go", "unrelated", "func unrelated() {", "println(1)"),
	}
	gs := BuildReviewGroups(units, GroupPolicy{MaxChars: DefaultGroupChars})
	if len(gs) != 2 {
		t.Fatalf("groups = %v, want 2", groupIDs(gs))
	}
	first := gs[0].ID()
	for _, want := range []string{"a.go:retryCaps", "b.go:(*C).Do", "a_test.go:TestRetryCaps"} {
		if !strings.Contains(first, want) {
			t.Errorf("group %q lacks %q", first, want)
		}
	}
	if gs[1].ID() != "z.go:unrelated" {
		t.Errorf("second group = %q, want the unrelated unit alone", gs[1].ID())
	}
}

func TestBuildReviewGroupsRespectsMaxChars(t *testing.T) {
	big := strings.Repeat("x", 400)
	units := []*Unit{
		unit("a.go:retryCaps", "a.go", "retryCaps", big, big),
		unit("b.go:(*C).Do", "b.go", "(*C).Do", "retryCaps(3)", big),
	}
	// The two are linked, but together they exceed the cap.
	if gs := BuildReviewGroups(units, GroupPolicy{MaxChars: 900}); len(gs) != 2 {
		t.Errorf("groups = %v, want each unit on its own under a tight cap", groupIDs(gs))
	}
	if gs := BuildReviewGroups(units, GroupPolicy{MaxChars: DefaultGroupChars}); len(gs) != 1 {
		t.Errorf("groups = %v, want one group under the default cap", groupIDs(gs))
	}
}

func TestBuildReviewGroupsKeepsPROrderAndIsStable(t *testing.T) {
	units := []*Unit{
		unit("a.go:alpha", "a.go", "alpha", "func alpha() {", "beta()"),
		unit("z.go:zeta", "z.go", "zeta", "func zeta() {"),
		unit("b.go:beta", "b.go", "beta", "func beta() {"),
	}
	first := groupIDs(BuildReviewGroups(units, GroupPolicy{MaxChars: DefaultGroupChars}))
	if first[0] != "a.go:alpha + b.go:beta" {
		t.Errorf("group = %q, want members in PR order", first[0])
	}
	for range 5 {
		if got := groupIDs(BuildReviewGroups(units, GroupPolicy{MaxChars: DefaultGroupChars})); !equal(got, first) {
			t.Fatalf("grouping is not stable: %v then %v", first, got)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReviewGroupsOffIsOneGroupPerUnit(t *testing.T) {
	a := unit("a.go:retryCaps", "a.go", "retryCaps", "func retryCaps() {")
	b := unit("b.go:(*C).Do", "b.go", "(*C).Do", "retryCaps()")
	off := reviewGroups([]*Unit{a, b}, GroupPolicy{Enabled: false})
	if len(off) != 2 || off[0].ID() != a.ID {
		t.Errorf("grouping off = %v, want one group per unit in order", groupIDs(off))
	}
}

// groupReplyLLM answers a grouped review with one entry per id it was given,
// dropping the ids in omit so the per-unit fallback can be observed. Every
// answer places its unit in skim.
type groupReplyLLM struct {
	omit  map[string]bool
	calls []string
}

func (f *groupReplyLLM) Name() string    { return "fake" }
func (f *groupReplyLLM) ModelID() string { return "fake" }

func (f *groupReplyLLM) Call(_ context.Context, q llm.LLMRequest) (*llm.LLMResponse, error) {
	prompt := q.Messages[len(q.Messages)-1].Content
	name := q.Tools[0].Name
	f.calls = append(f.calls, name)
	triage := map[string]any{"bucket": "skim", "change_kind": "refactor", "confidence": 0.9, "reason": "r"}
	if name != "submit_group_analysis" {
		return toolResp(name, with(triage, map[string]any{
			"headline": "solo", "summary": "solo review", "focus": []any{}, "issues": []any{},
		})), nil
	}
	var units []any
	for _, id := range idsIn(prompt) {
		if f.omit[id] {
			continue
		}
		units = append(units, with(triage, map[string]any{
			"id": id, "headline": "h " + id, "summary": "s " + id,
			"focus": []any{"check " + id}, "issues": []any{},
		}))
	}
	return toolResp(name, map[string]any{"units": units}), nil
}

// with is m with the entries of base it does not set itself.
func with(base, m map[string]any) map[string]any {
	for k, v := range base {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	return m
}

func idsIn(prompt string) []string {
	var out []string
	for _, line := range strings.Split(prompt, "\n") {
		if id, ok := strings.CutPrefix(line, "#### unit id: "); ok {
			out = append(out, id)
		}
	}
	return out
}

func TestAnalyzeGroupWritesEveryMember(t *testing.T) {
	a := unit("a.go:retryCaps", "a.go", "retryCaps", "func retryCaps() {")
	b := unit("b.go:(*C).Do", "b.go", "(*C).Do", "retryCaps()")
	f := &groupReplyLLM{}
	s := &Summarizer{LLM: f, Critic: f, Policy: DefaultPolicy()}
	s.AnalyzeGroup(context.Background(), &ReviewGroup{Members: []*Unit{a, b}})

	if len(f.calls) != 1 {
		t.Errorf("calls = %v, want one call for the group", f.calls)
	}
	for _, u := range []*Unit{a, b} {
		if u.Summary != "s "+u.ID || u.Headline != "h "+u.ID {
			t.Errorf("%s: summary = %q, headline = %q", u.ID, u.Summary, u.Headline)
		}
		if !u.Reviewed {
			t.Errorf("%s: not marked reviewed", u.ID)
		}
		if len(u.Focus) != 1 || u.Focus[0] != "check "+u.ID {
			t.Errorf("%s: focus = %v", u.ID, u.Focus)
		}
		if d := u.Decision; d.Bucket != BucketSkim || d.ChangeKind != "refactor" || d.Source != "fake/fake" {
			t.Errorf("%s: decision = %+v, want the reviewer's skim", u.ID, d)
		}
	}
}

func TestAnalyzeGroupFallsBackForOmittedMember(t *testing.T) {
	a := unit("a.go:retryCaps", "a.go", "retryCaps", "func retryCaps() {")
	b := unit("b.go:(*C).Do", "b.go", "(*C).Do", "retryCaps()")
	f := &groupReplyLLM{omit: map[string]bool{b.ID: true}}
	s := &Summarizer{LLM: f, Critic: f, Policy: DefaultPolicy()}
	s.AnalyzeGroup(context.Background(), &ReviewGroup{Members: []*Unit{a, b}})

	if a.Summary != "s "+a.ID {
		t.Errorf("a summary = %q", a.Summary)
	}
	// The member the reply skipped is reviewed on its own instead of
	// being left without notes.
	if b.Summary != "solo review" {
		t.Errorf("b summary = %q, want the per-unit fallback", b.Summary)
	}
	if len(f.calls) != 2 {
		t.Errorf("calls = %v, want the group call plus one fallback", f.calls)
	}
}

func TestAnalyzeGroupOfOneIsAnOrdinaryReview(t *testing.T) {
	a := unit("a.go:retryCaps", "a.go", "retryCaps", "func retryCaps() {")
	f := &groupReplyLLM{}
	s := &Summarizer{LLM: f, Critic: f, Policy: DefaultPolicy()}
	s.AnalyzeGroup(context.Background(), &ReviewGroup{Members: []*Unit{a}})
	if len(f.calls) != 1 || f.calls[0] != "submit_analysis" {
		t.Errorf("calls = %v, want the ungrouped review tool", f.calls)
	}
}

func TestGroupToolSchemaRequiresID(t *testing.T) {
	b, err := json.Marshal(groupAnalyzeTool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"units"`, `"id"`, `"headline"`, `"issues"`, `"bucket"`, `"confidence"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("group schema lacks %s: %s", want, b)
		}
	}
}

func TestGroupingPolicyDefaultsOnAndParsesOff(t *testing.T) {
	if p := DefaultPolicy(); !p.Grouping.Enabled || p.Grouping.MaxChars != DefaultGroupChars {
		t.Errorf("default grouping = %+v, want on at %d chars", p.Grouping, DefaultGroupChars)
	}
	// An unrelated policy file must leave grouping on.
	p, err := ParsePolicy([]byte("max_unit_chars: 100\n"))
	if err != nil || !p.Grouping.Enabled {
		t.Errorf("grouping = %+v, err = %v, want still on", p.Grouping, err)
	}
	p, err = ParsePolicy([]byte("grouping:\n  enabled: false\n  max_chars: 5000\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Grouping.Enabled || p.Grouping.MaxChars != 5000 {
		t.Errorf("grouping = %+v, want off at 5000", p.Grouping)
	}
	if _, err := ParsePolicy([]byte("grouping:\n  max_chars: 0\n")); err == nil {
		t.Error("max_chars: 0 should be rejected")
	}
}

// TestPipelineGroupingReviewsEveryUnitInFewerCalls runs the real pipeline
// over a fixture twice, with grouping on and off, and compares.
func TestPipelineGroupingReviewsEveryUnitInFewerCalls(t *testing.T) {
	run := func(enabled bool) (calls int, reviewed []string) {
		_, src := errorCacheCase(t)
		review := &groupReplyLLM{}
		policy := DefaultPolicy()
		policy.Grouping.Enabled = enabled
		p := &Pipeline{
			Presorter:   &Presorter{Policy: policy},
			Summarizer:  &Summarizer{LLM: review, Critic: review, Policy: policy},
			Concurrency: 1,
		}
		for _, u := range p.Run(context.Background(), src) {
			if u.Reviewed {
				reviewed = append(reviewed, u.ID)
			}
		}
		return len(review.calls), reviewed
	}

	offCalls, offUnits := run(false)
	onCalls, onUnits := run(true)

	if len(offUnits) == 0 {
		t.Fatal("fixture reviewed no units")
	}
	if !equal(onUnits, offUnits) {
		t.Errorf("grouping changed which units were reviewed:\n on: %v\noff: %v", onUnits, offUnits)
	}
	if onCalls >= offCalls {
		t.Errorf("review calls: grouped %d, ungrouped %d; grouping should need fewer", onCalls, offCalls)
	}
	t.Logf("%d units reviewed in %d calls grouped, %d ungrouped", len(onUnits), onCalls, offCalls)
}

func TestAnalyzeGroupKeepsARulesDecision(t *testing.T) {
	a := unit("a.go:retryCaps", "a.go", "retryCaps", "func retryCaps() {")
	b := unit("b.go:(*C).Do", "b.go", "(*C).Do", "retryCaps()")
	rule := Decision{Bucket: BucketHuman, Source: "rule", Reason: "migration"}
	a.Decision = rule
	f := &groupReplyLLM{}
	s := &Summarizer{LLM: f, Critic: f, Policy: DefaultPolicy()}
	s.AnalyzeGroup(context.Background(), &ReviewGroup{Members: []*Unit{a, b}})

	if len(f.calls) != 1 {
		t.Errorf("calls = %v, want one call for the group", f.calls)
	}
	if a.Decision.Source != "rule" || a.Decision.Bucket != BucketHuman || !a.Reviewed {
		t.Errorf("rule unit: decision = %+v, reviewed = %v, want the rule's bucket kept", a.Decision, a.Reviewed)
	}
	if b.Decision.Bucket != BucketSkim || !b.Reviewed {
		t.Errorf("other unit: decision = %+v, want the reviewer's", b.Decision)
	}
}

func TestAnalyzeFailureLeavesUnitHuman(t *testing.T) {
	a := unit("a.go:retryCaps", "a.go", "retryCaps", "func retryCaps() {")
	f := &fakeLLM{fn: func(llm.LLMRequest) (*llm.LLMResponse, error) { return nil, errors.New("boom") }}
	s := &Summarizer{LLM: f, Critic: f, Policy: DefaultPolicy()}
	s.Analyze(context.Background(), a)
	if d := a.Decision; d.Bucket != BucketHuman || d.Confidence != 0 || !d.Failed || a.Reviewed {
		t.Errorf("decision = %+v, reviewed = %v, want a failed human", d, a.Reviewed)
	}
}

// A CLI reply is parsed out of free text with no schema behind it, so an
// issues field that is not a list of issues is a failed review, never a
// clean one.
func TestAnalyzeMalformedIssuesIsAFailedReview(t *testing.T) {
	triage := map[string]any{"bucket": "skim", "change_kind": "refactor", "confidence": 0.9, "reason": "r", "headline": "h", "summary": "s"}
	for name, issues := range map[string]any{"null": nil, "string": "none", "object": map[string]any{}, "unreadable entries": []any{"nil deref", 3}} {
		a := unit("a.go:retryCaps", "a.go", "retryCaps", "func retryCaps() {")
		f := &fakeLLM{fn: func(q llm.LLMRequest) (*llm.LLMResponse, error) {
			return toolResp(q.Tools[0].Name, with(triage, map[string]any{"issues": issues})), nil
		}}
		s := &Summarizer{LLM: f, Critic: f, Policy: DefaultPolicy()}
		s.Analyze(context.Background(), a)
		if d := a.Decision; a.Reviewed || d.Bucket != BucketHuman || !d.Failed {
			t.Errorf("%s: decision = %+v, reviewed = %v, want a failed human", name, d, a.Reviewed)
		}
	}
	// An empty list is a review that found nothing.
	a := unit("a.go:retryCaps", "a.go", "retryCaps", "func retryCaps() {")
	f := &fakeLLM{fn: func(q llm.LLMRequest) (*llm.LLMResponse, error) {
		return toolResp(q.Tools[0].Name, with(triage, map[string]any{"issues": []any{}})), nil
	}}
	(&Summarizer{LLM: f, Critic: f, Policy: DefaultPolicy()}).Analyze(context.Background(), a)
	if !a.Reviewed || a.Decision.Failed {
		t.Errorf("empty list: decision = %+v, reviewed = %v, want reviewed", a.Decision, a.Reviewed)
	}
}

func TestAnalyzeGroupMalformedIssuesFailsThatMember(t *testing.T) {
	a := unit("a.go:retryCaps", "a.go", "retryCaps", "func retryCaps() { maxRetries() }")
	b := unit("b.go:maxRetries", "b.go", "maxRetries", "func maxRetries() int {")
	triage := map[string]any{"bucket": "skim", "change_kind": "refactor", "confidence": 0.9, "reason": "r", "headline": "h", "summary": "s", "focus": []any{}}
	f := &fakeLLM{fn: func(q llm.LLMRequest) (*llm.LLMResponse, error) {
		return toolResp(q.Tools[0].Name, map[string]any{"units": []any{
			with(triage, map[string]any{"id": a.ID, "issues": nil}),
			with(triage, map[string]any{"id": b.ID, "issues": []any{}}),
		}}), nil
	}}
	s := &Summarizer{LLM: f, Critic: f, Policy: DefaultPolicy()}
	s.AnalyzeGroup(context.Background(), &ReviewGroup{Members: []*Unit{a, b}})
	if a.Reviewed || !a.Decision.Failed || a.Decision.Bucket != BucketHuman {
		t.Errorf("null issues: decision = %+v, reviewed = %v, want a failed human", a.Decision, a.Reviewed)
	}
	if !b.Reviewed || b.Decision.Failed {
		t.Errorf("empty issues: decision = %+v, reviewed = %v, want reviewed", b.Decision, b.Reviewed)
	}
}

func TestGroupingMemberCapBoundsGroupSize(t *testing.T) {
	// A chain of pairwise links: each unit names the next. Single-linkage
	// would pull all of them into one group without a member cap.
	var units []*Unit
	for i := range 12 {
		units = append(units, unit(
			fmt.Sprintf("a.go:fn%02d", i), "a.go", fmt.Sprintf("fn%02d", i),
			fmt.Sprintf("func fn%02d() {", i), fmt.Sprintf("fn%02d()", i+1)))
	}
	uncapped := BuildReviewGroups(units, GroupPolicy{MaxChars: 1 << 20})
	big := 0
	for _, g := range uncapped {
		big = max(big, len(g.Members))
	}
	if big < 12 {
		t.Fatalf("uncapped largest group = %d, want the whole chain", big)
	}
	capped := BuildReviewGroups(units, GroupPolicy{MaxChars: 1 << 20, MaxMembers: 4})
	for _, g := range capped {
		if len(g.Members) > 4 {
			t.Errorf("group %q has %d members, over the cap", g.ID(), len(g.Members))
		}
	}
}

func TestGroupingPolicyDefaultsIncludeMemberCap(t *testing.T) {
	if p := DefaultPolicy(); p.Grouping.MaxMembers != DefaultGroupMembers {
		t.Errorf("default max_members = %d, want %d", p.Grouping.MaxMembers, DefaultGroupMembers)
	}
	p, err := ParsePolicy([]byte("grouping:\n  max_members: 0\n"))
	if err != nil || p.Grouping.MaxMembers != 0 {
		t.Errorf("max_members: 0 (no cap) = %+v, err = %v", p.Grouping, err)
	}
	if _, err := ParsePolicy([]byte("grouping:\n  max_members: -1\n")); err == nil {
		t.Error("negative max_members should be rejected")
	}
}
