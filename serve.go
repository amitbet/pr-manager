package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/codemap"
	"github.com/amitbet/pr-manager/internal/activity"
	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

//go:embed ui
var uiFS embed.FS

// PRResult is what the UI renders: the PR, and every file with its units
// and their hunks in file order.
type PRResult struct {
	Key        string         `json:"key"`
	PR         *triage.PRInfo `json:"pr"`
	Classifier string         `json:"classifier"`
	Summarizer string         `json:"summarizer"`
	// PromptVersion is the triage.PromptVersion the result was reviewed
	// with; empty for results from before it was saved.
	PromptVersion string `json:"prompt_version,omitempty"`
	// SummaryLang is set on results from before translation, reviewed in
	// that language. Newer results are English; see translation.
	SummaryLang string                `json:"summary_lang,omitempty"`
	CreatedAt   time.Time             `json:"created_at"`
	DurationMS  int64                 `json:"duration_ms"`
	Counts      map[triage.Bucket]int `json:"counts"`
	// Overview is nil for results from before overviews, and when it
	// could not be written; see overview.
	Overview *triage.Overview `json:"overview,omitempty"`
	// Sequence is the changed call flow, written in the background once
	// the result is saved, or when a result without one is opened; see
	// sequence.
	Sequence *triage.Sequence `json:"sequence,omitempty"`
	// Carried is what this run kept from an earlier run of the same PR
	// (nil when it reviewed everything itself); see incremental.go.
	Carried *triage.CarryStats `json:"carried,omitempty"`
	// Threads is when the PR's review threads were last loaded onto the
	// units and what was left out; nil for local changes and results
	// from before threads.
	Threads *triage.ThreadStats `json:"threads,omitempty"`
	// Impact, Likelihood and Attention are the highest unit scores; Impact
	// is nil for results triaged without a code map.
	Impact     *triage.Impact     `json:"impact,omitempty"`
	Likelihood *triage.Likelihood `json:"likelihood,omitempty"`
	Attention  int                `json:"attention"`
	CodeMap    string             `json:"codemap,omitempty"` // map build time
	// ReviewBudget placed the units; Budgets lets the UI re-place them.
	ReviewBudget     string               `json:"review_budget,omitempty"`
	Budgets          []triage.NamedBudget `json:"budgets,omitempty"`
	Files            []resultFile         `json:"files"`
	LocalFixDir      string               `json:"local_fix_dir,omitempty"`
	LocalFixBranch   string               `json:"local_fix_branch,omitempty"`
	LocalFixLocation string               `json:"local_fix_location,omitempty"`
	FixRounds        int                  `json:"fix_rounds,omitempty"`
	// FixFromRev is the commit or branch whose review issues were fixed
	// in the checkout's current code instead.
	FixFromRev string `json:"fix_from_rev,omitempty"`
	// FixWarning says how a local checkout had changed since its triage
	// when the fix started.
	FixWarning string `json:"fix_warning,omitempty"`
	// FixBase is the commit the fix checkout started from: its changes are
	// the diff from it. FixedIssues is what the checks found fixed, over
	// this fix and the ones it continued (see fixpush.go).
	FixBase     string       `json:"fix_base,omitempty"`
	FixedIssues []fixedIssue `json:"fixed_issues,omitempty"`
	// FixPushed is the checkout's HEAD when the fix was pushed to the PR,
	// and FixPushedAs the PR head that push made (another commit when
	// several fixes went up together).
	FixPushed   string `json:"fix_pushed,omitempty"`
	FixPushedAs string `json:"fix_pushed_as,omitempty"`
	// FixCommit is the commit a fix made on its PR's branch in the
	// repository's fix checkout (see fixcheckout.go).
	FixCommit string `json:"fix_commit,omitempty"`
	// Resolved is what an earlier review of the PR raised that this one,
	// of other code, no longer does (see resolved.go).
	Resolved []resolvedIssue `json:"resolved,omitempty"`
}

// tierPolicy is the budget table the result was triaged with (the
// defaults for results cached before budgets).
func (r *PRResult) tierPolicy() triage.TierPolicy {
	tp := triage.DefaultTierPolicy()
	if len(r.Budgets) > 0 {
		tp.Budgets = map[string]triage.Budget{}
		for _, b := range r.Budgets {
			tp.Budgets[b.Name] = b.Budget
		}
		tp.ReviewBudget = r.ReviewBudget
	}
	return tp
}

type resultFile struct {
	triage.FileDiff
	Units []resultUnit `json:"units"`
}

type resultUnit struct {
	*triage.Unit
	Hunks []triage.Hunk `json:"hunks"`
}

type prSummary struct {
	Key        string                `json:"key"`
	PR         triage.PRRef          `json:"pr"`
	Title      string                `json:"title"`
	State      string                `json:"state"`
	Classifier string                `json:"classifier"`
	CreatedAt  time.Time             `json:"created_at"`
	Counts     map[triage.Bucket]int `json:"counts"`
	Impact     *triage.Impact        `json:"impact,omitempty"`
	Likelihood *triage.Likelihood    `json:"likelihood,omitempty"`
	Attention  int                   `json:"attention"`
	LocalPath  string                `json:"local_path,omitempty"`
	Rev        string                `json:"rev,omitempty"`
	HeadRef    string                `json:"head_ref,omitempty"`
}

// triager runs PR triage jobs and caches results as JSON files.
type triager struct {
	opts    options
	fetcher *triage.PRFetcher
	results string

	// dismissed holds the issues people rejected, applied to every result
	// as it loads (nil in tests that build a triager directly).
	dismissed *dismissals
	// reviews keeps the pending review comments, for the chat agent (nil
	// in tests that build a triager directly).
	reviews *reviews

	mu    sync.Mutex
	jobs  map[string]*job
	fixMu sync.Mutex
	// checkouts are the repositories' fix checkouts, where PR fixes run
	// (see fixcheckout.go); fixMu serializes the fixes of local reviews,
	// and of PRs fixed before them.
	checkouts *fixCheckouts
	trMu      sync.Mutex // guards trLocks
	// trLocks has a lock per translation file, so one PR is translated once.
	trLocks map[string]*sync.Mutex

	// root is the parent of every job's context; shutdown cancels it, which
	// kills the jobs' LLM CLI process groups, and waits for running.
	root       context.Context
	cancelRoot context.CancelFunc
	running    sync.WaitGroup
	closing    bool // guarded by mu
	stopOnce   sync.Once
}

