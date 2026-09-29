package triage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/llm"
)

// carryUnit is a reviewed unit with a diff, the shape a previous run
// leaves behind.
func carryUnit(id, file, symbol string, lines ...string) *Unit {
	u := &Unit{
		ID: id, File: file, Symbol: symbol, Status: StatusModified,
		Hunks:    []Hunk{{Header: "@@ -1,3 +1,3 @@ " + symbol, NewStart: 1, OldStart: 1, Lines: lines}},
		Decision: Decision{Bucket: BucketSkim, Source: "llm", Confidence: 1, ChangeKind: "behavior"},
		Reviewed: true, Headline: "does a thing", Summary: "a summary of " + id,
	}
	return u
}

// clone is the same unit as a fresh run would build it: same diff, no
// review yet.
func clone(u *Unit) *Unit {
	c := *u
	c.Hunks = append([]Hunk(nil), u.Hunks...)
	c.Reviewed, c.Headline, c.Summary, c.Issues, c.Focus = false, "", "", nil, nil
	return &c
}

func TestCarryOverKeepsUnchangedUnits(t *testing.T) {
	a := carryUnit("a.go:Fetch", "a.go", "Fetch", " ctx := context.TODO()", "+\tresp, err := get(ctx)")
	b := carryUnit("b.go:Store", "b.go", "Store", " func Store() {", "+\twriteRow()")
	prev := []*Unit{a, b}

	// The push touches b only.
	fa, fb := clone(a), clone(b)
	fb.Hunks[0].Lines = []string{" func Store() {", "+\twriteRow(ctx)"}

	c := PlanCarryOver([]*Unit{fa, fb}, prev, "deadbeef")
	if c.Reuse["a.go:Fetch"] != a {
		t.Errorf("an untouched unit in another file should keep its review, why: %q", c.Why["a.go:Fetch"])
	}
	if _, ok := c.Reuse["b.go:Store"]; ok {
		t.Error("the unit the push changed must be reviewed again")
	}
	if c.Why["b.go:Store"] != "its own diff changed" {
		t.Errorf("why = %q", c.Why["b.go:Store"])
	}
}

func TestCarryOverReviewsAgainWhenTheContextMoved(t *testing.T) {
	cases := []struct {
		name       string
		keeper     *Unit
		changer    *Unit
		changeTo   []string
		wantReason string
	}{{
		name:       "same file",
		keeper:     carryUnit("a.go:Fetch", "a.go", "Fetch", "+\tx := 1"),
		changer:    carryUnit("a.go:Store", "a.go", "Store", "+\ty := 2"),
		changeTo:   []string{"+\ty := 3"},
		wantReason: "same file",
	}, {
		name:       "the keeper calls the changed unit",
		keeper:     carryUnit("a.go:Fetch", "a.go", "Fetch", "+\tv := decodeRow(b)"),
		changer:    carryUnit("b.go:decodeRow", "b.go", "decodeRow", "+\treturn nil, err"),
		changeTo:   []string{"+\treturn nil, nil"},
		wantReason: "name each other",
	}, {
		// The keeper was reviewed as "this code moved here from Handle,
		// so its behavior is not new". Handle no longer shows those lines
		// as removed, which is exactly the claim that has to be checked
		// again — even though the keeper's own lines did not move.
		name: "code the keeper took over is no longer shown as removed",
		keeper: carryUnit("a.go:Fetch", "a.go", "Fetch",
			"+\tif err := validateRequestBody(r); err != nil {",
			"+\t\treturn fmt.Errorf(\"bad request body: %w\", err)",
			"+\t}",
			"+\tlog.Printf(\"validated the request body\")"),
		changer: carryUnit("b.go:Handle", "b.go", "Handle",
			"-\tif err := validateRequestBody(r); err != nil {",
			"-\t\treturn fmt.Errorf(\"bad request body: %w\", err)",
			"-\t}",
			"-\tlog.Printf(\"validated the request body\")"),
		changeTo:   []string{"-\tsomething else entirely in this handler"},
		wantReason: "code moved",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := []*Unit{tc.keeper, tc.changer}
			fk, fc := clone(tc.keeper), clone(tc.changer)
			fc.Hunks[0].Lines = tc.changeTo
			c := PlanCarryOver([]*Unit{fk, fc}, prev, "deadbeef")
			if _, ok := c.Reuse[fk.ID]; ok {
				t.Fatalf("%s kept its review although %s changed", fk.ID, fc.ID)
			}
			if !strings.Contains(c.Why[fk.ID], tc.wantReason) {
				t.Errorf("why = %q, want it to mention %q", c.Why[fk.ID], tc.wantReason)
			}
		})
	}
}

