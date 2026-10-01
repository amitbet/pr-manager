package triage

import (
	"context"
	"fmt"
	"strings"

	"github.com/amitbet/pr-manager/llm"
)

// Units are reviewed in parallel, each call seeing only its own diff, so
// one defect is often reported from every unit it touches: a race from
// the caller and the callee, a missing check once per test that shows it.
// Comments on GitHub repeat review issues the same way. Dedupe finds the
// repeats after the fact and links each to the one issue that stands for
// them. A repeat stays on the record, but only the issue it points at
// counts.

// IssueRef names an issue by its unit and title. A title, not an index,
// because a later review of either unit can reorder its issues.
type IssueRef struct {
	Unit  string `json:"unit"`
	Title string `json:"title"`
}

// dedupeLineGap is how close two claims on one file must be to be
// compared at all.
const dedupeLineGap = 12

// dedupeMaxClaims caps the claims sent in the one dedupe call.
const dedupeMaxClaims = 80

// claim is one finding the dedupe pass compares: a review issue
// (issue >= 0) or a confirmed review comment (thread set).
type claim struct {
	u      *Unit
	issue  int
	thread *Thread
}

func (c claim) is() Issue {
	if c.thread != nil {
		return *c.thread.Issue
	}
	return c.u.Issues[c.issue]
}

func (c claim) compared() bool {
	if c.thread != nil {
		return c.thread.Compared
	}
	return c.u.Issues[c.issue].Compared
}

func (c claim) setCompared() {
	if c.thread != nil {
		c.thread.Compared = true
	} else {
		c.u.Issues[c.issue].Compared = true
	}
}

// Dedupe links the repeated findings across units and re-places every
// unit under tp. Only claims not compared before are asked about, so a
// re-run or a thread refresh that brought nothing new makes no call. A
// failed call links nothing and leaves the claims to be compared next time.
func (s *Summarizer) Dedupe(ctx context.Context, units []*Unit, tp TierPolicy) {
	model := s.Critic
	if model == nil {
		model = s.LLM
	}
	byID := unitIndex(units)
	resolveSameAs(byID)
	claims := dedupeClaims(units)
	groups := candidateGroups(claims)
	if len(groups) == 0 || model == nil {
		if model != nil {
			for _, c := range claims {
				c.setCompared()
			}
		}
		return
	}
	ids, prompt := dedupePrompt(groups)
	args, _, err := llm.CallTool(ctx, model, []llm.ChatMessage{
		{Role: "system", Content: dedupeSystem},
		{Role: "user", Content: prompt},
	}, dedupeTool, reviewMaxTokens)
	if err != nil {
		return
	}
	for _, set := range decodeSets(args, ids) {
		linkSet(set)
	}
	for _, c := range claims {
		c.setCompared()
	}
	resolveSameAs(byID)
	for _, u := range units {
		tp.ApplyDismissals(u)
	}
}

func unitIndex(units []*Unit) map[string]*Unit {
	m := make(map[string]*Unit, len(units))
	for _, u := range units {
		m[u.ID] = u
	}
	return m
}

// findIssue is the index of the issue titled title on u, or -1.
func findIssue(u *Unit, title string) int {
	if u == nil {
		return -1
	}
	for i, is := range u.Issues {
		if is.Title == title {
			return i
		}
	}
	return -1
}

// rootOf follows SameAs links from u's issue i to the issue that stands
// for it, or reports false when a link leads nowhere or round in a loop.
func rootOf(byID map[string]*Unit, u *Unit, i int) (*Unit, int, bool) {
	for range 16 {
		ref := u.Issues[i].SameAs
		if ref == nil {
			return u, i, true
		}
		u = byID[ref.Unit]
		if i = findIssue(u, ref.Title); i < 0 {
			return nil, -1, false
		}
	}
	return nil, -1, false
}

// resolveSameAs points every link at the issue that stands for it, and
// drops the links whose issue is gone (its unit was reviewed again and did
// not raise it). An issue whose link was dropped counts on its own again
// and is compared afresh. Comments that repeat an issue that is now a
// repeat itself are pointed at its root too.
func resolveSameAs(byID map[string]*Unit) {
	for _, u := range byID {
		for i := range u.Issues {
			is := &u.Issues[i]
			if is.SameAs == nil {
				continue
			}
			ru, ri, ok := rootOf(byID, u, i)
			if !ok || (ru == u && ri == i) {
				is.SameAs, is.Compared = nil, false
				continue
			}
			is.SameAs = &IssueRef{Unit: ru.ID, Title: ru.Issues[ri].Title}
		}
	}
	for _, u := range byID {
		for i := range u.Threads {
			t := &u.Threads[i]
			if t.DuplicateOf == nil {
				continue
			}
			target := u
			if t.DuplicateUnit != "" {
				target = byID[t.DuplicateUnit]
			}
			j := *t.DuplicateOf
			if target == nil || j < 0 || j >= len(target.Issues) {
				continue // AssignThreads resolves what it can and drops the rest
			}
			if ru, ri, ok := rootOf(byID, target, j); ok && (ru != target || ri != j) {
				pointThread(u, t, ru, ri)
			}
		}
	}
}

