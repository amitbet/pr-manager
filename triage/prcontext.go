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

// A unit's added code counts as moved from another unit only when the match
// is more than a few stock lines both happen to share: a run of at least
// minMovedLines consecutive distinctive added lines, or at least
// movedShare of them.
const movedShare = 0.8

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
			n, m, sample, ok := movedFrom(lines[u], lines[o])
			if ok {
				moved[o] = max(moved[o], n)
				rest := fmt.Sprintf("the other %d are new. The matching part is code that moved, so the behavior it carries is not new; judge the rest as new code.", m-n)
				if n == m {
					rest = "none are new. This is code that moved, so the behavior it carries is not new."
				}
				notes = append(notes, label(u)+fmt.Sprintf("%d of the %d distinctive added lines here match lines removed from %s in this PR (e.g. `%s`); %s", n, m, o.ID, sample, rest))
			}
		}
		if claims := absoluteClaims(u); len(claims) > 0 {
			notes = append(notes, label(u)+"These added comment or doc lines make absolute claims: "+quoteClaims(claims)+". Try to break each one: pick an input that meets the claim's condition and trace what the code does with it. A claim the code breaks is a defect, with that input as the failure scenario; if only the wording overstates correct code, it is low at most. A claim you cannot break is not an issue.")
		}
	}
	var sb strings.Builder
	var hidden []string
	left := budget
	for _, o := range rankRelated(members, units, moved, lines) {
		d := lines[o].diff
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
// each group. A unit any member ranks highly is shown early. lines holds
// every unit's precomputed text (see changedText).
func rankRelated(members, units []*Unit, moved map[*Unit]int, lines map[*Unit]changed) []*Unit {
	member := make(map[*Unit]bool, len(members))
	for _, u := range members {
		member[u] = true
	}
	rank := func(o *Unit) int {
		if moved[o] > 0 {
			return 0
		}
		oc := lines[o]
		best := 3
		for _, u := range members {
			uc := lines[u]
			switch {
			case mentions(oc.diff, uc.name) || mentions(uc.diff, oc.name):
				return 1
			case o.File == u.File:
				best = min(best, 2)
			}
		}
		return best
	}
	var out []*Unit
	var keys []int
	for _, o := range units {
		if !member[o] && reviewable(o) {
			out = append(out, o)
			keys = append(keys, rank(o))
		}
	}
	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool { return keys[idx[i]] < keys[idx[j]] })
	sorted := make([]*Unit, len(out))
	for i, k := range idx {
		sorted[i] = out[k]
	}
	return sorted
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

// mentions reports whether text contains name as a whole word (as the
// regexp \bname\b would, with ASCII word characters).
func mentions(text, name string) bool {
	if len(name) < 3 || !identRe.MatchString(name) {
		return false
	}
	for off := 0; ; {
		i := strings.Index(text[off:], name)
		if i < 0 {
			return false
		}
		i += off
		end := i + len(name)
		if (i == 0 || !isWordByte(text[i-1])) && (end == len(text) || !isWordByte(text[end])) {
			return true
		}
		off = i + 1
	}
}

func isWordByte(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// changed is a unit's text as buildContext compares it: added and removed
// lines, the whole diff and the symbol's short name, computed once.
type changed struct {
	added, removed map[string]bool
	// addedSeq is the added lines kept in added, in diff order, repeats
	// included.
	addedSeq   []string
	diff, name string
}

// changedText collects a unit's added and removed lines, trimmed, leaving
// out lines too short or generic to show that code moved.
func changedText(u *Unit) changed {
	c := changed{added: map[string]bool{}, removed: map[string]bool{}, diff: u.Diff(), name: shortName(u.Symbol)}
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
				c.addedSeq = append(c.addedSeq, t)
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

// movedFrom reports whether u's added code moved from o: n of u's m
// distinctive added lines (see boilerplate) were removed from o, forming a
// run of minMovedLines in a row or movedShare of all m. Stock lines such as
// `if err != nil {` or `return nil, err` match anywhere, so they count
// toward neither.
func movedFrom(u, o changed) (n, m int, sample string, ok bool) {
	seen := map[string]bool{}
	run, best := 0, 0
	for _, l := range u.addedSeq {
		if boilerplate(l) {
			continue
		}
		if !seen[l] {
			seen[l] = true
			m++
		}
		if !o.removed[l] {
			run = 0
			continue
		}
		run++
		best = max(best, run)
	}
	for l := range seen {
		if o.removed[l] {
			n++
			if len(l) > len(sample) || (len(l) == len(sample) && l < sample) {
				sample = l
			}
		}
	}
	ok = n >= minMovedLines && (best >= minMovedLines || float64(n) >= movedShare*float64(m))
	return n, m, sample, ok
}

// stockWords are the identifiers of lines every function repeats: error
// checks, returns of zero values, closing an else.
var stockWords = map[string]bool{
	"if": true, "else": true, "return": true, "err": true, "error": true, "nil": true, "null": true,
	"None": true, "undefined": true, "true": true, "false": true, "True": true, "False": true,
	"ok": true, "ctx": true, "break": true, "continue": true, "defer": true, "cancel": true,
	"try": true, "catch": true, "except": true, "finally": true, "raise": true, "throw": true,
	"pass": true, "self": true, "this": true, "e": true, "ex": true, "Exception": true,
	"new": true, "void": true, "await": true, "async": true, "fmt": true, "Errorf": true,
	"errors": true, "Is": true, "As": true, "string": true, "int": true, "bool": true,
}

var identWord = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// boilerplate reports whether a line holds nothing but stock words and
// punctuation, so it matching a removed line says nothing about where it
// came from.
func boilerplate(l string) bool {
	for _, w := range identWord.FindAllString(l, -1) {
		if !stockWords[w] {
			return false
		}
	}
	return true
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