type job struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"` // triage | fix | index
	URL     string    `json:"url"`
	Started time.Time `json:"started"`
	Status  string    `json:"status"` // running | done | error
	Stage   string    `json:"stage"`
	Done    int       `json:"done"`
	Total   int       `json:"total"`
	Error   string    `json:"error,omitempty"`
	Warning string    `json:"warning,omitempty"`
	Key     string    `json:"key,omitempty"`
	Result  any       `json:"result,omitempty"` // index jobs
	// Cached is when the result a triage job reused was made, if it
	// reused one instead of running.
	Cached *time.Time `json:"cached,omitempty"`
	// CachedBy is the model that placed the reused result's units, and
	// RunsWith the model a re-run would use, when they differ.
	CachedBy string `json:"cached_by,omitempty"`
	RunsWith string `json:"runs_with,omitempty"`

	log  *activity.Log
	done func() // untracks the job; nil once shutdown began
}

func newTriager(o options) (*triager, error) {
	t := &triager{
		opts:    o,
		fetcher: &triage.PRFetcher{Dir: filepath.Join(o.cache, "repos")},
		results: filepath.Join(o.cache, "results"),
		jobs:    map[string]*job{},
		trLocks: map[string]*sync.Mutex{},
	}
	t.checkouts = newFixCheckouts(o.cache, t.fetcher)
	t.root, t.cancelRoot = context.WithCancel(context.Background())
	d, err := newDismissals(o)
	if err != nil {
		return nil, err
	}
	t.dismissed = d
	return t, os.MkdirAll(t.results, 0o755)
}

// jobOptions overrides the server's provider flags with non-empty values.
type jobOptions struct {
	Classifier    string  `json:"classifier"`
	ClassifyModel string  `json:"classify_model"`
	Summarizer    string  `json:"summarizer"`
	SummaryModel  string  `json:"summary_model"`
	ReviewTools   *bool   `json:"review_tools"`
	ClassifyBatch *bool   `json:"classify_batch"`
	Lint          *string `json:"lint"`
	Incremental   *bool   `json:"incremental"`
	SummaryLang   *string `json:"summary_lang"`
	// Translator and TranslateModel work like Classifier and ClassifyModel.
	Translator      string  `json:"translator"`
	TranslateModel  string  `json:"translate_model"`
	TranslateEffort *string `json:"translate_effort"`
	// Chat and ChatModel pick the chat agent, the same way.
	Chat      string  `json:"chat"`
	ChatModel string  `json:"chat_model"`
	CodeRoot  *string `json:"code_root"`
	Org       *string `json:"org"`
	Force     bool    `json:"force"`
	// Rereview reviews these units again and keeps every other unit's
	// review from the result RereviewOf (see chatactions.go).
	Rereview   []string `json:"rereview,omitempty"`
	RereviewOf string   `json:"rereview_of,omitempty"`
}

func (t *triager) options(jo jobOptions) options {
	o := t.opts
	// A model picked while the provider is left on "default" applies to
	// the default provider.
	if jo.Classifier != "" || jo.ClassifyModel != "" {
		if jo.Classifier != "" {
			o.classifier = llm.ProviderID(jo.Classifier)
		}
		o.classifyModel = jo.ClassifyModel
	}
	if jo.Summarizer != "" || jo.SummaryModel != "" {
		if jo.Summarizer != "" {
			o.summarizer = llm.ProviderID(jo.Summarizer)
		}
		o.summaryModel = jo.SummaryModel
	}
	if jo.ReviewTools != nil {
		o.reviewTools = *jo.ReviewTools
	}
	if jo.ClassifyBatch != nil {
		switch {
		case !*jo.ClassifyBatch:
			o.classifyBatch = 1
		case o.classifyBatch == 1:
			// Turned on over a server started with -classify-batch 1.
			o.classifyBatch = triage.DefaultBatchUnits
		}
	}
	if jo.Lint != nil {
		o.lint, o.lintSet = strings.TrimSpace(*jo.Lint), true
	}
	if jo.Incremental != nil {
		o.incremental = *jo.Incremental
	}
	if jo.Translator != "" || jo.TranslateModel != "" {
		if jo.Translator != "" {
			o.translator = llm.ProviderID(jo.Translator)
		}
		o.translateModel = jo.TranslateModel
	}
	if jo.Chat != "" || jo.ChatModel != "" {
		if jo.Chat != "" {
			o.chat = llm.ProviderID(jo.Chat)
		}
		o.chatModel = jo.ChatModel
	}
	if jo.TranslateEffort != nil {
		o.translateEffort = strings.TrimSpace(*jo.TranslateEffort)
	}
	if jo.SummaryLang != nil {
		o.summaryLang = strings.TrimSpace(*jo.SummaryLang)
	}
	if strings.EqualFold(o.summaryLang, "english") {
		o.summaryLang = ""
	}
	if jo.CodeRoot != nil {
		o.codeRoot = strings.TrimSpace(*jo.CodeRoot)
	}
	if jo.Org != nil {
		o.org = strings.TrimSpace(*jo.Org)
	}
	return resolveProviders(o)
}

