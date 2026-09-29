package triage

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

// A dismissal is a person telling the reviewer it was wrong. Keeping that
// is what separates a tool people keep using from one they stop reading:
// the same issue should not come back on the next push, and the unit it
// was holding in human review should fall back to where it belongs.
//
// Two keys are derived from every issue. Key identifies this issue: the
// unit it is on and the code it quotes, not the model's wording, which
// changes between runs. Pattern identifies the kind of claim, from the
// words of its title with the code and the noise taken out. Only Key is
// matched today; Pattern is stored so the same records can later answer
// "this reviewer has been told three times that unchecked writes to a
// buffer are fine here" without asking anyone to label anything twice.

// IssueKey identifies one review issue across re-runs of the same PR and
// across the pushes that follow. The quoted evidence anchors it to the
// code, so a reworded title is still the same issue; without evidence the
// title has to stand in.
func IssueKey(unit string, is Issue) string {
	anchor := normalizeCode(is.Evidence)
	if anchor == "" {
		anchor = normalizeWords(is.Title)
	}
	// The severity is left out on purpose: the critic re-rates it on every
	// run, and a dismissal must survive the same claim coming back at a
	// different severity. It also keeps keys saved by earlier versions valid.
	return digest("issue", unit, anchor)
}

// LintKey identifies one static-analysis finding. The rule and the line's
// message are the whole claim, so they are the key: the same rule firing
// again on the same unit is the same finding.
func LintKey(unit string, f LintFinding) string {
	return digest("lint", unit, f.Label(), normalizeWords(f.Message))
}

// IssuePattern is the shape of the claim, for matching issues that are
// not literally the same one. Unused by the matching today; see the note
// above.
func IssuePattern(is Issue) string {
	return normalizeWords(is.Title)
}

func digest(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:8])
}

// normalizeCode collapses the whitespace of a quoted line so indentation
// and line wrapping do not make a new issue out of an old one.
func normalizeCode(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

var wordRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]+`)

// normalizeWords reduces a sentence to its significant words, sorted, so
// "Retry loop can spin forever" and "Loop retries forever" share most of
// their shape. Numbers, punctuation and filler words drop out.
func normalizeWords(s string) string {
	var out []string
	seen := map[string]bool{}
	for _, w := range wordRe.FindAllString(strings.ToLower(s), -1) {
		if len(w) < 3 || stopWords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "not": true, "but": true, "are": true,
	"was": true, "has": true, "have": true, "with": true, "this": true, "that": true,
	"when": true, "will": true, "can": true, "may": true, "from": true, "into": true,
	"its": true, "their": true, "than": true, "then": true, "does": true, "which": true,
}

// Dismissed says whether a key was dismissed and why.
type Dismissed func(key string) (string, bool)

// ApplyDismissed marks the issues and lint findings of units that a person
// has dismissed, and re-places every unit under tp. Nothing is deleted:
// the reviewer's claim and the reason it was rejected both stay on the
// record, which is what makes the dismissals worth learning from.
func ApplyDismissed(units []*Unit, tp TierPolicy, dismissed Dismissed) int {
	n := 0
	for _, u := range units {
		for i := range u.Issues {
			key := IssueKey(u.ID, u.Issues[i])
			why, ok := dismissed(key)
			u.Issues[i].Dismissed, u.Issues[i].DismissedWhy, u.Issues[i].DismissKey = ok, why, ""
			if ok {
				u.Issues[i].DismissKey = key
				n++
			}
		}
		for i := range u.Lint {
			key := LintKey(u.ID, u.Lint[i])
			why, ok := dismissed(key)
			u.Lint[i].Dismissed, u.Lint[i].DismissedWhy, u.Lint[i].DismissKey = ok, why, ""
			if ok {
				u.Lint[i].DismissKey = key
				n++
			}
		}
		tp.ApplyDismissals(u)
	}
	return n
}
