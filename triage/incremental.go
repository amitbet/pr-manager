package triage

import (
	"fmt"
	"sort"
	"strings"
)

// Reviewing a PR again after a push repeats almost all of the work. The
// author usually pushes a fix to one or two files; everything else has
// the same diff it had, and gets the same review, from the same model,
// for the same money.
//
// What makes reuse more than a string comparison is that a unit is not
// reviewed alone. The prompt carries the rest of the PR (see
// setReviewContext) exactly so a defect that is only visible across two
// units can be found: a caller that limits the problem, a test that shows
// the intent, code that moved from somewhere else. A unit whose own lines
// did not move can still deserve a different verdict because the unit it
// was judged against changed. So a unit keeps its review only when its own
// diff is unchanged *and* nothing it was judged against moved.

// ReviewCarry is the plan for one run: which units keep the review an
// earlier run earned, and why each of the others has to be reviewed again.
type ReviewCarry struct {
	// From names the run the reviews come from (a head commit, or the
	// diff hash of a local run).
	From string
	// Reuse maps a unit ID to the reviewed unit whose answer it keeps.
	Reuse map[string]*Unit
	// Why says, per unit ID, what changed that costs it its review. It
	// covers only units an earlier run had reviewed; a unit that is new,
	// or that was never reviewed, is simply not in it.
	Why map[string]string
}

// CarryStats is what a run kept, counted from the units themselves after
// it finished rather than from the plan, so it records what happened.
type CarryStats struct {
	From     string `json:"from"`     // the earlier run's head, or its key for a local run
	Reused   int    `json:"reused"`   // units that kept an earlier review
	Reviewed int    `json:"reviewed"` // units this run reviewed itself
}

// CountCarried counts what a finished run kept. It returns nil when the
// run reviewed everything itself, which is the ordinary case.
func CountCarried(units []*Unit) *CarryStats {
	st := CarryStats{}
	for _, u := range units {
		switch {
		case u.CarriedFrom != "":
			st.Reused++
			st.From = u.CarriedFrom
		case u.Reviewed:
			st.Reviewed++
		}
	}
	if st.Reused == 0 {
		return nil
	}
	return &st
}

func (c *ReviewCarry) reuse(u *Unit) *Unit {
	if c == nil {
		return nil
	}
	return c.Reuse[u.ID]
}

// PlanCarryOver works out which of fresh can keep the review previous
// earned. from names the earlier run. Units the earlier run did not
// review are never carried: there is no answer to keep, and their prior
// may have moved since.
func PlanCarryOver(fresh, previous []*Unit, from string) *ReviewCarry {
	c := &ReviewCarry{From: from, Reuse: map[string]*Unit{}, Why: map[string]string{}}
	old := make(map[string]*Unit, len(previous))
	for _, u := range previous {
		old[u.ID] = u
	}
	freshByID := make(map[string]*Unit, len(fresh))
	for _, u := range fresh {
		freshByID[u.ID] = u
	}
	// What this push changed: units whose diff moved, units it added, and
	// units it dropped. A unit that is gone counts too — code that was
	// judged against it was judged against something that no longer exists.
	//
	// Both versions of a unit whose diff moved go in, the one before the
	// push and the one after. The link that matters can live in either:
	// a unit reviewed as "this code moved here from X" has to be reviewed
	// again once X stops showing those lines as removed, even though X is
	// still in the PR.
	var changed []*Unit
	for _, u := range fresh {
		o := old[u.ID]
		if o != nil && diffBody(o) == diffBody(u) {
			continue
		}
		changed = append(changed, u)
		if o != nil {
			changed = append(changed, o)
		}
	}
	for _, o := range previous {
		if freshByID[o.ID] == nil && reviewable(o) {
			changed = append(changed, o)
		}
	}
	x := newRelIndex(changed)
	for _, u := range fresh {
		o := old[u.ID]
		if o == nil || !o.Reviewed {
			continue // nothing to keep
		}
		if diffBody(o) != diffBody(u) {
			c.Why[u.ID] = "its own diff changed"
			continue
		}
		if why, yes := x.related(u); yes {
			c.Why[u.ID] = why
			continue
		}
		c.Reuse[u.ID] = o
	}
	return c
}