func TestCarryOverReviewsAgainWhenAUnitIsDropped(t *testing.T) {
	a := carryUnit("a.go:Fetch", "a.go", "Fetch", "+\tv := decodeRow(b)")
	gone := carryUnit("b.go:decodeRow", "b.go", "decodeRow", "+\treturn nil, err")
	c := PlanCarryOver([]*Unit{clone(a)}, []*Unit{a, gone}, "deadbeef")
	if _, ok := c.Reuse["a.go:Fetch"]; ok {
		t.Error("a unit judged against code the push removed must be reviewed again")
	}
}

func TestCarryOverIgnoresGeneratedNeighbours(t *testing.T) {
	a := carryUnit("a.go:Fetch", "a.go", "Fetch", "+\tx := 1")
	gen := carryUnit("api.pb.go", "api.pb.go", "", "+\tgenerated line")
	gen.Decision = Decision{Bucket: BucketNone, Source: "rule", Reason: "generated"}
	fa, fg := clone(a), clone(gen)
	fg.Decision = gen.Decision
	fg.Hunks[0].Lines = []string{"+\tanother generated line"}
	c := PlanCarryOver([]*Unit{fa, fg}, []*Unit{a, gen}, "deadbeef")
	if _, ok := c.Reuse["a.go:Fetch"]; !ok {
		t.Errorf("a regenerated file is never shown to a reviewer, so it cannot cost a review: %q", c.Why["a.go:Fetch"])
	}
}

func TestCarryOverSkipsUnreviewedAndNewUnits(t *testing.T) {
	never := carryUnit("a.go:Fetch", "a.go", "Fetch", "+\tx := 1")
	never.Reviewed, never.Summary = false, ""
	fresh := clone(never)
	added := carryUnit("z.go:New", "z.go", "New", "+\tbrand new")
	c := PlanCarryOver([]*Unit{fresh, clone(added)}, []*Unit{never}, "deadbeef")
	if len(c.Reuse) != 0 {
		t.Errorf("nothing was reviewed before, so there is nothing to keep: %v", c.Reuse)
	}
	if _, ok := c.Why["z.go:New"]; ok {
		t.Error("a unit the earlier run never saw is not 'reviewed again', it is just reviewed")
	}
}

func TestDiffBodyIgnoresHunkHeaderLineNumbers(t *testing.T) {
	a := carryUnit("a.go:Fetch", "a.go", "Fetch", " ctx := c", "+\tx := 1")
	b := clone(a)
	b.Hunks[0].Header = "@@ -40,3 +47,3 @@ Fetch"
	b.Hunks[0].NewStart, b.Hunks[0].OldStart = 47, 40
	if diffBody(a) != diffBody(b) {
		t.Error("the same lines shifted down the file are the same code")
	}
	b.Hunks[0].Lines = []string{" ctx := c", "+\tx := 2"}
	if diffBody(a) == diffBody(b) {
		t.Error("different lines are different code")
	}
}