// settingsHash is the part of a cache key that depends only on how a run
// is configured: the prompt version, the providers and models, the review
// settings and the code-map build. It is kept apart from the change being
// triaged because two runs of the same PR can only reuse each other's
// reviews when this matches (see incremental.go).
func settingsHash(o options) string {
	lint := ""
	if o.lintSet {
		lint = o.lint
	}
	// With a summarizer the classifier is not used (see buildPipeline), so
	// its settings must not split the cache.
	classifier, classifyModel, fallback, fallbackModel, classifyEffort := o.classifier, o.classifyModel, o.fallback, o.fallbackModel, o.classifyEffort
	if o.summarizer != "off" {
		classifier, classifyModel, fallback, fallbackModel, classifyEffort = "", "", "", "", ""
	}
	parts := []string{triage.PromptVersion, classifier, classifyModel, fallback, fallbackModel, o.summarizer, o.summaryModel, classifyEffort, o.reviewEffort, fmt.Sprint(o.reviewTools), lint, codeMapSettings(loadCodeMap(o.codemapDir))}
	if o.classifyBatch > 0 && o.summarizer == "off" {
		// Appended only when set, so existing keys stay valid.
		parts = append(parts, fmt.Sprintf("batch=%d", o.classifyBatch))
	}
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return fmt.Sprintf("%x", h[:4])
}

func cacheKey(ref triage.PRRef, head string, o options) string {
	return fmt.Sprintf("%s__%.10s__%s", ref.FileKey(), head, settingsHash(o))
}

// latestCached returns the newest cached result for this PR head from any
// provider or code-map version, so a PR that was already triaged is only
// re-run when asked (force), not because the model or map changed; the
// job says when the model differs (see noteModel). Results from another
// prompt version are not reused: a new version fixes what the reviews
// missed. Only English results count: other languages are translated from
// them. Nor are results diffed against another base branch than baseRef
// (see sameBase).
func (t *triager) latestCached(ref triage.PRRef, head, baseRef string) (*PRResult, error) {
	pattern := filepath.Join(t.results, fmt.Sprintf("%s__%.10s__*.json", ref.FileKey(), head))
	paths, _ := filepath.Glob(pattern)
	var best *PRResult
	for _, p := range paths {
		r, err := t.Load(strings.TrimSuffix(filepath.Base(p), ".json"))
		if err == nil && r.SummaryLang == "" && r.PromptVersion == triage.PromptVersion && sameBase(r, baseRef) && (best == nil || r.CreatedAt.After(best.CreatedAt)) {
			best = r
		}
	}
	if best == nil {
		return nil, os.ErrNotExist
	}
	return best, nil
}

// Run triages one PR, reusing a cached result unless force is set.
func (t *triager) Run(ctx context.Context, ref triage.PRRef, jo jobOptions, progress func(stage string, done, total int)) (*PRResult, error) {
	o := t.options(jo)
	progress("fetch", 0, 0)
	info, err := t.fetcher.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	key := cacheKey(ref, info.HeadOid, o)
	if !jo.rerun() {
		if r, err := t.Load(key); err == nil && sameBase(r, info.BaseRef) {
			return t.withThreads(ctx, t.withPRInfo(r, info), o, progress), nil
		}
		if r, err := t.latestCached(ref, info.HeadOid, info.BaseRef); err == nil {
			return t.withThreads(ctx, t.withPRInfo(r, info), o, progress), nil
		}
	}
	src, err := t.fetcher.Source(ctx, info)
	if err != nil {
		return nil, err
	}
	if err := ensureCodeMap(ctx, o, ref, progress); err != nil {
		return nil, err
	}
	key = cacheKey(ref, info.HeadOid, o) // the first build turns the map on
	policy, gitattrs, err := sourceConfig(src, false)
	if err != nil {
		return nil, err
	}
	pipe, err := buildPipeline(o, policy, gitattrs)
	if err != nil {
		return nil, err
	}
	pipe.Progress = progress
	if m := loadCodeMap(o.codemapDir); m != nil {
		pipe.CodeMap = &triage.CodeMap{Map: m, Repo: codeMapRepo(m, ref)}
	}
	carry := t.planCarry(ctx, key, info.BaseOid, o, jo)
	pipe.CarryFrom = carry.carryFrom()
	return t.runSource(ctx, key, info, src, pipe, o, carry)
}

// sameBase reports whether a saved result was diffed against the branch
// the PR targets now. The cache key covers the head, not the base, and a
// PR can be retargeted without a push; its diff and review are then
// another's. Resolve has the base branch's name but not its commit, so
// only a renamed base is caught here; a base that moved on keeps the
// saved diff until the head changes. Results saved without a base branch
// are kept.
func sameBase(r *PRResult, baseRef string) bool {
	return r.PR == nil || r.PR.BaseRef == "" || baseRef == "" || r.PR.BaseRef == baseRef
}

// withPRInfo puts what can change on a PR without a push (its state,
// title, description, URL, author and branch names) from info on a saved
// result, and saves it when any of it did, so a merged PR does not show as
// open. The diff's fields (base commit, commits, line counts) are left: the
// saved result is still that diff.
func (t *triager) withPRInfo(r *PRResult, info *triage.PRInfo) *PRResult {
	if r.PR == nil || r.PR.LocalPath != "" {
		return r
	}
	set := func(r *PRResult) {
		p := r.PR
		p.State, p.Title, p.Body, p.URL, p.Author = info.State, info.Title, info.Body, info.URL, info.Author
		p.BaseRef, p.HeadRef, p.MergedAt = info.BaseRef, info.HeadRef, info.MergedAt
	}
	p := r.PR
	if p.State == info.State && p.Title == info.Title && p.Body == info.Body && p.URL == info.URL &&
		p.Author == info.Author && p.BaseRef == info.BaseRef && p.HeadRef == info.HeadRef && p.MergedAt == info.MergedAt {
		return r
	}
	saved, err := t.updateResult(r.Key, func(r *PRResult) {
		if r.PR != nil {
			set(r)
		}
	})
	if err != nil {
		log.Printf("save PR details %s: %v", r.Key, err)
		set(r)
		return r
	}
	return saved
}

