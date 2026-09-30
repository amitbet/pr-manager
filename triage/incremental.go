package triage

import (
	"fmt"
	"regexp"
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
// over a fixed set of changed units, with each one's diff, names and moved
// lines worked out once.
type relIndex struct {
	units []*Unit
	text  map[*Unit]changed
	names map[*Unit][]depName
	diff  map[*Unit]string
	code  map[*Unit]string
}

func newRelIndex(moved []*Unit) *relIndex {
	x := &relIndex{text: map[*Unit]changed{}, names: map[*Unit][]depName{}, diff: map[*Unit]string{}, code: map[*Unit]string{}}
	for _, o := range moved {
		if !reviewable(o) {
			continue // generated and formatting units are not shown to a reviewer
		}
		x.units = append(x.units, o)
		x.text[o], x.names[o], x.diff[o], x.code[o] = changedText(o), declaredNames(o), o.Diff(), codeText(o)
	}
	sort.SliceStable(x.units, func(i, j int) bool { return x.units[i].ID < x.units[j].ID })
	return x
}

// related reports whether u was judged against a changed unit, and says
// which one. These are the links the review prompt ranks above plain
// context (see rankRelated): code that moved between the two, either one
// naming the other, and the same file. A change to any of them can flip
// a verdict on lines that did not themselves move.
//
// The naming link is stricter than the prompt's: rankRelated only orders
// context, so it can afford to ignore names too short to tell apart, but a
// missed link here keeps a stale verdict. See refersTo.
func (x *relIndex) related(u *Unit) (string, bool) {
	uText, uNames, uDiff, uCode := changedText(u), declaredNames(u), u.Diff(), codeText(u)
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
		if refersToAny(uDiff, uCode, x.names[o]) || refersToAny(x.diff[o], x.code[o], uNames) {
			return fmt.Sprintf("%s changed, and the two name each other", o.ID), true
		}
		if o.File == u.File {
			return fmt.Sprintf("%s changed in the same file", o.ID), true
		}
	}
	return "", false
}

// depName is a name other code can reach a unit's declaration by. A member
// (a method) is only reached through a selector, x.name.
type depName struct {
	name   string
	member bool
}

