package triage

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Issue is a problem the reviewer found while summarizing a unit.
type Issue struct {
	Severity string `json:"severity"`       // low | medium | high | critical
	Line     int    `json:"line,omitempty"` // new-file line, 0 if not tied to one
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
	// Evidence quotes the code that causes it; Scenario is the concrete
	// input or state that goes wrong.
	Evidence string `json:"evidence,omitempty"`
	Scenario string `json:"failure_scenario,omitempty"`
	// PreExisting: the reviewer says the behavior was there before the PR.
	PreExisting bool `json:"pre_existing,omitempty"`
	// Capped is why the severity was lowered from Claimed, the reviewer's own.
	Claimed string `json:"claimed_severity,omitempty"`
	Capped  string `json:"capped,omitempty"`
	// CriticRejected is the critic's reason for rejecting a medium or
	// worse issue, which is then kept at low rather than dropped.
	CriticRejected string `json:"critic_rejected,omitempty"`
	// Dismissed is set when a person rejected the issue. A dismissed issue
	// is kept and shown, but it stops counting: not in the unit's
	// attention, not in what a clean review is worth, and not in the pin a
	// medium-or-worse issue puts on the unit.
	Dismissed    bool   `json:"dismissed,omitempty"`
	DismissedWhy string `json:"dismissed_why,omitempty"`
	// DismissKey is the stored record to delete to restore it; set only
	// while the issue is dismissed.
	DismissKey string `json:"dismiss_key,omitempty"`
}

// Live reports whether the issue still counts toward a unit's bucket.
func (is Issue) Live() bool { return !is.Dismissed }

// liveIssues drops the ones a person dismissed.
func liveIssues(issues []Issue) []Issue {
	out := issues[:0:0]
	for _, is := range issues {
		if is.Live() {
			out = append(out, is)
		}
	}
	return out
}

// capTo lowers the severity to sev, keeping the first claim and reason.
func (is *Issue) capTo(sev, why string) {
	if severityWeight[is.Severity] <= severityWeight[sev] {
		return
	}
	if is.Claimed == "" {
		is.Claimed = is.Severity
	}
	is.Severity = sev
	is.Capped = strings.TrimPrefix(is.Capped+"; "+why, "; ")
}

var severityWeight = map[string]int{"low": 15, "medium": 45, "high": 75, "critical": 95}

// attentionScore turns review findings into 0-100: the worst issue, plus 5
// for each further issue. No issues is 0.
func attentionScore(issues []Issue) int {
	issues = liveIssues(issues)
	worst := 0
	for _, is := range issues {
		worst = max(worst, severityWeight[is.Severity])
	}
	if worst == 0 {
		return 0
	}
	return min(100, worst+5*(len(issues)-1))
}

// TierPolicy picks buckets from one score per unit. Before review it is
// the prior: √(impact × likelihood) weighted by the change kind. A review
// that found nothing lowers it by the review budget's trust; issues set a
// floor on it (their attention). The budget's cut-offs then pick human,
// skim or none. Some units skip the score (pins) or have a minimum bucket
// (floors); see pinAndFloor and afterReview.
type TierPolicy struct {
	// ReviewBudget names the default step in Budgets (most … least).
	ReviewBudget string `yaml:"review_budget"`
	// Budgets override the built-in steps by name; unset fields keep them.
	Budgets map[string]Budget `yaml:"budgets"`
	// KindWeights scale the prior by the classifier's change kind; kinds
	// not listed weigh 1.
	KindWeights map[string]float64 `yaml:"kind_weights"`
	// CriticalImpact: units at or above it always go to human (0: off).
	CriticalImpact int `yaml:"critical_impact"`
}

// Budget is one step of the review budget. Trust (0-1) is how much a clean
// review lowers a unit's score (half as much with only low issues). A unit
// scoring Human or more goes to human review, Skim or more to skim.
// LiftFloors lets a review that found nothing lift a unit's floor, so a
// clean behavior change can drop to none.
type Budget struct {
	Trust      float64 `yaml:"trust" json:"trust"`
	Human      int     `yaml:"human" json:"human"`
	Skim       int     `yaml:"skim" json:"skim"`
	LiftFloors bool    `yaml:"lift_floors" json:"lift_floors,omitempty"`
}