func (t *triager) runSource(ctx context.Context, key string, info *triage.PRInfo, src *triage.Source, pipe *triage.Pipeline, o options, carry *carryPlan) (*PRResult, error) {
	start := time.Now()
	units := pipe.Run(ctx, src)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The earlier run's verdicts, so the refresh only sends the comments
	// whose text changed to the model.
	if prior := carry.threads(units); len(prior) > 0 {
		triage.AssignThreads(units, prior)
	}
	// Before the overview, so confirmed comments are in it.
	threads := t.refreshThreads(ctx, info, units, o, pipe.Presorter.Policy.Tiers, pipe.Progress)
	// After the comments, so a comment that repeats an issue of another
	// unit is merged into it too; before the counts, which it changes.
	if pipe.Summarizer != nil {
		if pipe.Progress != nil {
			pipe.Progress("dedupe", 0, 0)
		}
		pipe.Summarizer.Dedupe(ctx, units, pipe.Presorter.Policy.Tiers)
	}

	r := &PRResult{
		Key: key, PR: info, CreatedAt: time.Now(), DurationMS: time.Since(start).Milliseconds(),
		PromptVersion: triage.PromptVersion,
		Counts:        (&triage.Report{Units: units}).Counts(), Threads: threads,
	}
	r.Classifier, r.Summarizer = runBy(o)
	r.Impact, r.Likelihood, r.Attention = (&triage.Report{Units: units}).Scores()
	r.Carried = triage.CountCarried(units)
	if pipe.CodeMap != nil {
		r.CodeMap = codeMapVersion(pipe.CodeMap.Map)
	}
	tiers := pipe.Presorter.Policy.Tiers
	_, r.ReviewBudget, _ = tiers.Budget("")
	r.Budgets = tiers.OrderedBudgets()
	if pipe.Summarizer != nil {
		if pipe.Progress != nil {
			pipe.Progress("overview", 0, 1)
		}
		// Without one, the result gets it when it is opened.
		ov, err := triage.WriteOverview(ctx, pipe.Summarizer.LLM, info, units)
		if err != nil {
			log.Printf("overview %s: %v", key, err)
		}
		r.Overview = ov
	}
	byFile := map[string][]resultUnit{}
	for _, u := range units {
		byFile[u.File] = append(byFile[u.File], resultUnit{Unit: u, Hunks: u.Hunks})
	}
	for _, f := range src.Files {
		us := byFile[f.Path]
		sort.SliceStable(us, func(i, j int) bool { return us[i].Line < us[j].Line })
		r.Files = append(r.Files, resultFile{FileDiff: f, Units: us})
	}
	if prev := t.earlierReview(key); prev != nil {
		r.Resolved = resolvedSince(prev, r)
	}
	if err := t.saveResult(r); err != nil {
		return r, err
	}
	if pipe.Summarizer != nil {
		t.sequenceAfter(key, o)
		// Translated in the background, so the triage does not wait for it.
		// Opening the PR meanwhile waits on this call rather than starting
		// another; a failure means it is translated when the PR is opened.
		if !triage.IsEnglish(o.summaryLang) {
			if done := t.track(); done != nil {
				go func() {
					defer done()
					ctx, cancel := t.detached(ctx)
					defer cancel()
					if _, err := t.translate(ctx, r, o); err != nil {
						log.Printf("translate %s to %s: %v", key, o.summaryLang, err)
					}
				}()
			}
		}
	}
	return r, nil
}

// runBy describes the models a run with o uses: the one that places the
// units, and the one that reviews them.
func runBy(o options) (classifier, summarizer string) {
	classifier, summarizer = describe(o.classifier, o.classifyModel, o.classifyEffort), describe(o.summarizer, o.summaryModel, o.reviewEffort)
	switch {
	case o.summarizer != "off":
		// The review call placed the units.
		classifier = summarizer
	case o.classifier == "openjev":
		classifier += " → " + describe(o.fallback, o.fallbackModel, o.classifyEffort)
	}
	if o.reviewTools && (o.summarizer == "codex" || o.summarizer == "claude-code") {
		summarizer += " +repo tools"
	}
	return classifier, summarizer
}

func describe(provider, model, effort string) string {
	s := provider
	if model != "" {
		s += "/" + model
	}
	if (provider == llm.OpenAIAPI || provider == llm.AzureOpenAI || provider == "codex" || provider == "claude-code") && effort != "" {
		s += " @" + effort // reasoning effort; other providers ignore it
	}
	return s
}