var (
	// topDeclRe is an unindented declaration line in the common languages:
	// what a unit declares when its symbol does not say (a Go file that no
	// longer parses, a file split without declarations).
	topDeclRe = regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?(?:pub\s+)?(?:async\s+)?(?:func|var|const|let|type|def|class|fn|interface|struct|enum)\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)`)
	// blockSpecRe is a spec line of a Go var or const block, which names
	// the unit after its first spec only: "\tname = ...", "\ta, b int".
	blockSpecRe = regexp.MustCompile(`^\t([A-Za-z_][A-Za-z0-9_]*(?:\s*,\s*[A-Za-z_][A-Za-z0-9_]*)*)\s*(?:=[^=]|=$|\s+[A-Za-z_*\[])`)
)

// declaredNames lists the names a unit declares, for dependency links: its
// symbol's short name, plus the top-level declarations its changed lines
// add or remove, and every spec a changed line of a var or const block
// touches. A declaration a push removes counts as much as one it adds.
func declaredNames(u *Unit) []depName {
	var out []depName
	seen := map[string]bool{}
	add := func(n string, member bool) {
		if n != "" && identRe.MatchString(n) && !seen[n] {
			seen[n] = true
			out = append(out, depName{n, member})
		}
	}
	add(shortName(u.Symbol), strings.Contains(u.Symbol, "."))
	block := strings.HasPrefix(u.Symbol, "var ") || strings.HasPrefix(u.Symbol, "const ")
	for _, h := range u.Hunks {
		for _, l := range h.Lines {
			if len(l) < 2 || (l[0] != '+' && l[0] != '-') {
				continue
			}
			if m := topDeclRe.FindStringSubmatch(l[1:]); m != nil {
				add(m[1], strings.HasPrefix(l[1:], "func (") || strings.HasPrefix(l[1:], "func("))
			}
			if !block {
				continue
			}
			if m := blockSpecRe.FindStringSubmatch(l[1:]); m != nil {
				for _, n := range strings.Split(m[1], ",") {
					add(strings.TrimSpace(n), false)
				}
			}
		}
	}
	return out
}

func refersToAny(diff, code string, names []depName) bool {
	for _, n := range names {
		if refersTo(diff, code, n) {
			return true
		}
	}
	return false
}

// refersTo reports whether a diff uses n. A name of three characters or
// more counts anywhere in diff as a whole word, as the prompt ranks it
// (see mentions). A shorter one (db, r, ID) would match half of any
// diff that way, and the prompt ignores it for that reason, but a
// dependency on it is as real as any other. So it counts only in code,
// the diff lines with their comments cut (see codeText): a member only as
// a selector (.ID), anything else as a whole word that the same code does
// not declare as a local (i := 0, for i, let r), which is what nearly
// every short word in a function body is.
func refersTo(diff, code string, n depName) bool {
	if len(n.name) >= 3 {
		return mentions(diff, n.name)
	}
	if n.member {
		return hasWord(code, n.name, func(i int) bool { return i > 0 && code[i-1] == '.' })
	}
	return hasWord(code, n.name, nil) && !declaresLocal(code, n.name)
}

// hasWord reports whether text holds name as a whole word at an offset
// keep accepts (any, when keep is nil).
func hasWord(text, name string, keep func(i int) bool) bool {
	for off := 0; ; {
		i := strings.Index(text[off:], name)
		if i < 0 {
			return false
		}
		i += off
		end := i + len(name)
		if (i == 0 || !isWordByte(text[i-1])) && (end == len(text) || !isWordByte(text[end])) && (keep == nil || keep(i)) {
			return true
		}
		off = i + 1
	}
}

// declaresLocal reports whether code declares name for itself: a Go short
// declaration, a var, let or const, or a Python for. Such code refers to its own
// name, not to a package-level one of the same spelling.
func declaresLocal(code, name string) bool {
	re := regexp.MustCompile(`(?m)(?:^|[^A-Za-z0-9_.])(?:(?:[A-Za-z_][A-Za-z0-9_]*\s*,\s*)*` + name + `(?:\s*,\s*[A-Za-z_][A-Za-z0-9_]*)*\s*:=|for\s+` + name + `\s+in\b|(?:var|let|const)\s+` + name + `(?:[^A-Za-z0-9_.]|$))`)
	return re.MatchString(code)
}

// codeText is a unit's diff lines without their +/-/space markers, hunk
// headers or comments: the code a short name has to appear in to count.
// A comment is a line that starts one (//, /*, "* ", "# "), or the tail of a
// line after " //" or " # ".
func codeText(u *Unit) string {
	var sb strings.Builder
	for _, h := range u.Hunks {
		for _, l := range h.Lines {
			if len(l) < 2 {
				continue
			}
			t := strings.TrimSpace(l[1:])
			if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") || t == "*" || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "*/") || t == "#" || strings.HasPrefix(t, "# ") || strings.HasPrefix(t, "#!") {
				continue
			}
			for _, c := range []string{" //", "\t//", " # ", "\t# "} {
				if i := strings.Index(t, c); i >= 0 {
					t = t[:i]
				}
			}
			sb.WriteString(t)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// applyCarried gives u the review an earlier run wrote for it. The bucket
// and the score are not copied: impact and likelihood were measured again
// for this run, so afterReview places the unit from the fresh prior and
// the kept issues.
func applyCarried(u, old *Unit, from string) {
	u.Summary, u.Headline = old.Summary, old.Headline
	u.Focus = append([]string(nil), old.Focus...)
	u.Issues = CarryIssues(old, u)
	u.Reviewed = true
	u.CarriedFrom = from
	// A dismissal is re-applied when the result is loaded, from the
	// repository's own record, so it must not be carried as a fact here.
	for i := range u.Issues {
		u.Issues[i].Dismissed, u.Issues[i].DismissedWhy, u.Issues[i].DismissKey = false, "", ""
	}
}

// CarryIssues is a copy of old's issues for u, a later version of the
// same unit, with each line moved to where u has it. A unit keeps its
// review when its diff lines are unchanged, not its line numbers: code
// added or removed above it moves the whole unit, and an issue left at
// its old line points into whatever sits there now.
func CarryIssues(old, u *Unit) []Issue {
	out := append([]Issue(nil), old.Issues...)
	for i := range out {
		out[i].Line = rebaseLine(out[i].Line, old.Hunks, u.Hunks)
	}
	return out
}

// rebaseLine moves a new-file line from the coordinates of from's hunks to
// those of to's. The two show the same lines, so their hunks correspond in
// order: a line inside a hunk keeps its offset in it, counted over the
// new-side lines (context and added), and a line outside all of them moves
// with the hunk before it, or with the first hunk when it is above them.
// Hunks that do not correspond (not the same number) leave line as it is.
func rebaseLine(line int, from, to []Hunk) int {
	if line <= 0 || len(from) == 0 || len(from) != len(to) {
		return line
	}
	delta := to[0].NewStart - from[0].NewStart
	for i, h := range from {
		if line < h.NewStart {
			break
		}
		n, m := newSideLines(h), newSideLines(to[i])
		if off := line - h.NewStart; off < n {
			return to[i].NewStart + min(off, max(m-1, 0))
		}
		delta = to[i].NewStart + m - (h.NewStart + n)
	}
	return max(line+delta, 1)
}

// newSideLines counts the lines a hunk shows of the new file.
func newSideLines(h Hunk) int {
	n := 0
	for _, l := range h.Lines {
		if l == "" || l[0] == ' ' || l[0] == '+' {
			n++
		}
	}
	return n
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
