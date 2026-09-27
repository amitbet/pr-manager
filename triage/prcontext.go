package triage

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/amitbet/pr-manager/codemap/decls"
)

// DefaultReviewContextChars caps the other units' diffs added to each
// review prompt.
const DefaultReviewContextChars = 32000

// minMovedLines is how many added lines must match removed lines of another
// unit before the added code counts as moved from there.
const minMovedLines = 3

// setReviewContext gives every unit with a diff the rest of the PR as the
// reviewer should see it: notes on moved and new code, then the other
// units' diffs, most related first, until budget characters are used.
// Without it the reviewer judges each unit alone. It flags behavior that
// only moved as new, and rates impact without the code that limits it.
func setReviewContext(units []*Unit, base ContentFunc, budget int) {
	baseDecls := baseDeclNames(units, base)
	lines := map[*Unit]changed{}
	for _, u := range units {
		lines[u] = changedText(u)
	}
	for _, u := range units {
		if len(u.Hunks) == 0 {
			continue
		}
		u.ReviewContext = buildContext([]*Unit{u}, units, baseDecls, lines, budget)
	}
}

// buildContext renders the rest of the PR for one review task. The units
// in members are its focus and are left out; everything else is context,
// most related first, until budget characters are used. One member
// produces exactly the per-unit context; several share the budget instead
// of spending it once each, and their notes carry the unit ID.
func buildContext(members, units []*Unit, baseDecls map[string]map[string]bool, lines map[*Unit]changed, budget int) string {
	member := make(map[*Unit]bool, len(members))
	for _, u := range members {
		member[u] = true
	}
	label := func(u *Unit) string {
		if len(members) == 1 {
			return ""
		}
		return u.ID + ": "
	}
	var notes []string
	moved := map[*Unit]int{}
	for _, u := range members {
		switch {
		case u.Status == StatusAdded:
			notes = append(notes, label(u)+"This file is new in the PR.")
		case u.Symbol != "" && baseDecls[u.File] != nil && !baseDecls[u.File][u.Symbol]:
			notes = append(notes, label(u)+fmt.Sprintf("%s does not exist at the merge base: it is new or renamed. Removed lines in the other units show what it replaced.", u.Symbol))
		}
		for _, o := range units {
			if member[o] || !reviewable(o) {
				continue
			}
			n, sample := overlap(lines[u].added, lines[o].removed)
			if n >= minMovedLines {
				moved[o] = max(moved[o], n)
				notes = append(notes, label(u)+fmt.Sprintf("%d added lines here were removed from %s in this PR (e.g. `%s`): that code moved, so the behavior it carries is not new.", n, o.ID, sample))
			}
		}
	}
	var sb strings.Builder
	var hidden []string
	left := budget
	for _, o := range rankRelated(members, units, moved) {
		d := o.Diff()
		if budget > 0 && len(d) > left {
			hidden = append(hidden, o.ID)
			continue
		}
		left -= len(d)
		fmt.Fprintf(&sb, "\n#### %s (%s)\n```diff\n%s\n```\n", o.ID, o.Status, d)
	}
	for _, o := range units {
		if !member[o] && !reviewable(o) {
			hidden = append(hidden, o.ID)
		}
	}
	noun := "the unit above"
	heading := "\nNotes on this unit:\n"
	if len(members) > 1 {
		noun = "the units above"
		heading = "\nNotes on these units:\n"
	}
	var out strings.Builder
	if len(notes) > 0 {
		out.WriteString(heading)
		for _, n := range notes {
			out.WriteString("- " + n + "\n")
		}
	}
	if sb.Len() > 0 {
		fmt.Fprintf(&out, "\nOther changes in the same PR, for context. Review only %s, but use these to judge it: a caller or fallback here may limit or cause a problem.\n", noun)
		out.WriteString(sb.String())
	}
	if len(hidden) > 0 {
		sort.Strings(hidden)
		fmt.Fprintf(&out, "\nAlso changed, not shown: %s\n", strings.Join(hidden, ", "))
	}
	return out.String()
}