// pointThread links comment t on u to issue i of target.
func pointThread(u *Unit, t *Thread, target *Unit, i int) {
	t.DuplicateOf, t.DuplicateTitle, t.DuplicateUnit = &i, target.Issues[i].Title, ""
	if target != u {
		t.DuplicateUnit = target.ID
	}
}

// dedupeClaims are the findings that still count on their own: review
// issues nobody dismissed or linked, and confirmed open comments that do
// not already repeat an issue.
func dedupeClaims(units []*Unit) []claim {
	var out []claim
	for _, u := range units {
		for i, is := range u.Issues {
			if is.Live() {
				out = append(out, claim{u: u, issue: i})
			}
		}
		for i := range u.Threads {
			t := &u.Threads[i]
			if t.Status == ThreadValid && !t.Fixed && t.Issue != nil && t.DuplicateOf == nil {
				out = append(out, claim{u: u, issue: -1, thread: t})
			}
		}
	}
	return out
}

// candidateGroups joins claims that might be the same finding: close
// together in one file, quoting the same code, or with titles that share
// most of their words. Only groups with a review issue (a comment can only
// repeat an issue) and a claim not compared before are kept, up to
// dedupeMaxClaims claims in all.
func candidateGroups(claims []claim) [][]claim {
	parent := make([]int, len(claims))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	words := make([]map[string]bool, len(claims))
	for i, c := range claims {
		words[i] = wordSet(c.is().Title)
	}
	for i := range claims {
		for j := i + 1; j < len(claims); j++ {
			if (!claims[i].compared() || !claims[j].compared()) && mayRepeat(claims[i], claims[j], words[i], words[j]) {
				parent[find(i)] = find(j)
			}
		}
	}
	byRoot := map[int][]claim{}
	var order []int
	for i, c := range claims {
		r := find(i)
		if byRoot[r] == nil {
			order = append(order, r)
		}
		byRoot[r] = append(byRoot[r], c)
	}
	var out [][]claim
	n := 0
	for _, r := range order {
		g := byRoot[r]
		hasIssue, fresh := false, false
		for _, c := range g {
			hasIssue = hasIssue || c.thread == nil
			fresh = fresh || !c.compared()
		}
		if len(g) < 2 || !hasIssue || !fresh || n+len(g) > dedupeMaxClaims {
			continue
		}
		out = append(out, g)
		n += len(g)
	}
	return out
}

func mayRepeat(a, b claim, wa, wb map[string]bool) bool {
	ia, ib := a.is(), b.is()
	fa, fb := claimFile(a), claimFile(b)
	if fa == fb && ia.Line > 0 && ib.Line > 0 && max(ia.Line-ib.Line, ib.Line-ia.Line) <= dedupeLineGap {
		return true
	}
	if ea := normalizeCode(ia.Evidence); ea != "" && ea == normalizeCode(ib.Evidence) {
		return true
	}
	return titleOverlap(wa, wb) >= 0.5
}

func claimFile(c claim) string {
	if c.thread != nil && c.thread.Path != "" {
		return c.thread.Path
	}
	return c.u.File
}

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(normalizeWords(s)) {
		m[w] = true
	}
	return m
}

// titleOverlap is the share of the smaller title's words the other has too.
func titleOverlap(a, b map[string]bool) float64 {
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(a) < 2 {
		return 0
	}
	n := 0
	for w := range a {
		if b[w] {
			n++
		}
	}
	return float64(n) / float64(len(a))
}

const dedupeSystem = `You merge duplicate findings on one pull request. Its change units were reviewed in parallel, each review seeing only its own part, so one defect is often reported several times: from each unit it touches, or once by the review and once by a person's comment on the PR.

The findings come in candidate groups of ones that look alike. Within a group, say which findings are the same problem. Two findings are the same when they describe one root cause and one fix would resolve both, even if they are worded differently, anchored to different lines or files, or rated at different severities. They are not the same when they share a line or a theme but fail in different ways, or when they are the same kind of mistake made in two places that each need their own fix.

Return each set of two or more findings that are the same, by their ids. Leave out findings with no duplicate. When unsure, leave them apart: a missed duplicate costs a reader a minute, a wrong merge hides a defect.` + untrustedData

