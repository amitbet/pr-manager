package main

import (
	"cmp"
	"context"
	"path/filepath"
	"strings"

	"github.com/amitbet/pr-manager/internal/activity"
	"github.com/amitbet/pr-manager/triage"
)

// Re-reviewing a PR after a push costs the same as reviewing it the first
// time, even though the author usually changed one or two files. A run can
// keep the review an earlier run of the same change earned for every unit
// whose diff, and whose surroundings, did not move (triage/incremental.go
// decides which those are). Everything else still runs on the whole diff:
// units, presort, lint, impact, likelihood, classification and scoring are
// cheap and are redone, so the buckets are as fresh as a full run's.
//
// Two things block it outright, because they change what every unit was
// judged against rather than any one unit's lines:
//
//   - different settings (prompt version, models, review effort, review
//     tools, lint, code map on or off): the settings hash is part of the
//     cache key, and only a run with the same one is looked at. A rebuilt
//     map is not different settings (see codeMapSettings): no review
//     reads the map, and what does is scored again;
//   - a different merge base: after a rebase or a base-branch merge, every
//     unit's context is a different piece of code.
//
// -force asks for a re-run, so it asks for a real one and carries nothing.

// splitKey breaks a result key into the change's identity, the content it
// was built from (a head commit for a PR, a diff hash for a local run) and
// the settings it was built with. Both key layouts end with those two.
func splitKey(key string) (prefix, content, settings string, ok bool) {
	parts := strings.Split(key, "__")
	if len(parts) < 3 {
		return "", "", "", false
	}
	n := len(parts)
	return strings.Join(parts[:n-2], "__"), parts[n-2], parts[n-1], true
}

// resultUnitsWithHunks is resultUnits with each unit's hunks put back.
// They are stored beside the unit rather than on it, so a loaded unit has
// none until this runs, and comparing diffs needs them.
func resultUnitsWithHunks(r *PRResult) []*triage.Unit {
	var units []*triage.Unit
	for _, f := range r.Files {
		for _, u := range f.Units {
			u.Unit.Hunks = u.Hunks
			units = append(units, u.Unit)
		}
	}
	return units
}

// previousRun finds the newest earlier run of the same change: the same
// identity and settings, a different head or diff, the same merge base,
// and in English, since another language is translated from a run like
// this one rather than reviewed.
func (t *triager) previousRun(key, base string) *PRResult {
	prefix, content, settings, ok := splitKey(key)
	if !ok {
		return nil
	}
	paths, _ := filepath.Glob(filepath.Join(t.results, prefix+"__*__"+settings+".json"))
	var best *PRResult
	for _, p := range paths {
		k := strings.TrimSuffix(filepath.Base(p), ".json")
		if _, c, _, ok := splitKey(k); !ok || c == content {
			continue
		}
		r, err := t.Load(k)
		switch {
		case err != nil, r.SummaryLang != "", r.PR == nil:
			continue
		case base != "" && r.PR.BaseOid != base:
			continue // rebased: every unit was judged against other code
		case best != nil && !r.CreatedAt.After(best.CreatedAt):
			continue
		}
		best = r
	}
	return best
}

// carryPlan is what an earlier run of the same change offers this one:
// the hook that decides which units keep their review, and that run's
// review comments, so the ones nobody has touched are not judged again.
type carryPlan struct {
	prev *PRResult
	hook func([]*triage.Unit) *triage.ReviewCarry
}

func (p *carryPlan) carryFrom() func([]*triage.Unit) *triage.ReviewCarry {
	if p == nil {
		return nil
	}
	return p.hook
}

// carryFrom builds the plan, or nil when there is nothing earlier to
// carry from. It logs what it decided to the job's activity log, so a
// reader can see which units were not reviewed again and why.
func (t *triager) carryFrom(ctx context.Context, key, base string, o options, force bool) *carryPlan {
	if !o.incremental || force {
		return nil
	}
	prev := t.previousRun(key, base)
	if prev == nil {
		return nil
	}
	from := prev.PR.HeadOid
	if from == "" {
		from = prev.Key
	}
	old := resultUnitsWithHunks(prev)
	return &carryPlan{prev: prev, hook: func(fresh []*triage.Unit) *triage.ReviewCarry {
		c := triage.PlanCarryOver(fresh, old, from)
		activity.Printf(ctx, "incremental: %s", c.Summary())
		return c
	}}
}

// threads returns the earlier run's review comments, for the thread
// refresh to match against: a comment whose text has not changed keeps
// its verdict instead of being sent to the model again.
//
// The duplicate link is dropped where this run reviewed the unit it
// points into again, because it is an index into that unit's issues and
// those were just rewritten. A unit that kept its review kept its issues
// with it, so the link still points at the same one.
func (p *carryPlan) threads(fresh []*triage.Unit) []triage.Thread {
	if p == nil {
		return nil
	}
	carried := map[string]bool{}
	for _, u := range fresh {
		if u.CarriedFrom != "" {
			carried[u.ID] = true
		}
	}
	var out []triage.Thread
	for _, f := range p.prev.Files {
		for _, u := range f.Units {
			for _, th := range u.Threads {
				if target := cmp.Or(th.DuplicateUnit, u.ID); !carried[target] {
					th.DuplicateOf, th.DuplicateUnit, th.Compared = nil, "", false
				}
				out = append(out, th)
			}
		}
	}
	return out
}