// reviewable reports whether another unit's diff is worth showing: rule
// skips (generated files, formatting) are noise.
func reviewable(u *Unit) bool {
	return len(u.Hunks) > 0 && !(u.Decision.Source == "rule" && u.Decision.Bucket == BucketNone)
}

// rankRelated orders the units outside members: code moved from them,
// then units that name a member or that a member names (callers and
// callees), then the same file, then the rest, keeping PR order inside
// each group. A unit any member ranks highly is shown early.
func rankRelated(members, units []*Unit, moved map[*Unit]int) []*Unit {
	member := make(map[*Unit]bool, len(members))
	names := make([]string, len(members))
	diffs := make([]string, len(members))
	for i, u := range members {
		member[u] = true
		names[i] = shortName(u.Symbol)
		diffs[i] = u.Diff()
	}
	rank := func(o *Unit) int {
		if moved[o] > 0 {
			return 0
		}
		oDiff, oName := o.Diff(), shortName(o.Symbol)
		best := 3
		for i, u := range members {
			switch {
			case mentions(oDiff, names[i]) || mentions(diffs[i], oName):
				return 1
			case o.File == u.File:
				best = min(best, 2)
			}
		}
		return best
	}
	var out []*Unit
	for _, o := range units {
		if !member[o] && reviewable(o) {
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}

// shortName is the identifier other code uses for a unit's symbol:
// "(*T).M" → "M", "type T" → "T", "var x" → "x". "" for import blocks.
func shortName(sym string) string {
	if sym == "" || sym == "imports" {
		return ""
	}
	f := strings.Fields(sym)
	sym = f[len(f)-1]
	// (*T).M, Class.method: the member name.
	if i := strings.LastIndex(sym, "."); i >= 0 {
		sym = sym[i+1:]
	}
	return sym
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func mentions(text, name string) bool {
	if len(name) < 3 || !identRe.MatchString(name) {
		return false
	}
	return regexp.MustCompile(`\b` + name + `\b`).MatchString(text)
}

type changed struct{ added, removed map[string]bool }

// changedText collects a unit's added and removed lines, trimmed, leaving
// out lines too short or generic to show that code moved.
func changedText(u *Unit) changed {
	c := changed{map[string]bool{}, map[string]bool{}}
	for _, h := range u.Hunks {
		for _, l := range h.Lines {
			if len(l) == 0 {
				continue
			}
			t := strings.TrimSpace(l[1:])
			if len(t) < 12 || strings.HasPrefix(t, "//") {
				continue
			}
			switch l[0] {
			case '+':
				c.added[t] = true
			case '-':
				c.removed[t] = true
			}
		}
	}
	return c
}

// overlap counts lines in both sets and returns the longest as a sample.
func overlap(a, b map[string]bool) (int, string) {
	n, sample := 0, ""
	for l := range a {
		if b[l] {
			n++
			if len(l) > len(sample) || (len(l) == len(sample) && l < sample) {
				sample = l
			}
		}
	}
	return n, sample
}

// baseDeclNames lists the declarations at the merge base of each modified
// source file with units, so new declarations can be told apart. Files it
// can't read are left out (nil: unknown).
func baseDeclNames(units []*Unit, base ContentFunc) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	if base == nil {
		return out
	}
	seen := map[string]bool{}
	for _, u := range units {
		if seen[u.File] || !(strings.HasSuffix(u.File, ".go") || decls.HasDecls(u.File)) || u.Status != StatusModified {
			continue
		}
		seen[u.File] = true
		src, err := base(u.File)
		if err != nil {
			continue
		}
		ds := declRanges(u.File, src)
		if ds == nil {
			continue
		}
		names := map[string]bool{}
		for _, d := range ds {
			names[d.name] = true
		}
		out[u.File] = names
	}
	return out
}