// BudgetNames orders the steps from the most human review to the least.
var BudgetNames = []string{"most", "more", "balanced", "less", "least"}

// DefaultBudget is the step used when the policy names none.
const DefaultBudget = "balanced"

func DefaultTierPolicy() TierPolicy {
	return TierPolicy{
		ReviewBudget: DefaultBudget,
		Budgets: map[string]Budget{
			"most":     {Trust: 0, Human: 30, Skim: 10},
			"more":     {Trust: 0.15, Human: 35, Skim: 12},
			"balanced": {Trust: 0.3, Human: 40, Skim: 15},
			"less":     {Trust: 0.45, Human: 45, Skim: 18, LiftFloors: true},
			"least":    {Trust: 0.6, Human: 50, Skim: 20, LiftFloors: true},
		},
		KindWeights: map[string]float64{
			"behavior": 1, "config": 1, "test": 0.8, "refactor": 0.6, "rename": 0.5,
			"docs": 0.3, "format": 0.3, "generated": 0.3,
		},
		CriticalImpact: 85,
	}
}

// Budget returns the named step, or the policy's default for "".
func (tp TierPolicy) Budget(name string) (Budget, string, error) {
	if name == "" {
		name = tp.ReviewBudget
	}
	if name == "" {
		name = DefaultBudget
	}
	b, ok := tp.Budgets[name]
	if !ok {
		return b, name, fmt.Errorf("unknown review budget %q (want one of %s)", name, strings.Join(BudgetNames, ", "))
	}
	return b, name, nil
}

