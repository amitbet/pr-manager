package triage

import (
	"context"
	"sort"
	"sync"

	"github.com/amitbet/pr-manager/internal/activity"
	"github.com/amitbet/pr-manager/llm"
	"golang.org/x/sync/errgroup"
)

type Pipeline struct {
	Presorter *Presorter
	// Classifier places units no review call sees: every unit when there
	// is no Summarizer, else only those ReviewFilter leaves out. With a
	// Summarizer the review call places the unit it reviews.
	Classifier Classifier // nil: those units are "human"
	// Decisions, if set, keeps classifier answers between runs: a unit
	// whose file, declaration and diff were classified before under the
	// same ClassifyKey is not sent to the model again.
	Decisions DecisionStore
	// ClassifyKey names what else a decision depends on (the classifier,
	// its model and effort, the thresholds); it is part of the store key.
	ClassifyKey string
	Summarizer  *Summarizer // nil: no analysis, only the classifier's decisions
	CodeMap     *CodeMap    // nil: no impact scores, no file history and no tier moves
	// Lint runs the repository's static analysis over the lines the PR
	// adds, before anything is scored (nil or disabled: no findings).
	Lint        *Linter
	Concurrency int
	// ReviewConcurrency limits the analyze stage; 0 uses Concurrency.
	// Review calls are slower (bigger model, repo tools), so they get
	// their own limit.
	ReviewConcurrency int
	// ReviewFilter, when set, limits the analyze stage to selected units;
	// the Classifier places the rest. Scoring still covers the full diff.
	ReviewFilter func(*Unit) bool
	// CarryFrom, when set, is asked once the units are built which of
	// them may keep the review an earlier run of the same change earned
	// (see PlanCarryOver). Everything else still runs on the whole diff:
	// only the review calls are saved.
	CarryFrom func(fresh []*Unit) *ReviewCarry
	// Progress, if set, is called as units finish in each LLM stage.
	Progress func(stage string, done, total int)
	// Warn, if set, gets problems that only degrade the run.
	Warn func(msg string)
}

type Report struct {
	Base  string  `json:"base"`
	Head  string  `json:"head"`
	Units []*Unit `json:"units"`
}

// Scores returns the highest code-map impact (nil without a map), the
// highest likelihood and the highest review attention. Units a rule skipped
// (generated, formatting) do not count: their impact shows up where the
// source changed.
func (r *Report) Scores() (*Impact, *Likelihood, int) {
	var d *Impact
	var l *Likelihood
	att := 0
	for _, u := range r.Units {
		skipped := u.Decision.Source == "rule" && u.Decision.Bucket == BucketNone
		if skipped {
			continue
		}
		if u.Impact.Known() && (d == nil || u.Impact.Score > d.Score) {
			d = u.Impact
		}
		if u.Likelihood != nil && (l == nil || u.Likelihood.Score > l.Score) {
			l = u.Likelihood
		}
		att = max(att, u.Attention)
	}
	return d, l, att
}

func (r *Report) Counts() map[Bucket]int {
	c := map[Bucket]int{}
	for _, u := range r.Units {
		c[u.Decision.Bucket]++
	}
	return c
}

