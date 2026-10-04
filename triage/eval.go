package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/amitbet/pr-manager/llm"
)

// An eval case is NAME.json in the fixtures dir. Either point it at a
// past PR in a local clone (same code path as a real run):
//
//	{"repo": "../some-repo", "base": "abc123", "head": "def456", "labels": {...}}
//
// or make it self-contained with a saved diff and the head versions of
// the changed files:
//
//	{"diff": "NAME.diff", "head_dir": "NAME.head", "labels": {...}}
//
// Relative paths resolve against the fixtures dir; base_dir (optional)
// holds the merge-base versions for a saved diff. Labels map a unit ID
// (file:symbol, as in the JSON report) or a file path to a bucket; a file
// label applies to every unit in that file. Unlabeled units aren't scored.
//
// findings scores the review itself, keyed the same way (a file key covers
// the issues of all its units):
//
//	"findings": {"x.go": {
//	  "must_find":     [{"match": "transient.*cach", "min_severity": "medium"}],
//	  "must_not_find": [{"match": "pointer", "min_severity": "medium"}]}}
//
// match is a case-insensitive regexp over an issue's title and detail (not
// the failure scenario, which names timeouts and errors in most issues). A must_find rule is found when some issue matches; its
// severity must also sit in [min_severity, max_severity] (default: any).
// A must_not_find rule is broken by a matching issue at min_severity or
// above (default low: any issue). "Check" items don't count as issues.
type EvalCase struct {
	Name     string                   `json:"-"`
	Repo     string                   `json:"repo,omitempty"`
	Base     string                   `json:"base,omitempty"`
	Head     string                   `json:"head,omitempty"`
	Diff     string                   `json:"diff,omitempty"`
	HeadDir  string                   `json:"head_dir,omitempty"`
	BaseDir  string                   `json:"base_dir,omitempty"`
	Labels   map[string]Bucket        `json:"labels"`
	Findings map[string]FindingLabels `json:"findings,omitempty"`
}

type FindingLabels struct {
	MustFind    []FindingRule `json:"must_find,omitempty"`
	MustNotFind []FindingRule `json:"must_not_find,omitempty"`
}

type FindingRule struct {
	Match       string `json:"match"`
	MinSeverity string `json:"min_severity,omitempty"`
	MaxSeverity string `json:"max_severity,omitempty"`
	Note        string `json:"note,omitempty"`
	re          *regexp.Regexp
}

func (r FindingRule) matches(is Issue) bool {
	return r.re.MatchString(is.Title + "\n" + is.Detail)
}

// severityIn reports whether sev is within [min, max]; empty bounds are open.
func severityIn(sev, min, max string) bool {
	w := severityWeight[sev]
	return (min == "" || w >= severityWeight[min]) && (max == "" || w <= severityWeight[max])
}

