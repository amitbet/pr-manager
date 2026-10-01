package main

// A new triage of a PR reviews its new head afresh, and an issue the push
// fixed is simply not found again: it would vanish without a word. The
// result keeps what the earlier review raised and this one no longer
// does, as resolved, and the Issues tab lists it at the bottom.

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

// resolvedIssue is an issue an earlier review of the PR raised that a
// later one, of other code, no longer has.
type resolvedIssue struct {
	UnitID string       `json:"unit_id"`
	File   string       `json:"file"`
	Issue  triage.Issue `json:"issue"`
	// Since is the head (or, for a local review, the result key) the
	// issue was last raised on, and At when.
	Since string    `json:"since"`
	At    time.Time `json:"at"`
	// Gone: the unit is no longer in the PR's diff at all.
	Gone bool `json:"gone,omitempty"`
}

// earlierReview is the newest review of the same change before key's,
// of other code, at any settings: English, and not a fix's.
func (t *triager) earlierReview(key string) *PRResult {
	prefix, content, _, ok := splitKey(key)
	if !ok {
		return nil
	}
	paths, _ := filepath.Glob(filepath.Join(t.results, prefix+"__*.json"))
	var best *PRResult
	for _, p := range paths {
		k := strings.TrimSuffix(filepath.Base(p), ".json")
		if pf, c, _, ok := splitKey(k); !ok || pf != prefix || c == content {
			continue
		}
		r, err := t.Load(k)
		if err != nil || r.SummaryLang != "" || r.PR == nil || r.LocalFixDir != "" {
			continue
		}
		if best == nil || r.CreatedAt.After(best.CreatedAt) {
			best = r
		}
	}
	return best
}

// resolvedSince is what prev raised, and the issues it already had as
// resolved, that r no longer has. Only a unit r reviewed afresh can say
// so: a unit that kept prev's review has its issues as they were, and one
// left unreviewed says nothing. When the two ran with other settings, a
// unit whose diff did not move is left out too: a different model
// raising other things is not a fix. A unit no longer in the diff took
// its issues with it.
func resolvedSince(prev, r *PRResult) []resolvedIssue {
	_, _, ps, _ := splitKey(prev.Key)
	_, _, rs, _ := splitKey(r.Key)
	now := unitsByID(r)
	was := unitsByID(prev)
	since := prev.Key
	if prev.PR.LocalPath == "" {
		since = prev.PR.HeadOid
	}
	// still is whether is, of unitID, is open in r. A unit that can't
	// answer counts as still having it.
	still := func(unitID string, is triage.Issue) (open, gone bool) {
		u := now[unitID]
		if u == nil {
			return false, true
		}
		if !u.Reviewed || u.CarriedFrom != "" {
			return true, false
		}
		if o := was[unitID]; ps != rs && (o == nil || triage.SameDiff(o, u)) {
			return true, false
		}
		for _, n := range u.Issues {
			if sameClaim(unitID, is, n) {
				return true, false
			}
		}
		return false, false
	}
	var out []resolvedIssue
	seen := map[string]bool{}
	add := func(x resolvedIssue) {
		s := issueScope(x.UnitID, x.Issue)
		if seen[s] {
			return
		}
		if open, gone := still(x.UnitID, x.Issue); !open {
			seen[s] = true
			x.Gone = gone
			out = append(out, x)
		}
	}
	for _, f := range prev.Files {
		for _, u := range f.Units {
			for _, is := range u.Issues {
				if !is.Dismissed {
					add(resolvedIssue{UnitID: u.ID, File: f.Path, Issue: is, Since: since, At: prev.CreatedAt})
				}
			}
		}
	}
	// Resolved before, and not raised again.
	for _, x := range prev.Resolved {
		s := issueScope(x.UnitID, x.Issue)
		if seen[s] {
			continue
		}
		back := false
		if u := now[x.UnitID]; u != nil {
			for _, n := range u.Issues {
				back = back || sameClaim(x.UnitID, x.Issue, n)
			}
		}
		if !back {
			seen[s] = true
			out = append(out, x)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Issue.Line < b.Issue.Line
	})
	return out
}

// sameClaim is whether n, from a review of other code, is a found again:
// the same quoted code or title, or a title mostly of the same words. A
// reworded claim about code that moved should not pass for a fix.
func sameClaim(unitID string, a, n triage.Issue) bool {
	if issueScope(unitID, a) == issueScope(unitID, n) {
		return true
	}
	wa, wn := strings.Fields(triage.IssuePattern(a)), strings.Fields(triage.IssuePattern(n))
	if len(wa) == 0 || len(wn) == 0 {
		return false
	}
	in := map[string]bool{}
	for _, w := range wa {
		in[w] = true
	}
	both := 0
	for _, w := range wn {
		if in[w] {
			both++
		}
	}
	return float64(both)/float64(len(wa)+len(wn)-both) >= 0.5
}