// OrderedBudgets lists the steps most → least, then any custom ones.
func (tp TierPolicy) OrderedBudgets() []NamedBudget {
	var out []NamedBudget
	seen := map[string]bool{}
	for _, n := range BudgetNames {
		if b, ok := tp.Budgets[n]; ok {
			out = append(out, NamedBudget{n, b})
			seen[n] = true
		}
	}
	var extra []string
	for n := range tp.Budgets {
		if !seen[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	for _, n := range extra {
		out = append(out, NamedBudget{n, tp.Budgets[n]})
	}
	return out
}

type NamedBudget struct {
	Name string `json:"name"`
	Budget
}

// Score is how a unit's bucket was picked. It carries everything the
// budget needs, so the UI can re-bucket with another budget without a re-run.
type Score struct {
	Base  int     `json:"base"`  // √(impact × likelihood); unknown ones count as 50
	Kind  float64 `json:"kind"`  // change-kind weight
	Prior int     `json:"prior"` // base × kind: the score before review
	// Clean is the share of the budget's trust the review earned: 1 found
	// nothing, 0.5 only low issues, 0 not reviewed.
	Clean float64 `json:"clean"`
	// Classified is the classifier's bucket (after its confidence thresholds).
	Classified Bucket `json:"classified,omitempty"`
	// Pin fixes the bucket whatever the budget; Floor is the lowest it can go.
	Pin    Bucket `json:"pin,omitempty"`
	PinWhy string `json:"pin_why,omitempty"`
	// PinIssue: Pin came from an issue the review found, so dismissing
	// that issue can lift it (see ApplyDismissals). A pin from a failed
	// review is not one of these and never lifts.
	PinIssue bool `json:"pin_issue,omitempty"`
	// PinBefore is the pin the unit had before an issue pinned it to
	// human (a rule's or the classifier's bucket), put back when the
	// issue is dismissed.
	PinBefore    Bucket `json:"pin_before,omitempty"`
	PinBeforeWhy string `json:"pin_before_why,omitempty"`
	Floor        Bucket `json:"floor,omitempty"`
	FloorWhy     string `json:"floor_why,omitempty"`
	// CodeFloor is the lowest bucket a unit whose change is more than
	// comments and whitespace can go, set by changesCode and not by the
	// model, so a comment that talks the model into "none" cannot put code
	// there. Unlike Floor no budget's lift_floors lifts it, and it raises a
	// pin that came from the classifier's bucket. Rule-placed units have
	// none: a rule's none does not rest on the model's answer.
	CodeFloor    Bucket `json:"code_floor,omitempty"`
	CodeFloorWhy string `json:"code_floor_why,omitempty"`
	// CommentPin is the pin an open, confirmed review comment of medium
	// or worse sets (see ApplyThreads). It is kept apart from Pin so it
	// goes away when the comment is resolved or fixed, and it overrides
	// any Pin but human.
	CommentPin    Bucket `json:"comment_pin,omitempty"`
	CommentPinWhy string `json:"comment_pin_why,omitempty"`
	// Total, Budget and Why are for the policy's budget.
	Total  int    `json:"total"`
	Budget string `json:"budget"`
	Why    string `json:"why"`
}

// TotalAt is the score under b: the prior lowered by the trust a clean
// review earned, but never below the review's attention.
func (s *Score) TotalAt(b Budget, attention int) int {
	return max(attention, int(math.Round(float64(s.Prior)*(1-b.Trust*s.Clean))))
}

// Place picks the bucket under b and says why.
func (s *Score) Place(b Budget, name string, attention int) (Bucket, int, string) {
	t := s.TotalAt(b, attention)
	if pin, why := s.pin(); pin != "" {
		why = fmt.Sprintf("%s: %s (any budget)", pin, why)
		if s.CodeFloor != "" && s.CodeFloor.rank() > pin.rank() {
			pin = s.CodeFloor
			why = fmt.Sprintf("%s: %s (any budget; the classifier's %s)", pin, s.CodeFloorWhy, s.Pin)
		}
		return pin, t, why
	}
	bk, cut := BucketNone, fmt.Sprintf("< %d", b.Skim)
	switch {
	case t >= b.Human:
		bk, cut = BucketHuman, fmt.Sprintf("≥ %d", b.Human)
	case t >= b.Skim:
		bk, cut = BucketSkim, fmt.Sprintf("≥ %d", b.Skim)
	}
	why := fmt.Sprintf("score %d = %s → %s (%s on %s)", t, s.arithmetic(b, attention), bk, cut, name)
	if s.Floor != "" && s.Floor.rank() > bk.rank() {
		if b.LiftFloors && s.Clean == 1 {
			why += fmt.Sprintf("; %s lifted by the clean review", s.FloorWhy)
		} else {
			bk = s.Floor
			why += fmt.Sprintf("; raised to %s: %s", bk, s.FloorWhy)
		}
	}
	if s.CodeFloor != "" && s.CodeFloor.rank() > bk.rank() {
		bk = s.CodeFloor
		why += fmt.Sprintf("; raised to %s: %s (no budget lifts this)", bk, s.CodeFloorWhy)
	}
	return bk, t, why
}

// pin is the pin that applies: a comment pin wins over any other but human.
func (s *Score) pin() (Bucket, string) {
	if s.CommentPin != "" && s.Pin != BucketHuman {
		return s.CommentPin, s.CommentPinWhy
	}
	return s.Pin, s.PinWhy
}

func (s *Score) arithmetic(b Budget, attention int) string {
	x := fmt.Sprintf("%d", s.Base)
	if s.Kind != 1 {
		x += fmt.Sprintf(" × kind %.2g", s.Kind)
	}
	if f := b.Trust * s.Clean; f > 0 {
		what := "clean review"
		if s.Clean < 1 {
			what = "only low issues"
		}
		x += fmt.Sprintf(" × %.2g (%s)", 1-f, what)
	}
	if lowered := int(math.Round(float64(s.Prior) * (1 - b.Trust*s.Clean))); attention > lowered {
		x = fmt.Sprintf("review attention %d (over %s)", attention, x)
	}
	return x
}

// prior scores a classified unit and places it before review. The bucket
// the classifier picked moves the kind weight: "human" is at least 0.8
// (a deleted assertion is a test change a person should see), "none" at
// most 0.3.
func (tp TierPolicy) prior(u *Unit, maxChars int) {
	imp, lk := 50, 50
	if u.Impact.Known() {
		imp = u.Impact.Score
	}
	if u.Likelihood != nil {
		lk = u.Likelihood.Score
	}
	d := u.Decision
	s := &Score{Base: int(math.Round(math.Sqrt(float64(imp * lk)))), Kind: 1, Classified: d.Bucket}
	if w, ok := tp.KindWeights[d.ChangeKind]; ok {
		s.Kind = w
	}
	// The model's own answer can put code in none only through a rule
	// (presort), never by itself: see CodeFloor. Its "none" does not lower
	// the kind weight of such a unit either.
	code := d.Source != "rule" && changesCode(u)
	if code {
		s.CodeFloor, s.CodeFloorWhy = BucketSkim, "the change is more than comments and whitespace, so only a rule can skip it"
	}
	switch {
	case d.Bucket == BucketHuman:
		s.Kind = max(s.Kind, 0.8)
	case d.Bucket == BucketNone && !code:
		s.Kind = min(s.Kind, 0.3)
	}
	s.Prior = int(math.Round(float64(s.Base) * s.Kind))

	switch {
	case d.Source == "rule":
		s.Pin, s.PinWhy = d.Bucket, d.Reason
	case d.Source == "none":
		s.Pin, s.PinWhy = BucketHuman, d.Reason
	case d.Confidence == 0:
		s.Pin, s.PinWhy = BucketHuman, "no confident classification: "+d.Reason
	case d.Bucket == BucketHuman && isTestPath(u.File):
		// Test code scores no likelihood, so its score is 0 and only a
		// floor would hold it, which a clean review lifts. A weakened test
		// is the classifier's call: keep it.
		s.Pin, s.PinWhy = BucketHuman, "the classifier asked for human review of test code, which is not scored"
	case !u.Impact.Known():
		// Without the map the score is mostly a guess: keep the classifier's call.
		s.Pin, s.PinWhy = d.Bucket, "classifier's bucket, impact unknown ("+impactUnknown(u.Impact)+")"
	case tp.CriticalImpact > 0 && u.Impact.Score >= tp.CriticalImpact:
		s.Pin, s.PinWhy = BucketHuman, fmt.Sprintf("critical impact %d (%s)", u.Impact.Score, impactWhere(u.Impact))
	}
	// Text in the change written for the reviewer or the triage is the PR
	// author steering the review: a person reads it.
	if d.Source != "rule" && s.Pin != BucketHuman {
		if said := reviewerDirectedText(u); len(said) > 0 {
			s.Pin, s.PinWhy = BucketHuman, "added comments or strings address the reviewer or assert their own safety: \""+strings.Join(said, "\", \"")+"\""
		}
	}
	switch {
	case d.ChangeKind == "behavior":
		s.Floor, s.FloorWhy = BucketSkim, "a behavior change is never skipped"
	case d.Bucket == BucketHuman && d.Source != "rule":
		s.Floor, s.FloorWhy = BucketSkim, "the classifier asked for human review"
	case len(d.RiskSignals) > 0:
		s.Floor, s.FloorWhy = BucketSkim, "risk signals: "+strings.Join(d.RiskSignals, ", ")
	case maxChars > 0 && len(u.Diff()) > maxChars:
		s.Floor, s.FloorWhy = BucketSkim, "diff too long to show the classifier whole"
	}
	u.Score = s
	tp.place(u)
}

// afterReview applies what the reviewer found. prev is the bucket the
// review saw; a failed review call raised it.
func (tp TierPolicy) afterReview(u *Unit, prev Bucket) {
	s := u.Score
	u.Attention = attentionScore(u.Issues)
	worst := worstIssue(u.Issues)
	why := ""
	fromIssue := false
	raised := u.Decision.Bucket != prev && len(u.Decision.Escalated) > 0
	switch {
	case raised: // the review call failed
		why = u.Decision.Escalated[len(u.Decision.Escalated)-1]
	case !u.Reviewed:
		why = "review failed or gave no answer"
	case severityWeight[worst.Severity] >= severityWeight["medium"]:
		why = fmt.Sprintf("review found a %s issue: %s", worst.Severity, worst.Title)
		fromIssue = true
	}
	if why != "" && s.Pin != BucketHuman {
		if fromIssue {
			s.PinBefore, s.PinBeforeWhy = s.Pin, s.PinWhy
		}
		s.Pin, s.PinWhy, s.PinIssue = BucketHuman, why, fromIssue
	}
	s.Clean = cleanShare(u)
	tp.place(u)
}

// cleanShare is how much of the budget's trust the review earned: all of
// it when nothing was found, half when only low issues were, none when a
// real issue stands or the review never answered. Dismissed issues do not
// count, so dismissing the last one earns the unit its discount back.
func cleanShare(u *Unit, extra ...Issue) float64 {
	if !u.Reviewed {
		return 0
	}
	all := append(liveIssues(u.Issues), extra...)
	switch {
	case len(all) == 0:
		return 1
	case worstIssue(all).Severity == "low":
		return 0.5
	}
	return 0
}

// ApplyDismissals re-places u after its issues were dismissed or restored.
// It redoes what the review decided from the issues that still count: the
// unit's attention, the discount a clean review earns, and the pin a
// medium-or-worse issue put on it. A pin from a failed review stays, and
// so does the likelihood score, which was measured when the PR was
// triaged and is not a judgement anyone dismissed.
func (tp TierPolicy) ApplyDismissals(u *Unit) {
	s := u.Score
	if s == nil {
		return
	}
	// Results scored before PinIssue existed only say so in the reason.
	if !s.PinIssue && s.Pin == BucketHuman && strings.HasPrefix(s.PinWhy, "review found a ") {
		s.PinIssue = true
	}
	if s.PinIssue {
		if w := worstIssue(u.Issues); severityWeight[w.Severity] >= severityWeight["medium"] {
			s.Pin, s.PinWhy = BucketHuman, fmt.Sprintf("review found a %s issue: %s", w.Severity, w.Title)
		} else {
			s.Pin, s.PinWhy = s.PinBefore, s.PinBeforeWhy
		}
	}
	tp.ApplyThreads(u) // attention, clean and the comment pin, from what is left
}

// ApplyThreads counts u's open, confirmed review comments as if the
// review had found them: they raise its attention, cost a clean review its
// discount, and one of medium or worse pins it to human. Comments that
// repeat one of u's issues count once. It recomputes from scratch, so a
// comment that is resolved or fixed stops counting. Unscored units are
// left alone.
func (tp TierPolicy) ApplyThreads(u *Unit) {
	s := u.Score
	if s == nil {
		return
	}
	var from []Issue
	var worst *Thread
	for i := range u.Threads {
		t := &u.Threads[i]
		if t.Status != ThreadValid || t.Fixed || t.Issue == nil || t.DuplicateOf != nil {
			continue
		}
		from = append(from, *t.Issue)
		if worst == nil || severityWeight[t.Issue.Severity] > severityWeight[worst.Issue.Severity] {
			worst = t
		}
	}
	all := append(liveIssues(u.Issues), from...)
	u.Attention = attentionScore(all)
	s.CommentPin, s.CommentPinWhy = "", ""
	if worst != nil && severityWeight[worst.Issue.Severity] >= severityWeight["medium"] {
		s.CommentPin = BucketHuman
		s.CommentPinWhy = fmt.Sprintf("confirmed %s review comment by @%s: %s", worst.Issue.Severity, worst.Author, worst.Issue.Title)
	}
	s.Clean = cleanShare(u, from...)
	tp.place(u)
}

// place sets the bucket for the policy's budget.
func (tp TierPolicy) place(u *Unit) {
	b, name, err := tp.Budget("")
	if err != nil {
		b, name = DefaultTierPolicy().Budgets[DefaultBudget], DefaultBudget
	}
	s := u.Score
	u.Decision.Bucket, s.Total, s.Why = s.Place(b, name, u.Attention)
	s.Budget = name
}

// Rescore scores units triaged before scores existed, from what they
// stored. The stored bucket stands in for the classifier's, which older
// runs did not keep apart from their tier moves.
func Rescore(units []*Unit, tp TierPolicy) {
	for _, u := range units {
		if u.Score != nil || u.Decision.Bucket == "" {
			continue
		}
		tp.prior(u, 0)
		for _, e := range u.Decision.Escalated {
			if strings.HasPrefix(e, "summarizer:") || strings.HasPrefix(e, "summary failed:") {
				if u.Score.Pin != BucketHuman {
					u.Score.Pin, u.Score.PinWhy = BucketHuman, e
				}
			}
		}
		if u.Reviewed {
			tp.afterReview(u, u.Decision.Bucket)
		}
	}
}

// Rebucket re-places every scored unit under another budget.
func Rebucket(units []*Unit, tp TierPolicy, budget string) error {
	if _, _, err := tp.Budget(budget); err != nil {
		return err
	}
	if budget != "" {
		tp.ReviewBudget = budget
	}
	for _, u := range units {
		if u.Score != nil {
			tp.place(u)
		}
	}
	SortByBucket(units)
	return nil
}

// worstIssue is the most severe issue that still counts; a dismissed one
// never places a unit.
func worstIssue(issues []Issue) Issue {
	var w Issue
	for _, is := range issues {
		if is.Live() && severityWeight[is.Severity] > severityWeight[w.Severity] {
			w = is
		}
	}
	return w
}

func impactUnknown(d *Impact) string {
	if d == nil || len(d.Notes) == 0 {
		return "no code map"
	}
	return d.Notes[0]
}

func impactWhere(d *Impact) string {
	if d.Basis == "" {
		return "matched " + d.Matched
	}
	return d.Matched + " " + d.Basis
}

// decodeIssues validates the reviewer's issues; unknown severities count
// as medium so a malformed answer cannot hide a problem. The review prompt's
// rules are enforced here, not trusted to the model:
//   - an issue that depends on code the reviewer did not see is a claim to
//     check, not a finding: it is returned in checks instead
//   - a problem the PR did not introduce is capped at low
//   - medium or worse needs a failure scenario, or it is capped at low
func decodeIssues(v any) (issues []Issue, checks []string) {
	list, _ := v.([]any)
	for _, x := range list {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		is := Issue{}
		str := func(k string) string { s, _ := m[k].(string); return strings.TrimSpace(s) }
		is.Title, is.Detail = str("title"), str("detail")
		if is.Title == "" && is.Detail == "" {
			continue
		}
		if is.Title == "" {
			is.Title = is.Detail
		}
		is.Evidence, is.Scenario = str("evidence"), str("failure_scenario")
		is.Severity = strings.ToLower(str("severity"))
		if _, ok := severityWeight[is.Severity]; !ok {
			is.Severity = "medium"
		}
		switch n := m["line"].(type) {
		case float64:
			is.Line = max(0, int(n))
		case int:
			is.Line = max(0, n)
		}
		if unseen, _ := m["depends_on_unseen_code"].(bool); unseen {
			c := is.Title
			if is.Detail != "" {
				c += ": " + is.Detail
			}
			checks = append(checks, c)
			continue
		}
		// Missing counts as introduced: only an explicit "no" caps it.
		if introduced, ok := m["introduced_by_pr"].(bool); ok && !introduced {
			is.PreExisting = true
			is.capTo("low", "pre-existing behavior")
		}
		if is.Scenario == "" {
			is.capTo("low", "no failure scenario")
		}
		issues = append(issues, is)
	}
	return issues, checks
}
