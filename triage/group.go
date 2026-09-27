package triage

import (
	"sort"
	"strings"
)

// DefaultGroupChars caps the focus diff of one review group. Groups are
// review tasks, not units: the cap bounds how much changed code a single
// review call has to hold in its head.
const DefaultGroupChars = 12000

// DefaultGroupMembers caps a group's unit count. The sweep in
// experiments/grouping-tuning found the character cap alone lets a chain of
// pairwise links pull 20+ units into one review, and that those reviews go
// quiet: uncapped arms reported 0 and 1 high-severity issues where capped
// ones reported 3, against 2 for ungrouped review.
const DefaultGroupMembers = 8

// smallPairChars is how small two changes in one file must be together
// before "same file" alone is enough to group them.
const smallPairChars = 1500

// GroupPolicy controls whether related units share one review call.
// Grouping cuts review calls and repeated context; it is not a recall
// feature (see experiments/hunk-grouping-prometheus-18905/FINDINGS.md).
//
// The two caps are the tuning knobs, both picked by the sweep over five
// public PRs in experiments/grouping-tuning. MaxChars bounds how much
// changed code one review holds; MaxMembers bounds how many units, which
// the character cap alone does not, because merging is single-linkage and
// chains.
type GroupPolicy struct {
	Enabled  bool `yaml:"enabled"`
	MaxChars int  `yaml:"max_chars"`
	// MaxMembers caps a group's unit count (0: no cap).
	MaxMembers int `yaml:"max_members"`
}

func DefaultGroupPolicy() GroupPolicy {
	return GroupPolicy{Enabled: true, MaxChars: DefaultGroupChars, MaxMembers: DefaultGroupMembers}
}

// ReviewGroup is one review call over units that are read together.
// A group of one behaves exactly like an ungrouped review.
type ReviewGroup struct {
	Members []*Unit
	// Context is the rest of the PR as the group's review prompt shows it,
	// the group equivalent of Unit.ReviewContext. Empty for single-member
	// groups, which use the member's own ReviewContext.
	Context string
}

func (g *ReviewGroup) ID() string {
	ids := make([]string, len(g.Members))
	for i, u := range g.Members {
		ids[i] = u.ID
	}
	return strings.Join(ids, " + ")
}

// BuildReviewGroups merges units that a reviewer should read together:
// units that name each other (caller and callee), a test named after the
// production symbol it exercises, and small changes in the same file.
// Merging is greedy from the strongest link down and stops at the policy's
// caps, so a large unit stays on its own. Units keep their input order,
// inside a group and between groups.
//
// Callers pass units that share a review prompt shape; see Pipeline.Run,
// which groups human units separately from the rest.
func BuildReviewGroups(units []*Unit, policy GroupPolicy) []*ReviewGroup {
	return buildGroups(units, defaultParams(policy))
}

// groupParams are the rule's tunables. Only the caps are configurable; the
// link weights are fixed, picked by the sweep in
// experiments/grouping-tuning.
type groupParams struct {
	maxChars int
	// maxMembers caps a group's unit count (0: no cap).
	maxMembers int
	// Link weights: a unit naming the other's symbol, a test named after
	// the other's symbol, and two changes in the same file that together
	// stay under smallPair characters.
	ref, test, sameFile, smallPair int
}

func defaultParams(policy GroupPolicy) groupParams {
	if policy.MaxChars <= 0 {
		policy.MaxChars = DefaultGroupChars
	}
	return groupParams{maxChars: policy.MaxChars, maxMembers: policy.MaxMembers,
		ref: 4, test: 3, sameFile: 1, smallPair: smallPairChars}
}

func buildGroups(units []*Unit, p groupParams) []*ReviewGroup {
	if len(units) < 2 {
		return soloGroups(units)
	}
	diff := make([]string, len(units))
	name := make([]string, len(units))
	for i, u := range units {
		diff[i] = u.Diff()
		name[i] = shortName(u.Symbol)
	}

	type link struct{ score, i, j int }
	var links []link
	for i := range units {
		for j := i + 1; j < len(units); j++ {
			score := 0
			if mentions(diff[i], name[j]) {
				score += p.ref
			}
			if mentions(diff[j], name[i]) {
				score += p.ref
			}
			if testFor(name[i], name[j]) || testFor(name[j], name[i]) {
				score += p.test
			}
			if units[i].File == units[j].File && len(diff[i])+len(diff[j]) < p.smallPair {
				score += p.sameFile
			}
			if score > 0 {
				links = append(links, link{score, i, j})
			}
		}
	}
	// Strongest link first; ties keep PR order so the result is stable.
	sort.SliceStable(links, func(a, b int) bool {
		if links[a].score != links[b].score {
			return links[a].score > links[b].score
		}
		if links[a].i != links[b].i {
			return links[a].i < links[b].i
		}
		return links[a].j < links[b].j
	})

	owner := make([]int, len(units))
	members := make([][]int, len(units))
	chars := make([]int, len(units))
	for i := range units {
		owner[i], members[i], chars[i] = i, []int{i}, len(diff[i])
	}
	for _, l := range links {
		a, b := owner[l.i], owner[l.j]
		if a == b || chars[a]+chars[b] > p.maxChars {
			continue
		}
		if p.maxMembers > 0 && len(members[a])+len(members[b]) > p.maxMembers {
			continue
		}
		members[a] = append(members[a], members[b]...)
		chars[a] += chars[b]
		for _, m := range members[b] {
			owner[m] = a
		}
		members[b] = nil
	}

	var out []*ReviewGroup
	for i := range units {
		if owner[i] != i {
			continue
		}
		idx := members[i]
		sort.Ints(idx)
		g := &ReviewGroup{Members: make([]*Unit, len(idx))}
		for k, m := range idx {
			g.Members[k] = units[m]
		}
		out = append(out, g)
	}
	return out
}

func soloGroups(units []*Unit) []*ReviewGroup {
	out := make([]*ReviewGroup, len(units))
	for i, u := range units {
		out[i] = &ReviewGroup{Members: []*Unit{u}}
	}
	return out
}

// testFor reports whether test is a changed test named after production
// symbol sym, e.g. TestRetryCaps for retryCaps. Substring rather than
// equality, because test names run the symbol together with the case they
// cover; case-insensitive, because the convention capitalizes the symbol's
// first letter after the Test prefix.
func testFor(test, sym string) bool {
	if len(sym) <= 4 || !strings.HasPrefix(test, "Test") && !strings.HasPrefix(test, "Benchmark") {
		return false
	}
	return strings.Contains(strings.ToLower(test), strings.ToLower(sym))
}

// setGroupContext gives each multi-member group the rest of the PR, using
// the same ranking as single units: a unit that any member ranks highly is
// shown early, and the budget is spent once for the whole group instead of
// once per member.
func setGroupContext(groups []*ReviewGroup, units []*Unit, base ContentFunc, budget int) {
	if len(groups) == 0 {
		return
	}
	baseDecls := baseDeclNames(units, base)
	lines := map[*Unit]changed{}
	for _, u := range units {
		lines[u] = changedText(u)
	}
	for _, g := range groups {
		if len(g.Members) > 1 {
			g.Context = buildContext(g.Members, units, baseDecls, lines, budget)
		}
	}
}