// Summary is the one line the job log gets: how much of the previous
// review this run can keep, and what the rest lost it on.
func (c *ReviewCarry) Summary() string {
	if c == nil {
		return "no earlier run to carry a review from"
	}
	reasons := map[string]int{}
	for _, why := range c.Why {
		switch {
		case why == "its own diff changed":
			reasons["their diff changed"]++
		case strings.Contains(why, "same file"):
			reasons["another change in the same file moved"]++
		case strings.Contains(why, "name each other"):
			reasons["a unit they name changed"]++
		default:
			reasons["code moved between them and a changed unit"]++
		}
	}
	var parts []string
	for r, n := range reasons {
		parts = append(parts, fmt.Sprintf("%d: %s", n, r))
	}
	sort.Strings(parts)
	out := fmt.Sprintf("carrying the review of %d unit(s) from %s", len(c.Reuse), c.From)
	if len(parts) > 0 {
		out += "; reviewing again " + strings.Join(parts, ", ")
	}
	return out
}

// diffBody is a unit's changed and context lines without the hunk
// headers. The headers carry line numbers, which shift whenever anything
// above them in the file moves; the lines themselves are the code that
// was reviewed and the context it was read in.
func diffBody(u *Unit) string {
	var sb strings.Builder
	for _, h := range u.Hunks {
		for _, l := range h.Lines {
			sb.WriteString(l)
			sb.WriteByte('\n')
		}
		sb.WriteByte('\x00') // keep two hunks from running together
	}
	return sb.String()
}

// relIndex answers "was this unit judged against something that changed?"
// over a fixed set of changed units, with each one's diff, name and moved
// lines worked out once.
type relIndex struct {
	units []*Unit
	text  map[*Unit]changed
	name  map[*Unit]string
	diff  map[*Unit]string
}

func newRelIndex(moved []*Unit) *relIndex {
	x := &relIndex{text: map[*Unit]changed{}, name: map[*Unit]string{}, diff: map[*Unit]string{}}
	for _, o := range moved {
		if !reviewable(o) {
			continue // generated and formatting units are not shown to a reviewer
		}
		x.units = append(x.units, o)
		x.text[o], x.name[o], x.diff[o] = changedText(o), shortName(o.Symbol), o.Diff()
	}
	sort.SliceStable(x.units, func(i, j int) bool { return x.units[i].ID < x.units[j].ID })
	return x
}

// related reports whether u was judged against a changed unit, and says
// which one. These are the links the review prompt ranks above plain
// context (see rankRelated): code that moved between the two, either one
// naming the other, and the same file. A change to any of them can flip
// a verdict on lines that did not themselves move.
func (x *relIndex) related(u *Unit) (string, bool) {
	uText, uName, uDiff := changedText(u), shortName(u.Symbol), u.Diff()
	for _, o := range x.units {
		if o.ID == u.ID {
			continue
		}
		if n, _ := overlap(uText.added, x.text[o].removed); n >= minMovedLines {
			return fmt.Sprintf("%s changed, and code moved between them", o.ID), true
		}
		if n, _ := overlap(x.text[o].added, uText.removed); n >= minMovedLines {
			return fmt.Sprintf("%s changed, and code moved between them", o.ID), true
		}
		if mentions(uDiff, x.name[o]) || mentions(x.diff[o], uName) {
			return fmt.Sprintf("%s changed, and the two name each other", o.ID), true
		}
		if o.File == u.File {
			return fmt.Sprintf("%s changed in the same file", o.ID), true
		}
	}
	return "", false
}

// applyCarried gives u the review an earlier run wrote for it. The bucket
// and the score are not copied: impact and likelihood were measured again
// for this run, so afterReview places the unit from the fresh prior and
// the kept issues.
func applyCarried(u, old *Unit, from string) {
	u.Summary, u.Headline = old.Summary, old.Headline
	u.Focus = append([]string(nil), old.Focus...)
	u.Issues = append([]Issue(nil), old.Issues...)
	u.Reviewed = true
	u.CarriedFrom = from
	// A dismissal is re-applied when the result is loaded, from the
	// repository's own record, so it must not be carried as a fact here.
	for i := range u.Issues {
		u.Issues[i].Dismissed, u.Issues[i].DismissedWhy, u.Issues[i].DismissKey = false, "", ""
	}
}

// CarriedDecision is the decision an earlier run gave old, for a unit that
// keeps it. The stored decision holds the placed bucket (budget, pins,
// review issues); the classifier's or reviewer's own call is in the score.
// It is marked Failed so it is never saved as a fresh answer (and a failed
// one lost its Failed flag in the stored result).
func CarriedDecision(old *Unit) Decision {
	d := old.Decision
	d.Escalated = append([]string(nil), d.Escalated...)
	d.RiskSignals = append([]string(nil), d.RiskSignals...)
	if old.Score != nil && old.Score.Classified != "" {
		d.Bucket = old.Score.Classified
	}
	d.Failed = true
	return d
}

// SameDiff says whether a and b show the same diff lines, context and
// all: whether a decision or review made for one holds for the other.
func SameDiff(a, b *Unit) bool { return diffBody(a) == diffBody(b) }