func TestApplyCarriedKeepsTheReviewAndDropsStaleDismissals(t *testing.T) {
	tp := DefaultTierPolicy()
	old := carryUnit("a.go:Fetch", "a.go", "Fetch", "+\tx := 1")
	old.Issues = []Issue{{Severity: "medium", Title: "Leaks a connection", Evidence: "x := 1", Scenario: "every call",
		Dismissed: true, DismissedWhy: "stale", DismissKey: "abc"}}
	old.Focus = []string{"check the caller"}

	u := clone(old)
	u.Impact, u.Likelihood = &Impact{Score: 40, Level: "medium"}, &Likelihood{Score: 40, Level: "medium"}
	tp.prior(u, 0)
	applyCarried(u, old, "deadbeef")
	if !u.Reviewed || u.Summary != old.Summary || len(u.Issues) != 1 || len(u.Focus) != 1 {
		t.Fatalf("the review did not come across: %+v", u)
	}
	if u.CarriedFrom != "deadbeef" {
		t.Errorf("CarriedFrom = %q", u.CarriedFrom)
	}
	if u.Issues[0].Dismissed || u.Issues[0].DismissKey != "" {
		t.Error("a dismissal is the repository's record and is re-applied on load, not carried as a fact")
	}
	// The unit is placed from this run's prior and the kept issue.
	tp.afterReview(u, u.Decision.Bucket)
	if u.Decision.Bucket != BucketHuman || !u.Score.PinIssue {
		t.Errorf("a carried medium issue should pin the unit like a fresh one: %s", u.Score.Why)
	}
	if st := CountCarried([]*Unit{u}); st == nil || st.Reused != 1 || st.From != "deadbeef" {
		t.Errorf("CountCarried = %+v", st)
	}
	if CountCarried([]*Unit{clone(old)}) != nil {
		t.Error("a run that carried nothing should report nothing")
	}
}

func TestCarrySummaryReadsAsASentence(t *testing.T) {
	c := &ReviewCarry{From: "abc1234", Reuse: map[string]*Unit{"a": nil, "b": nil},
		Why: map[string]string{"c": "its own diff changed", "d": "a.go:G changed in the same file"}}
	got := c.Summary()
	for _, want := range []string{"2 unit(s) from abc1234", "1: their diff changed", "1: another change in the same file moved"} {
		if !strings.Contains(got, want) {
			t.Errorf("Summary() = %q, missing %q", got, want)
		}
	}
	if (*ReviewCarry)(nil).Summary() == "" {
		t.Error("no earlier run should still say something")
	}
}

// pushDiff is a three-file change; push2 edits one line of store.go and
// leaves fetch.go and util.go exactly as they were.
const push1Diff = `diff --git a/pkg/store.go b/pkg/store.go
--- a/pkg/store.go
+++ b/pkg/store.go
@@ -1,4 +1,7 @@ package pkg
 package pkg
 
 func Store(k string, v int) {
+	if v < 0 {
+		panic("negative")
+	}
 	rows[k] = v
 }
diff --git a/pkg/fetch.go b/pkg/fetch.go
--- a/pkg/fetch.go
+++ b/pkg/fetch.go
@@ -1,3 +1,3 @@ package pkg
 package pkg
 
-func Fetch(k string) int { return rows[k] }
+func Fetch(k string) (int, bool) { v, ok := rows[k]; return v, ok }
diff --git a/docs/notes.md b/docs/notes.md
--- a/docs/notes.md
+++ b/docs/notes.md
@@ -1 +1,2 @@
 notes
+more notes
`

const push2Diff = `diff --git a/pkg/store.go b/pkg/store.go
--- a/pkg/store.go
+++ b/pkg/store.go
@@ -1,4 +1,7 @@ package pkg
 package pkg
 
 func Store(k string, v int) {
+	if v < 0 {
+		return
+	}
 	rows[k] = v
 }
diff --git a/pkg/fetch.go b/pkg/fetch.go
--- a/pkg/fetch.go
+++ b/pkg/fetch.go
@@ -1,3 +1,3 @@ package pkg
 package pkg
 
-func Fetch(k string) int { return rows[k] }
+func Fetch(k string) (int, bool) { v, ok := rows[k]; return v, ok }
diff --git a/docs/notes.md b/docs/notes.md
--- a/docs/notes.md
+++ b/docs/notes.md
@@ -1 +1,2 @@
 notes
+more notes
`

