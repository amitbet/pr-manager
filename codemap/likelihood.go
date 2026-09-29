package codemap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// Points turns one measured value into likelihood points:
// min(Max, max(0, value-From) * Per).
type Points struct {
	Per  float64 `json:"per" yaml:"per"`
	From float64 `json:"from,omitempty" yaml:"from"`
	Max  float64 `json:"max" yaml:"max"`
}

func (p Points) Of(v float64) float64 {
	return math.Min(p.Max, math.Max(0, v-p.From)*p.Per)
}

// LikelihoodWeights are the point rules behind every likelihood score. The
// place rules (history and complexity) score map records; pr-manager adds
// the change rules for a PR unit. Each rule is capped, and the total is
// capped at 100, so no single signal decides on its own.
type LikelihoodWeights struct {
	// Place: the code's history within the window, and its complexity.
	Fix     Points `json:"fix" yaml:"fix"`         // per recency-weighted fix commit
	Revert  Points `json:"revert" yaml:"revert"`   // per revert commit
	Churn   Points `json:"churn" yaml:"churn"`     // per recency-weighted commit
	Authors Points `json:"authors" yaml:"authors"` // per distinct author
	DirFix  Points `json:"dir_fix" yaml:"dir_fix"` // per recency-weighted fix elsewhere in the directory
	Cyclo   Points `json:"cyclo" yaml:"cyclo"`     // cyclomatic complexity of the worst function
	Nest    Points `json:"nest" yaml:"nest"`       // nesting depth

	// Change: what the PR does to the code.
	CycloDelta Points `json:"cyclo_delta" yaml:"cyclo_delta"` // complexity the change adds
	Size       Points `json:"size" yaml:"size"`               // changed lines
	NewToFile  Points `json:"new_to_file" yaml:"new_to_file"` // 1 when the author never changed the file
	Newcomer   Points `json:"newcomer" yaml:"newcomer"`       // 1 when the author has few commits in the repo
	CoChange   Points `json:"cochange" yaml:"cochange"`       // per usual partner file the PR leaves alone
	TestGap    Points `json:"test_gap" yaml:"test_gap"`       // 1 when code changed and no test next to it did
	Lint       Points `json:"lint" yaml:"lint"`               // per static-analysis finding on a line the change adds
	Diffusion  Points `json:"diffusion" yaml:"diffusion"`     // per directory the PR touches
	FixPR      Points `json:"fix_pr" yaml:"fix_pr"`           // 1 when the PR is itself a bug fix

	// NewcomerCommits: fewer commits than this in the repo makes a newcomer.
	NewcomerCommits int `json:"newcomer_commits" yaml:"newcomer_commits"`
}

// UnmarshalJSON fills in rules that a map written before they existed has
// no key for, while keeping a rule that was explicitly set to zero.
func (w *LikelihoodWeights) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	type plain LikelihoodWeights
	p := plain(*w)
	// Maps scored before the lint rule existed have no "lint" key; a
	// present key, even an all-zero one, overwrites this.
	p.Lint = DefaultLikelihoodWeights().Lint
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*w = LikelihoodWeights(p)
	return nil
}

func DefaultLikelihoodWeights() LikelihoodWeights {
	return LikelihoodWeights{
		Fix:     Points{Per: 12, Max: 30},
		Revert:  Points{Per: 15, Max: 20},
		Churn:   Points{Per: 1, From: 3, Max: 10},
		Authors: Points{Per: 2, From: 2, Max: 8},
		DirFix:  Points{Per: 2, Max: 6},
		Cyclo:   Points{Per: 1.25, From: 5, Max: 22},
		Nest:    Points{Per: 4, From: 3, Max: 10},

		CycloDelta: Points{Per: 2, Max: 14},
		Size:       Points{Per: 0.05, From: 20, Max: 6},
		NewToFile:  Points{Per: 8, Max: 8},
		Newcomer:   Points{Per: 6, Max: 6},
		CoChange:   Points{Per: 5, Max: 10},
		TestGap:    Points{Per: 6, Max: 6},
		Lint:       Points{Per: 4, Max: 12},
		Diffusion:  Points{Per: 1, From: 3, Max: 6},
		FixPR:      Points{Per: 4, Max: 4},

		NewcomerCommits: 10,
	}
}

// Factor is one reason a likelihood score has the points it has.
type Factor struct {
	ID     string `json:"id"`
	Points int    `json:"points"`
	Detail string `json:"detail"`
}

// Tally adds factors up to a 0-100 score. Factors worth less than half a
// point are dropped; the rest are sorted biggest first.
type Tally struct {
	sum     float64
	Factors []Factor
}

func (t *Tally) Add(id string, points float64, detail string, args ...any) {
	if points < 0.5 {
		return
	}
	t.sum += points
	t.Factors = append(t.Factors, Factor{ID: id, Points: int(math.Round(points)), Detail: fmt.Sprintf(detail, args...)})
}

func (t *Tally) Score() int {
	sort.SliceStable(t.Factors, func(i, j int) bool { return t.Factors[i].Points > t.Factors[j].Points })
	return min(100, int(math.Round(t.sum)))
}

// AddHistory scores a file's history.
func (w LikelihoodWeights) AddHistory(t *Tally, h *History, days int) {
	if h == nil {
		return
	}
	win := "in the history window"
	if days > 0 {
		win = fmt.Sprintf("in %d days", days)
	}
	t.Add("fixes", w.Fix.Of(h.RecentFixes), "%d fix commit%s %s (%.1f recency-weighted)", h.Fixes, plural(h.Fixes), win, h.RecentFixes)
	t.Add("reverts", w.Revert.Of(float64(h.Reverts)), "%d revert%s %s", h.Reverts, plural(h.Reverts), win)
	t.Add("churn", w.Churn.Of(h.Recent), "%d commit%s %s (%.1f recency-weighted)", h.Commits, plural(h.Commits), win, h.Recent)
	t.Add("authors", w.Authors.Of(float64(h.Authors)), "%d different authors %s", h.Authors, win)
}

// AddComplexity scores a function's (or file's worst function's) complexity.
func (w LikelihoodWeights) AddComplexity(t *Tally, cx *Complexity) {
	if cx == nil {
		return
	}
	t.Add("complexity", w.Cyclo.Of(float64(cx.Cyclo)), "cyclomatic complexity %d", cx.Cyclo)
	t.Add("nesting", w.Nest.Of(float64(cx.Nest)), "nested %d levels deep", cx.Nest)
}

// PlaceLikelihood is the likelihood of a map record before any change is
// known: its history plus its complexity.
func (w LikelihoodWeights) PlaceLikelihood(h *History, cx *Complexity, days int) (int, []Factor) {
	var t Tally
	w.AddHistory(&t, h, days)
	w.AddComplexity(&t, cx)
	return t.Score(), t.Factors
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Level maps a 0-100 score to the map's levels (the same cut-offs serve
// impact and likelihood).
func (m *Map) Level(v int) string { return m.level(v) }

// LikelihoodWeights returns the weights the map was scored with, or the
// defaults for a map built before likelihood existed.
func (m *Map) LikelihoodWeights() LikelihoodWeights {
	if m == nil || m.Meta.Likelihood == (LikelihoodWeights{}) {
		return DefaultLikelihoodWeights()
	}
	return m.Meta.Likelihood
}
