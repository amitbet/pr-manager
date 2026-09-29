package triage

import "testing"

// reviewed builds a unit that a review placed in human because of one
// medium issue, the case dismissing has to undo.
func reviewedUnit(t *testing.T, tp TierPolicy, issues ...Issue) *Unit {
	t.Helper()
	u := &Unit{ID: "a.go:F", File: "a.go", Hunks: []Hunk{{NewStart: 1, Lines: []string{"+x := 1"}}}}
	u.Decision = Decision{Bucket: BucketSkim, Source: "llm", Confidence: 1, ChangeKind: "behavior"}
	u.Impact = &Impact{Score: 40, Level: "medium"}
	u.Likelihood = &Likelihood{Score: 40, Level: "medium"}
	tp.prior(u, 0)
	u.Reviewed, u.Issues = true, issues
	tp.afterReview(u, u.Decision.Bucket)
	return u
}

func TestDismissingTheOnlyIssueLetsTheUnitFallBack(t *testing.T) {
	tp := DefaultTierPolicy()
	is := Issue{Severity: "medium", Title: "Retry loop can spin forever", Evidence: "for {", Scenario: "a 500 response never stops the loop"}
	u := reviewedUnit(t, tp, is)
	if u.Decision.Bucket != BucketHuman || !u.Score.PinIssue {
		t.Fatalf("a medium issue should pin the unit to human: bucket %s pin %q", u.Decision.Bucket, u.Score.PinWhy)
	}
	before := u.Attention

	key := IssueKey(u.ID, is)
	n := ApplyDismissed([]*Unit{u}, tp, func(k string) (string, bool) { return "intended: the caller times out", k == key })
	if n != 1 {
		t.Fatalf("want 1 dismissal applied, got %d", n)
	}
	if u.Score.Pin != "" {
		t.Errorf("the pin should lift with its issue, still %q", u.Score.PinWhy)
	}
	if u.Attention != 0 {
		t.Errorf("attention %d, want 0: a dismissed issue does not count (was %d)", u.Attention, before)
	}
	if u.Score.Clean != 1 {
		t.Errorf("clean %v, want 1: with nothing left standing the review is clean", u.Score.Clean)
	}
	if u.Decision.Bucket == BucketHuman {
		t.Errorf("unit still in human review at %s", u.Score.Why)
	}
	if !u.Issues[0].Dismissed || u.Issues[0].DismissedWhy == "" {
		t.Error("the issue and the reason for rejecting it should both stay on the record")
	}

	// Restoring puts it back exactly where the review left it.
	ApplyDismissed([]*Unit{u}, tp, func(string) (string, bool) { return "", false })
	if u.Decision.Bucket != BucketHuman || u.Attention != before {
		t.Errorf("restore left bucket %s attention %d, want human/%d", u.Decision.Bucket, u.Attention, before)
	}
}

func TestDismissingOneOfTwoKeepsTheWorstOne(t *testing.T) {
	tp := DefaultTierPolicy()
	low := Issue{Severity: "low", Title: "Unclear name", Evidence: "x := 1"}
	high := Issue{Severity: "high", Title: "Nil map write panics", Evidence: "m[k] = v", Scenario: "m is nil on the first call"}
	u := reviewedUnit(t, tp, low, high)
	ApplyDismissed([]*Unit{u}, tp, func(k string) (string, bool) { return "noise", k == IssueKey(u.ID, low) })
	if u.Decision.Bucket != BucketHuman {
		t.Errorf("bucket %s: the high issue still stands", u.Decision.Bucket)
	}
	if want := severityWeight["high"]; u.Attention != want {
		t.Errorf("attention %d, want %d: only the live issue counts", u.Attention, want)
	}
}

func TestFailedReviewPinDoesNotLift(t *testing.T) {
	tp := DefaultTierPolicy()
	u := reviewedUnit(t, tp)
	u.Reviewed = false
	tp.afterReview(u, u.Decision.Bucket)
	if u.Score.Pin != BucketHuman || u.Score.PinIssue {
		t.Fatalf("a failed review should pin without an issue behind it: %+v", u.Score)
	}
	ApplyDismissed([]*Unit{u}, tp, func(string) (string, bool) { return "", true })
	if u.Score.Pin != BucketHuman {
		t.Error("a pin from a failed review is not something anyone dismissed")
	}
}

func TestIssueKeyIgnoresRewording(t *testing.T) {
	a := Issue{Title: "Retry loop can spin forever", Evidence: "\tfor {", Severity: "medium"}
	b := Issue{Title: "Loop never terminates on error", Evidence: "for {", Severity: "high"}
	if IssueKey("a.go:F", a) != IssueKey("a.go:F", b) {
		t.Error("the same quoted line is the same issue however the model words it")
	}
	if IssueKey("a.go:F", a) == IssueKey("b.go:G", a) {
		t.Error("the same claim on another unit is another issue")
	}
	// Without evidence the title has to stand in, and then wording matters.
	c := Issue{Title: "Retry loop can spin forever"}
	if IssueKey("a.go:F", c) == IssueKey("a.go:F", Issue{Title: "Something else entirely"}) {
		t.Error("different titles with no evidence should not collide")
	}
}

func TestLintKeyAndDismissal(t *testing.T) {
	tp := DefaultTierPolicy()
	u := reviewedUnit(t, tp)
	f := LintFinding{Tool: "golangci-lint", Rule: "errcheck", Severity: lintError, Line: 1, Message: "Error return value is not checked"}
	u.Lint = []LintFinding{f}
	ApplyDismissed([]*Unit{u}, tp, func(k string) (string, bool) { return "checked by the caller", k == LintKey(u.ID, f) })
	if !u.Lint[0].Dismissed {
		t.Fatal("the lint finding should be marked dismissed")
	}
	if lintContext(u) != "" {
		t.Error("a dismissed finding should not go back into the review prompt")
	}
}