// Run triages src and returns units sorted human → skim → none.
func (p *Pipeline) Run(ctx context.Context, src *Source) []*Unit {
	units := BuildUnits(src.Files, src.Content, p.Presorter.Policy.MaxUnitChars)
	rest := p.Presorter.Presort(units, src)
	setRelated(units)

	sum := p.summarizer()

	// Asked after the presort, because a rule-skipped unit is not shown to
	// a reviewer and so cannot cost another unit its review. Pointless
	// without a reviewer: there would be no review stage to skip.
	var carry *ReviewCarry
	if p.CarryFrom != nil && sum != nil {
		carry = p.CarryFrom(units)
	}

	// With a reviewer, the review call places the units it reads (see
	// Analyze), and a carried unit keeps the bucket its review gave. The
	// classifier only places what no review call sees.
	toClassify := rest
	analyzed := map[*Unit]bool{}
	if sum != nil {
		toClassify = nil
		for _, u := range rest {
			switch old := carry.reuse(u); {
			case old != nil:
				u.Decision = CarriedDecision(old)
			case len(u.Hunks) == 0 || (p.ReviewFilter != nil && !p.ReviewFilter(u)):
				toClassify = append(toClassify, u)
			default:
				analyzed[u] = true
				u.Decision = Decision{Bucket: BucketHuman, Source: "pending", Reason: "not reviewed yet"}
			}
		}
	}

	// Classification only reads the units' diffs, so it runs while the
	// repository is checked out, linted and mapped. Whichever finishes
	// last owns the progress line: lint has no count to show, so it is
	// only reported once classify is done.
	var stageMu sync.Mutex
	classifyDone, linting := len(toClassify) == 0, false
	classified := make(chan struct{})
	go func() {
		defer close(classified)
		if len(toClassify) == 0 {
			return
		}
		p.classify(ctx, toClassify)
		stageMu.Lock()
		classifyDone = true
		if linting && p.Progress != nil {
			p.Progress("lint", 0, 0)
		}
		stageMu.Unlock()
	}()

	// The repository at the PR head is checked out once and shared: the
	// linters read it, and a reviewer whose provider supports tools reads
	// it too. It is removed when the run ends.
	wantTools := sum != nil && sum.Tools && llm.SupportsWorkspace(sum.LLM)
	var ws *llm.Workspace
	if p.Lint.on() || wantTools {
		w, cleanup, err := reviewWorkspace(src, p.Warn)
		defer cleanup()
		if err != nil && p.Warn != nil {
			p.Warn("repository at the PR head unavailable: " + err.Error())
		}
		ws = w
	}
	if p.Lint.on() {
		if p.Lint.Warn == nil {
			p.Lint.Warn = p.Warn
		}
		dir := ""
		if ws != nil {
			dir = ws.Dir
		}
		stageMu.Lock()
		linting = true
		if classifyDone && p.Progress != nil {
			p.Progress("lint", 0, 0)
		}
		stageMu.Unlock()
		p.Lint.Run(ctx, dir, src, units)
		stageMu.Lock()
		linting = false
		stageMu.Unlock()
	}

	if p.CodeMap != nil {
		files := map[string]FileDiff{}
		for _, f := range src.Files {
			files[f.Path] = f
		}
		for _, u := range units {
			u.Impact = p.CodeMap.Assess(u, files[u.File], src.BaseContent)
		}
	}
	// Likelihood reads the decisions, so it waits for them.
	<-classified
	lc := newLikelihoodCtx(src, p.CodeMap)
	for _, u := range units {
		u.Likelihood = lc.assess(u, fileOf(src, u.File))
	}
	tiers := p.Presorter.Policy.Tiers
	maxChars := p.Presorter.Policy.MaxUnitChars

	// Every unit gets a score, so every unit explains its bucket. Units
	// the review places are scored once it has.
	for _, u := range units {
		if !analyzed[u] {
			tiers.prior(u, maxChars)
		}
	}

	if sum != nil {
		setReviewContext(units, src.BaseContent, p.Presorter.Policy.ReviewContextChars)
		if wantTools {
			// Without a workspace the review still runs, just without tools.
			sum.workspace = ws
		}
		var toAnalyze, carried []*Unit
		prev := map[*Unit]Bucket{}
		for _, u := range units {
			// A unit whose diff and surroundings did not move keeps the
			// answer an earlier run already paid for.
			if old := carry.reuse(u); old != nil {
				prev[u] = u.Decision.Bucket
				applyCarried(u, old, carry.From)
				carried = append(carried, u)
				continue
			}
			// Rule-placed units the reviewer would see are reviewed under
			// the rule's bucket. Fixtures are only context for the tests
			// that read them.
			if analyzed[u] || (u.Decision.Source == "rule" && reviewable(u) && u.Decision.ChangeKind != "fixture" && (p.ReviewFilter == nil || p.ReviewFilter(u))) {
				toAnalyze = append(toAnalyze, u)
				prev[u] = u.Decision.Bucket
			}
		}
		limit := p.ReviewConcurrency
		if limit <= 0 {
			limit = p.Concurrency
		}
		groups := reviewGroups(toAnalyze, p.Presorter.Policy.Grouping)
		setGroupContext(groups, units, src.BaseContent, p.Presorter.Policy.ReviewContextChars)
		runStage(ctx, p, "analyze", limit, groups, (*ReviewGroup).ID, sum.AnalyzeGroup)
		for _, u := range append(toAnalyze, carried...) {
			if analyzed[u] {
				tiers.prior(u, maxChars)
				prev[u] = u.Decision.Bucket
			}
			tiers.afterReview(u, prev[u])
		}
	}

	SortByBucket(units)
	return units
}

