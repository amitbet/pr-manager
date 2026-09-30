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
// unit it is on, the code it quotes and its severity, not the model's
// wording, which changes between runs. Pattern identifies the kind of
// claim, from the words of its title with the code and the noise taken
// out. Only Key is matched today; Pattern is stored so the same records
// can later answer "this reviewer has been told three times that
// unchecked writes to a buffer are fine here" without asking anyone to
// label anything twice.

// IssueKey identifies one review issue across re-runs of the same PR and
// across the pushes that follow. The quoted evidence anchors it to the
// code, so a reworded title is still the same issue; without evidence the
// title has to stand in. The severity is part of the key so that two
// claims about the same line stay apart: dismissing a low note that quotes
// `return err` must not also silence a critical one quoting it. Matching
// (issueKeys) lets a dismissal cover the same claim at its own severity or
// lower, so a re-rating by the critic only brings it back if it got worse.
func IssueKey(unit string, is Issue) string {
	return digest("issue", unit, issueAnchor(is), is.Severity)
}

// legacyIssueKey is the key earlier versions stored, without the severity.
// Those records still match, at any severity, since what was dismissed at
// which severity is not recoverable from the key alone.
func legacyIssueKey(unit string, is Issue) string {
	return digest("issue", unit, issueAnchor(is))
}

func issueAnchor(is Issue) string {
	if anchor := normalizeCode(is.Evidence); anchor != "" {
		return anchor
	}
	return normalizeWords(is.Title)
}

// severityOrder ranks severities from least to most severe.
var severityOrder = []string{"low", "medium", "high", "critical"}

// issueKeys are the keys under which a dismissal covers is: the claim at
// its own severity or any worse one, then the legacy key. A severity
// outside the known ones only matches itself.
func issueKeys(unit string, is Issue) []string {
	var keys []string
	at := -1
	for i, s := range severityOrder {
		if s == is.Severity {
			at = i
		}
	}
	if at < 0 {
		keys = append(keys, IssueKey(unit, is))
	} else {
		for _, s := range severityOrder[at:] {
			x := is
			x.Severity = s
			keys = append(keys, IssueKey(unit, x))
		}
	}
	return append(keys, legacyIssueKey(unit, is))
}

// IssueKeys are the keys a dismissal of is may be stored under (see
// issueKeys), so restoring it can drop every record that hides it.
func IssueKeys(unit string, is Issue) []string {
	return issueKeys(unit, is)
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
			u.Issues[i].Dismissed, u.Issues[i].DismissedWhy, u.Issues[i].DismissKey = false, "", ""
			for _, key := range issueKeys(u.ID, u.Issues[i]) {
				if why, ok := dismissed(key); ok {
					u.Issues[i].Dismissed, u.Issues[i].DismissedWhy, u.Issues[i].DismissKey = true, why, key
					n++
					break
				}
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