var dedupeTool = llm.ToolDefinition{
	Name:        "merge_duplicates",
	Description: "Submit the sets of findings that describe the same problem.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"sets": map[string]any{
				"type":        "array",
				"description": "One entry per problem reported more than once. Empty when there are no duplicates.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"ids":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Two or more finding ids from one group."},
						"reason": map[string]any{"type": "string", "description": "The shared root cause, in one sentence."},
					},
					"required": []string{"ids"},
				},
			},
		},
		"required": []string{"sets"},
	},
}

// dedupePrompt numbers the claims f1, f2… across the groups and lays each
// one out with where it is and what it claims.
func dedupePrompt(groups [][]claim) (map[string]claim, string) {
	ids := map[string]claim{}
	var b strings.Builder
	n := 0
	for g, group := range groups {
		fmt.Fprintf(&b, "Group %d:\n", g+1)
		for _, c := range group {
			n++
			id := fmt.Sprintf("f%d", n)
			ids[id] = c
			is := c.is()
			where := claimFile(c)
			if c.u.Symbol != "" {
				where += " (" + c.u.Symbol + ")"
			}
			if is.Line > 0 {
				where += fmt.Sprintf(" line %d", is.Line)
			}
			from := "review"
			if c.thread != nil {
				from = "comment by @" + c.thread.Author
			}
			fmt.Fprintf(&b, "- %s [%s, %s] %s: %s\n", id, from, is.Severity, where, is.Title)
			if is.Detail != "" {
				fmt.Fprintf(&b, "  %s\n", clipRunes(is.Detail, 600))
			}
			if is.Scenario != "" {
				fmt.Fprintf(&b, "  When: %s\n", clipRunes(is.Scenario, 400))
			}
			if is.Evidence != "" {
				fmt.Fprintf(&b, "  Code: %s\n", clipRunes(normalizeCode(is.Evidence), 300))
			}
		}
		b.WriteByte('\n')
	}
	return ids, b.String()
}

// decodeSets reads the model's sets, keeping known ids once each; a
// finding put in two sets stays in the first.
func decodeSets(args map[string]any, ids map[string]claim) [][]claim {
	list, _ := args["sets"].([]any)
	used := map[string]bool{}
	var out [][]claim
	for _, x := range list {
		m, _ := x.(map[string]any)
		raw, _ := m["ids"].([]any)
		var set []claim
		for _, r := range raw {
			id, _ := r.(string)
			id = strings.TrimSpace(id)
			if c, ok := ids[id]; ok && !used[id] {
				used[id] = true
				set = append(set, c)
			}
		}
		if len(set) > 1 {
			out = append(out, set)
		}
	}
	return out
}

// linkSet makes one review issue of set stand for the rest: the most
// severe, so linking never lowers what the PR counts, and of those one
// that was already compared, so a standing issue stays the one that
// stands when a new repeat of it arrives. A set of comments alone links
// nothing: a comment can only repeat an issue.
func linkSet(set []claim) {
	best := -1
	for i, c := range set {
		if c.thread != nil {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		w, bw := severityWeight[c.is().Severity], severityWeight[set[best].is().Severity]
		if w > bw || (w == bw && c.compared() && !set[best].compared()) {
			best = i
		}
	}
	if best < 0 {
		return
	}
	root := set[best]
	ref := IssueRef{Unit: root.u.ID, Title: root.is().Title}
	for i, c := range set {
		if i == best {
			continue
		}
		if c.thread != nil {
			pointThread(c.u, c.thread, root.u, root.issue)
			continue
		}
		r := ref
		c.u.Issues[c.issue].SameAs = &r
	}
}

// Repeats are the issues linked to unit's issue titled title.
func Repeats(units []*Unit, unit, title string) []IssueAt {
	var out []IssueAt
	for _, u := range units {
		for i, is := range u.Issues {
			if is.SameAs != nil && is.SameAs.Unit == unit && is.SameAs.Title == title {
				out = append(out, IssueAt{Unit: u, Index: i})
			}
		}
	}
	return out
}

// IssueAt is one issue of a unit.
type IssueAt struct {
	Unit  *Unit
	Index int
}