func (t *triager) Load(key string) (*PRResult, error) {
	if strings.ContainsAny(key, `/\`) {
		return nil, errors.New("bad key")
	}
	b, err := os.ReadFile(filepath.Join(t.results, key+".json"))
	if err != nil {
		return nil, err
	}
	// Results cached before "danger" was renamed "impact" have the same
	// shape under the old key.
	b = bytes.ReplaceAll(b, []byte(`"danger":`), []byte(`"impact":`))
	var r PRResult
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if len(r.Budgets) == 0 { // cached before review budgets
		tp := triage.DefaultTierPolicy()
		var units []*triage.Unit
		for _, f := range r.Files {
			for _, u := range f.Units {
				units = append(units, u.Unit)
			}
		}
		triage.Rescore(units, tp)
		_, r.ReviewBudget, _ = tp.Budget("")
		r.Budgets = tp.OrderedBudgets()
		r.Counts = (&triage.Report{Units: units}).Counts()
	}
	// Results cached before lift_floors: built-in steps get it back.
	def := triage.DefaultTierPolicy().Budgets
	for i, b := range r.Budgets {
		if d, ok := def[b.Name]; ok && d.Trust == b.Trust && d.Human == b.Human && d.Skim == b.Skim {
			r.Budgets[i].LiftFloors = d.LiftFloors
		}
	}
	// What a person has rejected is applied every time a result loads,
	// from the repository's record. A result saved after a load (the
	// overview, the sequence, a thread refresh) can carry those marks,
	// but all of it is worked out again here, pins, attention and counts
	// included, so restoring one still puts its unit back where the
	// review put it.
	t.dismissed.apply(&r)
	return &r, nil
}

func (t *triager) List() ([]prSummary, error) {
	paths, err := filepath.Glob(filepath.Join(t.results, "*.json"))
	if err != nil {
		return nil, err
	}
	out := []prSummary{}
	for _, p := range paths {
		r, err := t.Load(strings.TrimSuffix(filepath.Base(p), ".json"))
		if err != nil {
			continue
		}
		out = append(out, prSummary{Key: r.Key, PR: r.PR.PRRef, Title: r.PR.Title, State: r.PR.State, LocalPath: r.PR.LocalPath, Rev: r.PR.Rev, HeadRef: r.PR.HeadRef,
			Classifier: r.Classifier, CreatedAt: r.CreatedAt, Counts: r.Counts, Impact: r.Impact, Likelihood: r.Likelihood, Attention: r.Attention})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PR.Number != out[j].PR.Number {
			return out[i].PR.Number > out[j].PR.Number
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// newJob registers a running job. The context carries the job's activity
// log; progress also logs each new stage to it.
func (t *triager) newJob(kind, url string) (*job, context.Context, func(stage string, done, total int)) {
	var idb [6]byte
	_, _ = rand.Read(idb[:])
	j := &job{ID: hex.EncodeToString(idb[:]), Kind: kind, URL: url, Started: time.Now(), Status: "running", log: activity.New()}
	t.mu.Lock()
	t.jobs[j.ID] = j
	if !t.closing {
		t.running.Add(1)
		j.done = t.running.Done
	}
	t.mu.Unlock()
	ctx := activity.With(t.root, j.log)
	activity.Printf(ctx, "started %s", url)
	return j, ctx, func(stage string, done, total int) {
		t.mu.Lock()
		changed := j.Stage != stage
		j.Stage, j.Done, j.Total = stage, done, total
		t.mu.Unlock()
		if changed {
			activity.Printf(ctx, "stage %s", stage)
		}
	}
}

// warn sets a job's warning and logs it.
func (t *triager) warn(ctx context.Context, jobID, msg string) {
	t.mu.Lock()
	if j := t.jobs[jobID]; j != nil {
		j.Warning = msg
	}
	t.mu.Unlock()
	activity.Printf(ctx, "warning: %s", msg)
}

// finish records the job's outcome in its log.
func (j *job) finish(err error) {
	j.log.Close(err)
	if j.done != nil {
		j.done()
	}
}

// track counts background work that shutdown waits for. It returns nil
// once shutdown began; otherwise call the returned func when done.
func (t *triager) track() func() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing {
		return nil
	}
	t.running.Add(1)
	return t.running.Done
}

// detached is ctx's values without its cancellation, for work that outlives
// the caller: it is cancelled on shutdown instead.
func (t *triager) detached(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(t.root, cancel)
	return ctx, func() { stop(); cancel() }
}

// shutdown cancels every job and waits up to timeout for them to return,
// so the LLM CLIs they started are killed rather than left editing a
// checkout after the app exits. It is safe to call more than once.
func (t *triager) shutdown(timeout time.Duration) {
	t.stopOnce.Do(func() {
		t.mu.Lock()
		t.closing = true
		t.mu.Unlock()
		t.cancelRoot()
		done := make(chan struct{})
		go func() {
			t.running.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(timeout):
			log.Printf("shutdown: jobs still running after %s", timeout)
		}
	})
}

// withRoot cancels each request's context on shutdown too, and has shutdown
// wait for it, since handlers call LLMs as well.
func (t *triager) withRoot(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if done := t.track(); done != nil {
			defer done()
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		defer context.AfterFunc(t.root, cancel)()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// startIndex runs indexSources as a job; the job's Result is the summary.
func (t *triager) startIndex(jo jobOptions) *job {
	o := t.options(jo)
	j, ctx, progress := t.newJob("index", "index")
	go func() {
		res, err := indexSources(ctx, o, progress)
		j.finish(err)
		t.mu.Lock()
		defer t.mu.Unlock()
		if err != nil {
			j.Status, j.Error = "error", err.Error()
			log.Printf("index: %v", err)
			return
		}
		j.Status, j.Result = "done", res
		log.Printf("index: linked %d, cloned %d, updated %d, failed %d; %d repos", len(res.Linked), len(res.Cloned), len(res.Updated), len(res.Failed), res.Repos)
	}()
	return j
}

func (t *triager) start(url string, jo jobOptions) (*job, error) {
	ref, err := triage.ParsePRRef(url)
	if err != nil {
		return nil, err
	}
	j, ctx, progress := t.newJob("triage", ref.URL())
	go func() {
		defer t.saveJobLog(j) // after the job's outcome is set below
		r, err := t.Run(ctx, ref, jo, progress)
		j.finish(err)
		var now, nowReview string
		if err == nil {
			now, nowReview = runBy(t.options(jo))
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		if err != nil {
			j.Status, j.Error = "error", err.Error()
			log.Printf("triage %s: %v", j.URL, err)
			return
		}
		j.Status, j.Key = "done", r.Key
		j.markCached(r)
		j.noteModel(r, now, nowReview)
		log.Printf("triage %s: %v (%s)", j.URL, r.Counts, r.Key)
	}()
	return j, nil
}

// markCached records a result made before the job started as reused.
func (j *job) markCached(r *PRResult) {
	if r.CreatedAt.Before(j.Started) {
		j.Cached = &r.CreatedAt
	}
}

// noteModel records, on a job that reused a result, which models made it
// and which ones a re-run would use (see runBy), when they differ: the
// placing model, and the reviewer with its repo tools. Results saved
// without a summarizer are compared by the placing model alone.
func (j *job) noteModel(r *PRResult, classifier, summarizer string) {
	if j.Cached == nil {
		return
	}
	if r.Summarizer == "" {
		if classifier != r.Classifier {
			j.CachedBy, j.RunsWith = r.Classifier, classifier
		}
		return
	}
	was, now := modelLabel(r.Classifier, r.Summarizer), modelLabel(classifier, summarizer)
	if was != now {
		j.CachedBy, j.RunsWith = was, now
	}
}

// modelLabel names a run's models in one string: the reviewer alone when
// it placed the units (with its "+repo tools"), else both.
func modelLabel(classifier, summarizer string) string {
	if summarizer == "" || summarizer == classifier {
		return classifier
	}
	if strings.HasPrefix(summarizer, classifier) {
		return summarizer
	}
	return classifier + ", review " + summarizer
}

// snapshot copies j under t.mu, for encoding while its worker updates it.
func (t *triager) snapshot(j *job) job {
	t.mu.Lock()
	defer t.mu.Unlock()
	return *j
}

// jobLog is the activity of a job, for the UI to watch while it runs.
func (t *triager) jobLog(id string) ([]activity.Thread, bool) {
	t.mu.Lock()
	j, ok := t.jobs[id]
	t.mu.Unlock()
	if !ok {
		return nil, false
	}
	return j.log.Snapshot(), true
}

// jobList is every job of this server run, newest first.
func (t *triager) jobList() []job {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]job, 0, len(t.jobs))
	for _, j := range t.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Started.After(out[b].Started) })
	return out
}

func (t *triager) job(id string) (job, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[id]
	if !ok {
		return job{}, false
	}
	return *j, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// shutdownWait bounds how long quitting waits for jobs to stop.
const shutdownWait = 5 * time.Second

// newServeHandler returns the UI and API handler and a func that stops its
// jobs (see triager.shutdown), to call when the app quits.
func newServeHandler(o options) (http.Handler, func(), error) {
	t, err := newTriager(o)
	if err != nil {
		return nil, nil, err
	}
	rv, err := newReviews(o, t.fetcher)
	if err != nil {
		return nil, nil, err
	}
	t.reviews = rv
	static, _ := fs.Sub(uiFS, "ui")
	mux := http.NewServeMux()
	rv.routes(mux, t)
	t.chatActionRoutes(mux)
	t.dismissed.routes(mux, t)
	newUISettings(o.cache).routes(mux)
	treemapRoute(mux, o)
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		d := resolveProviders(o)
		writeJSON(w, 200, map[string]any{
			"classifier": d.classifier, "classify_model": d.classifyModel,
			"summarizer": d.summarizer, "summary_model": d.summaryModel,
			"translator": d.translator, "translate_model": d.translateModel, "translate_effort": d.translateEffort,
			"chat": d.chat, "chat_model": d.chatModel,
			"classify_effort": d.classifyEffort, "review_effort": d.reviewEffort,
			"review_dry_run": o.reviewDryRun,
			"review_tools":   o.reviewTools,
			"classify_batch": o.classifyBatch != 1,
			"lint":           lintOn(o),
			"incremental":    o.incremental,
			"linters":        triage.KnownLinters(),
			"summary_lang":   o.summaryLang,
			"review_budget":  orDefault(o.reviewBudget, triage.DefaultBudget),
			"recursive_fix":  true,
			"fix_agent":      true,
			"max_fix_rounds": 3,
			"fix_location":   "worktree",
			"budgets":        triage.DefaultTierPolicy().OrderedBudgets(),
			"codemap":        codeMapVersion(loadCodeMap(o.codemapDir)),
			"codemap_repos":  codeMapRepos(loadCodeMap(o.codemapDir)),
			"code_root":      o.codeRoot,
			"org":            o.org,
			"gh_host":        triage.DefaultHost(),
		})
	})
	mux.HandleFunc("POST /api/codemap/index", func(w http.ResponseWriter, r *http.Request) {
		var jo jobOptions
		if err := json.NewDecoder(r.Body).Decode(&jo); err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 202, t.snapshot(t.startIndex(jo)))
	})
	mux.HandleFunc("GET /api/providers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, providerList(r.Context(), o, r.URL.Query().Has("refresh")))
	})
	mux.HandleFunc("GET /api/results", func(w http.ResponseWriter, r *http.Request) {
		list, err := t.List()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, list)
	})
	mux.HandleFunc("GET /api/results/{key}", func(w http.ResponseWriter, r *http.Request) {
		res, err := t.Load(r.PathValue("key"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, res)
	})
	mux.HandleFunc("POST /api/results/{key}/translate", func(w http.ResponseWriter, r *http.Request) {
		var jo jobOptions
		if err := json.NewDecoder(r.Body).Decode(&jo); err != nil {
			writeErr(w, 400, err)
			return
		}
		res, err := t.Load(r.PathValue("key"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		tr, err := t.translation(r.Context(), res, jo)
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		writeJSON(w, 200, tr)
	})
	mux.HandleFunc("POST /api/results/{key}/overview", func(w http.ResponseWriter, r *http.Request) {
		var jo jobOptions
		if err := json.NewDecoder(r.Body).Decode(&jo); err != nil {
			writeErr(w, 400, err)
			return
		}
		ov, err := t.overview(r.Context(), r.PathValue("key"), jo)
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		writeJSON(w, 200, ov)
	})
	mux.HandleFunc("POST /api/results/{key}/sequence", func(w http.ResponseWriter, r *http.Request) {
		var jo jobOptions
		if err := json.NewDecoder(r.Body).Decode(&jo); err != nil {
			writeErr(w, 400, err)
			return
		}
		sq, err := t.sequence(r.Context(), r.PathValue("key"), jo)
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		writeJSON(w, 200, sq)
	})
	mux.HandleFunc("POST /api/results/{key}/chat", func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, chatMaxBody)).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		res, err := t.Load(r.PathValue("key"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		out, err := t.chat(r.Context(), res, req)
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("POST /api/triage", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			URL  string `json:"url"`
			Path string `json:"path"`
			jobOptions
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		var j *job
		if req.Path != "" {
			j, err = t.startLocal(req.Path, req.jobOptions)
		} else {
			j, err = t.start(req.URL, req.jobOptions)
		}
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 202, t.snapshot(j))
	})
	mux.HandleFunc("GET /api/revs", func(w http.ResponseWriter, r *http.Request) {
		revs, err := listRevs(r.URL.Query().Get("path"))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, revs)
	})
	mux.HandleFunc("POST /api/local/{key}/publish", func(w http.ResponseWriter, r *http.Request) {
		res, err := t.Load(r.PathValue("key"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		url, err := publishLocal(res)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		res.PR.URL = url
		res.PR.State = "OPEN"
		if _, err := t.updateResult(res.Key, func(r *PRResult) {
			r.PR.URL, r.PR.State = url, "OPEN"
		}); err != nil {
			log.Printf("save published PR link: %v", err)
		}
		if ref, err := triage.ParsePRRef(url); err == nil {
			localRef := triage.PRRef{Owner: "local", Repo: localPathID(res.PR.LocalPath)}
			rv.dmu.Lock()
			if ds, err := rv.load(localRef); err == nil && len(ds) > 0 {
				// Added to any drafts the PR already has, by ID.
				have, err := rv.load(ref)
				if err != nil {
					have = nil
				}
				seen := map[string]bool{}
				for _, d := range have {
					seen[d.ID] = true
				}
				for _, d := range ds {
					if !seen[d.ID] {
						have = append(have, d)
					}
				}
				if err := rv.save(ref, have); err != nil {
					log.Printf("copy local review notes: %v", err)
				}
			}
			rv.dmu.Unlock()
		}
		writeJSON(w, 200, map[string]string{"url": url})
	})
	mux.HandleFunc("POST /api/fix", func(w http.ResponseWriter, r *http.Request) {
		var req fixRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		j, err := t.startFix(req)
		if errors.Is(err, errUncommitted) {
			writeJSON(w, 409, map[string]string{"error": err.Error(), "code": "uncommitted"})
			return
		}
		if errors.Is(err, errRev) {
			writeJSON(w, 409, map[string]string{"error": err.Error(), "code": "rev"})
			return
		}
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 202, t.snapshot(j))
	})
	// A PR's fix checkouts: what they hold, their changes, and committing,
	// pushing or discarding them (see fixpush.go).
	mux.HandleFunc("GET /api/results/{key}/fixes", func(w http.ResponseWriter, r *http.Request) {
		res, err := t.Load(r.PathValue("key"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		fixes, marks, running, err := t.pendingFixes(r.Context(), res)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		if fixes == nil {
			fixes = []*pendingFix{}
		}
		writeJSON(w, 200, map[string]any{"fixes": fixes, "marks": marks, "running": running})
	})
	// The PR's branch in its repository's fix checkout (see fixbranch.go).
	mux.HandleFunc("GET /api/results/{key}/fixes/commit/{commit}", func(w http.ResponseWriter, r *http.Request) {
		files, err := t.branchCommitDiff(r.Context(), r.PathValue("key"), r.PathValue("commit"))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]any{"files": files})
	})
	mux.HandleFunc("POST /api/results/{key}/fixes/push", func(w http.ResponseWriter, r *http.Request) {
		head, err := t.pushBranch(r.Context(), r.PathValue("key"))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]string{"head": head})
	})
	mux.HandleFunc("POST /api/results/{key}/fixes/drop", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Commit string `json:"commit"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := t.dropFix(r.Context(), r.PathValue("key"), req.Commit); err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/results/{key}/fixes/complete", func(w http.ResponseWriter, r *http.Request) {
		res, err := t.Load(r.PathValue("key"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		if res.PR == nil || res.PR.LocalPath != "" {
			writeErr(w, 400, errors.New("only a PR's fixes are completed"))
			return
		}
		if err := t.complete(res.PR.PRRef); err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/results/{key}/fix/diff", func(w http.ResponseWriter, r *http.Request) {
		res, err := t.loadFix(r.PathValue("key"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		p, err := inspectFix(r.Context(), res, true)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, p)
	})
	mux.HandleFunc("POST /api/results/{key}/fix/commit", func(w http.ResponseWriter, r *http.Request) {
		var jo jobOptions
		if err := json.NewDecoder(r.Body).Decode(&jo); err != nil {
			writeErr(w, 400, err)
			return
		}
		p, err := t.commitFix(r.Context(), t.options(jo), r.PathValue("key"))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, p)
	})
	mux.HandleFunc("POST /api/fixes/push", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			jobOptions
			Keys []string `json:"keys"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		head, err := t.pushFixes(r.Context(), t.options(req.jobOptions), req.Keys)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]string{"head": head})
	})
	mux.HandleFunc("POST /api/results/{key}/fix/discard", func(w http.ResponseWriter, r *http.Request) {
		if err := t.discardFix(r.PathValue("key")); err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, t.jobList())
	})
	mux.HandleFunc("GET /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, ok := t.job(r.PathValue("id"))
		if !ok {
			writeErr(w, 404, errors.New("no such job"))
			return
		}
		writeJSON(w, 200, j)
	})
	mux.HandleFunc("GET /api/jobs/{id}/log", func(w http.ResponseWriter, r *http.Request) {
		threads, ok := t.jobLog(r.PathValue("id"))
		if !ok {
			writeErr(w, 404, errors.New("no such job"))
			return
		}
		writeJSON(w, 200, threads)
	})

	// Fixes made each in a worktree of its own move onto their PR's
	// branch, and PRs nobody fixed for a while are completed.
	if done := t.track(); done != nil {
		go func() {
			defer done()
			t.migrateFixes(t.root)
			for {
				t.autoComplete()
				select {
				case <-t.root.Done():
					return
				case <-time.After(time.Hour):
				}
			}
		}()
	}
	return t.withRoot(mux), func() { t.shutdown(shutdownWait) }, nil
}

// defaultAddr is a fixed port, so the UI's URL stays the same across
// restarts.
const defaultAddr = "127.0.0.1:8765"

func runServe(ctx context.Context, o options) error {
	mux, stopJobs, err := newServeHandler(o)
	if err != nil {
		return err
	}
	defer stopJobs()
	ln, err := net.Listen("tcp", o.addr)
	if err != nil && o.addr == defaultAddr {
		log.Printf("%s is taken (%v); using another port", defaultAddr, err)
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return fmt.Errorf("%w (another pr-manager serve running? `make stop` or `lsof -iTCP:%s`)", err, portOf(o.addr))
	}
	addr := ln.Addr().(*net.TCPAddr)
	host := addr.IP.String()
	if addr.IP.IsUnspecified() {
		host = "127.0.0.1"
		if addr.IP.To4() == nil {
			host = "::1"
		}
	}
	url := "http://" + net.JoinHostPort(host, fmt.Sprint(addr.Port))
	log.Printf("pr-manager UI on %s (Ctrl+C to stop)", url)
	go func() {
		if err := openBrowser(ctx, url); err != nil {
			log.Printf("could not open browser: %v; open %s manually", err, url)
		}
	}()
	srv := &http.Server{Handler: mux}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		log.Printf("shutting down")
		stopJobs() // cancels in-flight requests too, so Shutdown doesn't wait on them
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-stopped // Serve returns as soon as Shutdown starts
	return nil
}

func openBrowser(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "darwin":
		return proc.CommandContext(ctx, "open", url).Run()
	case "windows":
		return proc.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url).Run()
	default:
		return proc.CommandContext(ctx, "xdg-open", url).Run()
	}
}

// runPRs triages every PR by an author, sequentially, into the cache.
func runPRs(ctx context.Context, o options) error {
	t, err := newTriager(o)
	if err != nil {
		return err
	}
	refs, err := triage.ListPRs(o.repo, o.author, o.state, o.limit)
	if err != nil {
		return err
	}
	fmt.Printf("%d PRs by %s in %s (%s)\n", len(refs), o.author, o.repo, describe(resolveProviders(o).classifier, resolveProviders(o).classifyModel, o.classifyEffort))
	for _, ref := range refs {
		start := time.Now()
		r, err := t.Run(ctx, ref, jobOptions{Force: o.force}, func(string, int, int) {})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			fmt.Printf("#%-5d ERROR %v\n", ref.Number, err)
			continue
		}
		fmt.Printf("#%-5d human=%-3d skim=%-3d aux=%-3d none=%-3d %5.1fs  %s\n", ref.Number,
			r.Counts[triage.BucketHuman], r.Counts[triage.BucketSkim], r.Counts[triage.BucketAux], r.Counts[triage.BucketNone],
			time.Since(start).Seconds(), r.PR.Title)
	}
	return nil
}

func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return addr
}

// providerChoice is one provider the UI can offer, with its models and the
// defaults for each role.
type providerChoice struct {
	ID              string      `json:"id"`
	Reason          string      `json:"reason"`
	Live            bool        `json:"live"`
	Models          []llm.Model `json:"models"`
	ClassifyModel   string      `json:"classify_model,omitempty"`
	TranslateModel  string      `json:"translate_model,omitempty"`
	TranslateEffort string      `json:"translate_effort,omitempty"`
	SummaryModel    string      `json:"summary_model,omitempty"`
	ChatModel       string      `json:"chat_model,omitempty"` // the summary model's defaults
	Summarize       bool        `json:"summarize"`            // openjev only classifies
}

// providerList is GET /api/providers: the providers this machine can run
// (logged-in CLIs, API keys that are set, a running Ollama or OpenJev), the
// ones it can't and why, and the server's defaults.
func providerList(ctx context.Context, o options, refresh bool) map[string]any {
	d := resolveProviders(o)
	var avail []providerChoice
	var missing []map[string]string
	for _, c := range llm.Catalogs(ctx, o.openjevURL, refresh) {
		if !c.Available {
			missing = append(missing, map[string]string{"id": c.Provider, "reason": c.Reason})
			continue
		}
		pc := providerChoice{ID: c.Provider, Reason: c.Reason, Live: c.Live, Models: c.Models,
			ClassifyModel: llm.PickModel(c, classifyDefaults[c.Provider]...), SummaryModel: llm.PickModel(c, summaryDefaults[c.Provider]...),
			TranslateModel: llm.PickModel(c, translatePrefs(c.Provider)...), Summarize: c.Provider != "openjev", TranslateEffort: o.translateEffort}
		pc.ChatModel = pc.SummaryModel
		if pc.TranslateEffort == "auto" { // a -translate-effort flag applies to every provider
			pc.TranslateEffort = orDefault(translateEfforts[c.Provider], "low")
		}
		// A default the list doesn't show (hidden, or a built-in list) is
		// still offered.
		for _, m := range []string{pc.SummaryModel, pc.ClassifyModel, pc.TranslateModel} {
			if m != "" && !hasModel(pc.Models, m) {
				pc.Models = append([]llm.Model{{ID: m}}, pc.Models...)
			}
		}
		avail = append(avail, pc)
	}
	return map[string]any{
		"providers": avail, "unavailable": missing,
		"classifier": d.classifier, "classify_model": d.classifyModel,
		"summarizer": d.summarizer, "summary_model": d.summaryModel,
		"translator": d.translator, "translate_model": d.translateModel,
		"chat": d.chat, "chat_model": d.chatModel,
	}
}

func hasModel(ms []llm.Model, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

func codeMapRepos(m *codemap.Map) int {
	if m == nil {
		return 0
	}
	return len(m.Meta.Repos)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// lintOn is whether a triage lints under the server's -lint flag, read the
// way the pipeline reads it, so "none" and "false" show as off too.
func lintOn(o options) bool {
	p := triage.DefaultLintPolicy()
	if o.lintSet && p.Set(o.lint) != nil {
		return true
	}
	return p.Enabled
}