func LoadEvalCases(dir string) ([]EvalCase, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
	var out []EvalCase
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var c EvalCase
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		c.Name = strings.TrimSuffix(filepath.Base(p), ".json")
		c.Repo, c.Diff, c.HeadDir, c.BaseDir = abs(c.Repo), abs(c.Diff), abs(c.HeadDir), abs(c.BaseDir)
		if (c.Repo == "") == (c.Diff == "") {
			return nil, fmt.Errorf("%s: set exactly one of repo or diff", p)
		}
		for key, fl := range c.Findings {
			for _, rules := range [][]FindingRule{fl.MustFind, fl.MustNotFind} {
				for i := range rules {
					r := &rules[i]
					if r.re, err = regexp.Compile("(?is)" + r.Match); err != nil {
						return nil, fmt.Errorf("%s: findings %s: %w", p, key, err)
					}
					for _, sev := range []string{r.MinSeverity, r.MaxSeverity} {
						if _, ok := severityWeight[sev]; sev != "" && !ok {
							return nil, fmt.Errorf("%s: findings %s: unknown severity %q", p, key, sev)
						}
					}
				}
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func (c EvalCase) Source() (*Source, error) {
	if c.Repo != "" {
		return FromGit(c.Repo, c.Base, c.Head)
	}
	raw, err := os.ReadFile(c.Diff)
	if err != nil {
		return nil, err
	}
	return FromDiff(string(raw), c.HeadDir, c.BaseDir)
}

type EvalResult struct {
	Scored int
	Exact  int
	// Misses are human-labeled units that landed in "none": the metric
	// that matters. Under are human → skim. Over are any escalation.
	Misses, Under, Over []string
	// Faithfulness is OpenJev's p(summary is accurate) per summarized unit.
	Faithfulness map[string]float64
	// Confusion[label][predicted]
	Confusion map[Bucket]map[Bucket]int

	// Findings: must_find rules found, found at the wrong severity, and
	// missed; must_not_find rules broken. Issues counts every issue on the
	// labeled keys: TruePos matched a must_find rule, FalsePos broke a
	// must_not_find rule, the rest are unlabeled.
	MustFind, Found               int
	WrongSeverity, Missed, Broken []string
	Issues, TruePos, FalsePos     int
	// Listed is every issue on the labeled keys with its verdict.
	Listed []string

	// Budgets is how each review budget would have placed the same units.
	Budgets []BudgetResult
}

// BudgetResult re-places a run's units under one review budget. Under
// counts human-labeled units outside human; Defects are
// units with an issue matching a must_find rule, Hidden those outside human.
type BudgetResult struct {
	Name                   string
	Counts                 map[Bucket]int
	Under, Defects, Hidden int
}

// budgetResults places each case's units under every budget without
// touching their buckets.
func budgetResults(tp TierPolicy, runs []evalRun) []BudgetResult {
	var out []BudgetResult
	for _, nb := range tp.OrderedBudgets() {
		br := BudgetResult{Name: nb.Name, Counts: map[Bucket]int{}}
		for _, run := range runs {
			defect := defectUnits(run.c, run.units)
			for _, u := range run.units {
				got := u.Decision.Bucket
				if u.Score != nil {
					got, _, _ = u.Score.Place(nb.Budget, nb.Name, u.Attention)
				}
				br.Counts[got]++
				if want, ok := labelOf(run.c, u); ok && want == BucketHuman && got != BucketHuman {
					br.Under++
				}
				if defect[u] {
					br.Defects++
					if got != BucketHuman {
						br.Hidden++
					}
				}
			}
		}
		out = append(out, br)
	}
	return out
}

type evalRun struct {
	c     EvalCase
	units []*Unit
}

func labelOf(c EvalCase, u *Unit) (Bucket, bool) {
	want, ok := c.Labels[u.ID]
	if !ok {
		want, ok = c.Labels[u.File]
	}
	return want, ok
}

// defectUnits are the units whose issues match a must_find rule.
func defectUnits(c EvalCase, units []*Unit) map[*Unit]bool {
	out := map[*Unit]bool{}
	for key, fl := range c.Findings {
		for _, u := range units {
			if u.ID != key && u.File != key {
				continue
			}
			for _, is := range u.Issues {
				for _, rule := range fl.MustFind {
					if rule.matches(is) {
						out[u] = true
					}
				}
			}
		}
	}
	return out
}

// scoreFindings checks one case's issues against its finding labels.
func (r *EvalResult) scoreFindings(c EvalCase, units []*Unit) {
	for key, fl := range c.Findings {
		var issues []Issue
		for _, u := range units {
			if u.ID == key || u.File == key {
				issues = append(issues, u.Issues...)
			}
		}
		id := c.Name + "/" + key
		tp, fp := map[int]bool{}, map[int]bool{}
		for _, rule := range fl.MustFind {
			r.MustFind++
			found, inRange := false, false
			var got []string
			for i, is := range issues {
				if rule.matches(is) {
					found, tp[i] = true, true
					got = append(got, is.Severity)
					inRange = inRange || severityIn(is.Severity, rule.MinSeverity, rule.MaxSeverity)
				}
			}
			switch {
			case !found:
				r.Missed = append(r.Missed, fmt.Sprintf("%s: /%s/%s", id, rule.Match, noteText(rule)))
			case !inRange:
				r.WrongSeverity = append(r.WrongSeverity, fmt.Sprintf("%s: /%s/ at %s, want %s%s", id, rule.Match, strings.Join(got, ","), sevRange(rule), noteText(rule)))
			default:
				r.Found++
			}
		}
		for _, rule := range fl.MustNotFind {
			for i, is := range issues {
				if rule.matches(is) && severityIn(is.Severity, orDefault(rule.MinSeverity, "low"), "") {
					fp[i] = true
					r.Broken = append(r.Broken, fmt.Sprintf("%s: %s %q matches /%s/%s", id, is.Severity, is.Title, rule.Match, noteText(rule)))
				}
			}
		}
		r.Issues += len(issues)
		for i, is := range issues {
			verdict := "unlabeled"
			switch {
			case tp[i]:
				verdict, r.TruePos = "expected", r.TruePos+1
			case fp[i]:
				verdict, r.FalsePos = "FALSE POSITIVE", r.FalsePos+1
			}
			sev := is.Severity
			if is.Claimed != "" {
				sev += " (claimed " + is.Claimed + ")"
			}
			r.Listed = append(r.Listed, fmt.Sprintf("%s [%s] %s: %s", id, verdict, sev, is.Title))
		}
	}
}

func sevRange(r FindingRule) string {
	return orDefault(r.MinSeverity, "low") + ".." + orDefault(r.MaxSeverity, "critical")
}

func noteText(r FindingRule) string {
	if r.Note == "" {
		return ""
	}
	return " (" + r.Note + ")"
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Judge scores summaries with OpenJev. nil disables judging.
type Judge struct{ Jev *llm.OpenJev }

func (j *Judge) Faithful(ctx context.Context, u *Unit) (float64, error) {
	state := fmt.Sprintf("Code change:\n```diff\n%s\n```\n\nReviewer summary:\n%s", u.Diff(), u.Summary)
	resp, err := j.Jev.Decide(ctx, state, map[string]llm.JevQuestion{
		"faithful": {Type: "noul", Instructions: "Does the reviewer summary accurately describe the code change without omitting a behavior change?"},
	})
	if err != nil {
		return 0, err
	}
	a := resp.Answers["faithful"]
	if a.Noul == nil {
		return 0, fmt.Errorf("openjev: no noul answer")
	}
	return *a.Noul, nil
}

func RunEval(ctx context.Context, p *Pipeline, cases []EvalCase, judge *Judge) (*EvalResult, error) {
	res := &EvalResult{Faithfulness: map[string]float64{}, Confusion: map[Bucket]map[Bucket]int{}}
	var runs []evalRun
	defer func() { res.Budgets = budgetResults(p.Presorter.Policy.Tiers, runs) }()
	for _, c := range cases {
		src, err := c.Source()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Name, err)
		}
		units := p.Run(ctx, src)
		runs = append(runs, evalRun{c, units})
		res.scoreFindings(c, units)
		for _, u := range units {
			id := c.Name + "/" + u.ID
			if judge != nil && u.Summary != "" {
				if f, err := judge.Faithful(ctx, u); err == nil {
					res.Faithfulness[id] = f
				}
			}
			want, ok := labelOf(c, u)
			if !ok {
				continue
			}
			got := u.Decision.Bucket
			res.Scored++
			if res.Confusion[want] == nil {
				res.Confusion[want] = map[Bucket]int{}
			}
			res.Confusion[want][got]++
			switch {
			case got == want:
				res.Exact++
			case want == BucketHuman && got == BucketNone:
				res.Misses = append(res.Misses, id)
			case want == BucketHuman && got == BucketSkim:
				res.Under = append(res.Under, id)
			case got.rank() > want.rank():
				res.Over = append(res.Over, id)
			default: // skim → none
				res.Under = append(res.Under, id)
			}
		}
	}
	return res, nil
}

func (r *EvalResult) Print(w io.Writer) {
	pct := func(n int) float64 {
		if r.Scored == 0 {
			return 0
		}
		return 100 * float64(n) / float64(r.Scored)
	}
	fmt.Fprintf(w, "scored units: %d\nexact:        %d (%.1f%%)\n", r.Scored, r.Exact, pct(r.Exact))
	fmt.Fprintf(w, "MISSES human→none:   %d (%.1f%%)\n", len(r.Misses), pct(len(r.Misses)))
	fmt.Fprintf(w, "under-reviewed:      %d (%.1f%%)\n", len(r.Under), pct(len(r.Under)))
	fmt.Fprintf(w, "over-escalated:      %d (%.1f%%)\n", len(r.Over), pct(len(r.Over)))
	fmt.Fprintf(w, "\nconfusion (rows=label, cols=predicted)\n%-8s %6s %8s %5s %6s\n", "", "human", "skim", "aux", "none")
	for _, want := range []Bucket{BucketHuman, BucketSkim, BucketNone} {
		row := r.Confusion[want]
		fmt.Fprintf(w, "%-8s %6d %8d %5d %6d\n", want, row[BucketHuman], row[BucketSkim], row[BucketAux], row[BucketNone])
	}
	for _, id := range r.Misses {
		fmt.Fprintf(w, "  miss: %s\n", id)
	}
	if len(r.Budgets) > 0 {
		fmt.Fprintf(w, "\nreview budgets (all units; labeled human outside human; must_find defects outside human)\n%-10s %6s %8s %5s %5s %6s %8s\n", "", "human", "skim", "aux", "none", "under", "defects")
		for _, b := range r.Budgets {
			fmt.Fprintf(w, "%-10s %6d %8d %5d %5d %6d %5d/%d\n", b.Name, b.Counts[BucketHuman], b.Counts[BucketSkim], b.Counts[BucketAux], b.Counts[BucketNone], b.Under, b.Hidden, b.Defects)
		}
	}
	if r.MustFind > 0 || r.Issues > 0 {
		fmt.Fprintf(w, "\nfindings: %d/%d must_find found at the right severity, %d at the wrong one, %d missed\n",
			r.Found, r.MustFind, len(r.WrongSeverity), len(r.Missed))
		prec := "n/a"
		if r.TruePos+r.FalsePos > 0 {
			prec = fmt.Sprintf("%.0f%%", 100*float64(r.TruePos)/float64(r.TruePos+r.FalsePos))
		}
		fmt.Fprintf(w, "issues on labeled units: %d (%d expected, %d false positive, %d unlabeled), precision %s\n",
			r.Issues, r.TruePos, r.FalsePos, r.Issues-r.TruePos-r.FalsePos, prec)
		for _, s := range r.Listed {
			fmt.Fprintf(w, "  issue: %s\n", s)
		}
		for _, s := range r.Broken {
			fmt.Fprintf(w, "  false positive: %s\n", s)
		}
		for _, s := range r.WrongSeverity {
			fmt.Fprintf(w, "  severity: %s\n", s)
		}
		for _, s := range r.Missed {
			fmt.Fprintf(w, "  missed: %s\n", s)
		}
	}
	if len(r.Faithfulness) > 0 {
		ids := make([]string, 0, len(r.Faithfulness))
		sum := 0.0
		for id, f := range r.Faithfulness {
			ids = append(ids, id)
			sum += f
		}
		sort.Slice(ids, func(i, j int) bool { return r.Faithfulness[ids[i]] < r.Faithfulness[ids[j]] })
		fmt.Fprintf(w, "\nsummary faithfulness (openjev): mean %.2f over %d\n", sum/float64(len(ids)), len(ids))
		for _, id := range ids {
			if r.Faithfulness[id] < 0.5 {
				fmt.Fprintf(w, "  low: %.2f %s\n", r.Faithfulness[id], id)
			}
		}
	}
}