// classify decides the units the presort left: from Decisions where an
// earlier run already did, the rest in batches when the classifier
// supports them.
func (p *Pipeline) classify(ctx context.Context, units []*Unit) {
	if p.Classifier == nil {
		for _, u := range units {
			u.Decision = Decision{Bucket: BucketHuman, Source: "none", Reason: "no classifier configured"}
		}
		return
	}
	var todo []*Unit
	keys := map[*Unit]string{}
	carrier, _ := p.Classifier.(Carrier)
	for _, u := range units {
		if carrier != nil {
			if d, ok := carrier.Carry(u); ok {
				u.Decision = d
				continue
			}
		}
		if p.Decisions != nil {
			k := decisionKey(p.ClassifyKey, u)
			if d, ok := p.Decisions.Load(k); ok {
				u.Decision = d
				continue
			}
			keys[u] = k
		}
		todo = append(todo, u)
	}
	save := func(u *Unit) {
		if k, ok := keys[u]; ok && !u.Decision.Failed {
			p.Decisions.Save(k, u.Decision)
		}
	}
	bc, ok := p.Classifier.(BatchClassifier)
	var batches [][]*Unit
	if ok && p.Presorter.Policy.ClassifyBatch.MaxUnits > 1 {
		batches = classifyBatches(todo, p.Presorter.Policy.ClassifyBatch)
	}
	if len(batches) == 0 || len(batches) == len(todo) {
		p.each(ctx, "classify", p.Concurrency, todo, func(ctx context.Context, u *Unit) {
			u.Decision = p.Classifier.Classify(ctx, u)
			save(u)
		})
		return
	}
	runStage(ctx, p, "classify", p.Concurrency, batches, batchID, func(ctx context.Context, b []*Unit) {
		for i, d := range bc.ClassifyBatch(ctx, b) {
			b[i].Decision = d
			save(b[i])
		}
	})
}

// summarizer is a copy of p.Summarizer the run can give a workspace to,
// so the pipeline never mutates the caller's.
func (p *Pipeline) summarizer() *Summarizer {
	if p.Summarizer == nil {
		return nil
	}
	s := *p.Summarizer
	return &s
}

// SortByBucket orders units human → skim → none, and by score inside
// a bucket (then risk), stable for ties.
func SortByBucket(units []*Unit) {
	total := func(u *Unit) int {
		if u.Score == nil {
			return 0
		}
		return u.Score.Total
	}
	sort.SliceStable(units, func(i, j int) bool {
		a, b := units[i].Decision.Bucket.rank(), units[j].Decision.Bucket.rank()
		if a != b {
			return a > b
		}
		if ti, tj := total(units[i]), total(units[j]); ti != tj {
			return ti > tj
		}
		return units[i].Risk() > units[j].Risk()
	})
}

func fileOf(src *Source, p string) FileDiff {
	for _, f := range src.Files {
		if f.Path == p {
			return f
		}
	}
	return FileDiff{Path: p}
}

func (p *Pipeline) each(ctx context.Context, stage string, limit int, units []*Unit, fn func(context.Context, *Unit)) {
	runStage(ctx, p, stage, limit, units, (*Unit).id, fn)
}

func (u *Unit) id() string { return u.ID }

// runStage runs one concurrent LLM stage over units or review groups,
// reporting progress in the items the stage actually works on.
func runStage[T any](ctx context.Context, p *Pipeline, stage string, limit int, items []T, id func(T) string, fn func(context.Context, T)) {
	var mu sync.Mutex
	done := 0
	report := func() {
		if p.Progress != nil {
			p.Progress(stage, done, len(items))
		}
	}
	report()
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(max(1, limit))
	for _, it := range items {
		g.Go(func() error {
			ctx, t := activity.Start(ctx, "llm", "%s %s", stage, id(it))
			fn(ctx, it)
			t.Finish(nil)
			mu.Lock()
			done++
			report()
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()
}

// reviewGroups splits the units to review into review calls. Grouping
// off means one call per unit.
func reviewGroups(units []*Unit, policy GroupPolicy) []*ReviewGroup {
	if !policy.Enabled {
		return soloGroups(units)
	}
	return BuildReviewGroups(units, policy)
}

// setRelated gives each unit the IDs of the other units, capped so large
// PRs don't blow up every prompt.
const maxRelated = 60

func setRelated(units []*Unit) {
	for _, u := range units {
		u.Related = u.Related[:0]
		for _, o := range units {
			if o != u && len(u.Related) < maxRelated {
				u.Related = append(u.Related, o.ID)
			}
		}
	}
}