// countingPipeline builds a pipeline whose classifier and reviewer are
// fakes, and counts the review calls.
func countingPipeline(t *testing.T, reviews *int) *Pipeline {
	t.Helper()
	classify := &fakeLLM{fn: func(llm.LLMRequest) (*llm.LLMResponse, error) {
		return toolResp("submit_triage", map[string]any{"bucket": "skim", "change_kind": "behavior", "confidence": 0.95, "reason": "a change"}), nil
	}}
	review := &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		*reviews++
		name := "submit_summary"
		for _, tool := range req.Tools {
			name = tool.Name
		}
		if strings.HasPrefix(name, "submit_group_") {
			// One entry per unit id the prompt listed.
			var units []any
			for _, line := range strings.Split(req.Messages[len(req.Messages)-1].Content, "\n") {
				if id, ok := strings.CutPrefix(line, "#### unit id: "); ok {
					units = append(units, map[string]any{"id": id, "headline": "h", "summary": "s", "safe": true, "focus": []any{"check it"}, "issues": []any{}})
				}
			}
			return toolResp(name, map[string]any{"units": units}), nil
		}
		return toolResp(name, map[string]any{"headline": "h", "summary": "s", "safe": true, "focus": []any{"check it"}, "issues": []any{}}), nil
	}}
	policy := DefaultPolicy()
	policy.Lint.Enabled = false
	return &Pipeline{
		Presorter:  &Presorter{Policy: policy},
		Classifier: &LLMClassifier{LLM: classify, Policy: policy},
		Summarizer: &Summarizer{LLM: review, Critic: review, Policy: policy},
	}
}

// headDir writes the head versions of the changed files, so units are
// split at real declarations the way a PR run splits them.
func headDir(t *testing.T, store string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"pkg/store.go":  store,
		"pkg/fetch.go":  "package pkg\n\nfunc Fetch(k string) (int, bool) { v, ok := rows[k]; return v, ok }\n",
		"docs/notes.md": "notes\nmore notes\n",
	}
	for path, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const (
	storeAtPush1 = "package pkg\n\nfunc Store(k string, v int) {\n\tif v < 0 {\n\t\tpanic(\"negative\")\n\t}\n\trows[k] = v\n}\n"
	storeAtPush2 = "package pkg\n\nfunc Store(k string, v int) {\n\tif v < 0 {\n\t\treturn\n\t}\n\trows[k] = v\n}\n"
)

func TestPipelineCarriesReviewsAcrossAPush(t *testing.T) {
	run := func(raw, store string, carry func([]*Unit) *ReviewCarry) ([]*Unit, int) {
		src, err := FromDiff(raw, headDir(t, store), "")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		p := countingPipeline(t, &n)
		p.CarryFrom = carry
		return p.Run(context.Background(), src), n
	}

	first, firstCalls := run(push1Diff, storeAtPush1, nil)
	if firstCalls == 0 {
		t.Fatal("the first run reviewed nothing")
	}

	var plan *ReviewCarry
	second, secondCalls := run(push2Diff, storeAtPush2, func(fresh []*Unit) *ReviewCarry {
		plan = PlanCarryOver(fresh, first, "head1")
		return plan
	})

	byFile := map[string]*Unit{}
	for _, u := range second {
		byFile[u.File] = u
	}
	// fetch.go is untouched and in another file, so it keeps its review.
	fetch := byFile["pkg/fetch.go"]
	if fetch == nil || fetch.CarriedFrom != "head1" {
		t.Fatalf("fetch.go should have kept its review: %+v (why: %q)", fetch, plan.Why[fetch.ID])
	}
	if !fetch.Reviewed || fetch.Summary == "" || fetch.Score == nil {
		t.Errorf("a carried unit must look reviewed and be placed: %+v", fetch)
	}
	// docs/notes.md is untouched too, and nothing links it to store.go.
	if notes := byFile["docs/notes.md"]; notes == nil || notes.CarriedFrom != "head1" {
		t.Errorf("docs/notes.md should have kept its review: %+v", notes)
	}
	// store.go changed, so it is reviewed again.
	store := byFile["pkg/store.go"]
	if store == nil || store.CarriedFrom != "" {
		t.Errorf("store.go changed and must be reviewed again: %+v", store)
	}
	if plan.Why[store.ID] != "its own diff changed" {
		t.Errorf("why store.go was reviewed again: %q", plan.Why[store.ID])
	}
	if secondCalls >= firstCalls {
		t.Errorf("the second run made %d review calls against the first run's %d", secondCalls, firstCalls)
	}
	if st := CountCarried(second); st == nil || st.Reused == 0 {
		t.Errorf("CountCarried = %+v", st)
	}

	// Running the same diff again carries everything: no review calls.
	_, noneCalls := run(push2Diff, storeAtPush2, func(fresh []*Unit) *ReviewCarry {
		return PlanCarryOver(fresh, second, "head2")
	})
	if noneCalls != 0 {
		t.Errorf("an unchanged push made %d review calls, want 0", noneCalls)
	}
}
