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
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/codemap"
	"github.com/amitbet/pr-manager/internal/activity"
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
	// SummaryLang is set on results from before translation, reviewed in
	// that language. Newer results are English; see translation.
	SummaryLang string                `json:"summary_lang,omitempty"`
	CreatedAt   time.Time             `json:"created_at"`
	DurationMS  int64                 `json:"duration_ms"`
	Counts      map[triage.Bucket]int `json:"counts"`
	// Overview is nil for results from before overviews, and when it
	// could not be written; see overview.
	Overview *triage.Overview `json:"overview,omitempty"`
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
	HeadRef    string                `json:"head_ref,omitempty"`
}

// triager runs PR triage jobs and caches results as JSON files.
type triager struct {
	opts    options
	fetcher *triage.PRFetcher
	results string

	mu    sync.Mutex
	jobs  map[string]*job
	fixMu sync.Mutex
	trMu  sync.Mutex // guards trLocks
	// trLocks has a lock per translation file, so one PR is translated once.
	trLocks map[string]*sync.Mutex
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
	Key     string    `json:"key,omitempty"`
	Result  any       `json:"result,omitempty"` // index jobs
	// Cached is when the result a triage job reused was made, if it
	// reused one instead of running.
	Cached *time.Time `json:"cached,omitempty"`

	log *activity.Log
}

func newTriager(o options) (*triager, error) {
	t := &triager{
		opts:    o,
		fetcher: &triage.PRFetcher{Dir: filepath.Join(o.cache, "repos")},
		results: filepath.Join(o.cache, "results"),
		jobs:    map[string]*job{},
		trLocks: map[string]*sync.Mutex{},
	}
	return t, os.MkdirAll(t.results, 0o755)
}

// jobOptions overrides the server's provider flags with non-empty values.
type jobOptions struct {
	Classifier    string  `json:"classifier"`
	ClassifyModel string  `json:"classify_model"`
	Summarizer    string  `json:"summarizer"`
	SummaryModel  string  `json:"summary_model"`
	ReviewTools   *bool   `json:"review_tools"`
	SummaryLang   *string `json:"summary_lang"`
	CodeRoot      *string `json:"code_root"`
	Org           *string `json:"org"`
	Force         bool    `json:"force"`
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

func cacheKey(ref triage.PRRef, head string, o options) string {
	parts := []string{triage.PromptVersion, o.classifier, o.classifyModel, o.fallback, o.fallbackModel, o.summarizer, o.summaryModel, o.classifyEffort, o.reviewEffort, fmt.Sprint(o.reviewTools), codeMapVersion(loadCodeMap(o.codemapDir))}
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return fmt.Sprintf("%s__%.10s__%x", ref.FileKey(), head, h[:4])
}

// latestCached returns the newest cached result for this PR head from any
// prompt, provider or code-map version. A PR that was already triaged is
// only re-run when asked (force), not because the pipeline changed. Only
// English results count: other languages are translated from them.
func (t *triager) latestCached(ref triage.PRRef, head string) (*PRResult, error) {
	pattern := filepath.Join(t.results, fmt.Sprintf("%s__%.10s__*.json", ref.FileKey(), head))
	paths, _ := filepath.Glob(pattern)
	var best *PRResult
	for _, p := range paths {
		r, err := t.Load(strings.TrimSuffix(filepath.Base(p), ".json"))
		if err == nil && r.SummaryLang == "" && (best == nil || r.CreatedAt.After(best.CreatedAt)) {
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
	if !jo.Force {
		if r, err := t.Load(key); err == nil {
			return t.withThreads(ctx, r, o, progress), nil
		}
		if r, err := t.latestCached(ref, info.HeadOid); err == nil {
			return t.withThreads(ctx, r, o, progress), nil
		}
	}
	src, err := t.fetcher.Source(ctx, info)
	if err != nil {
		return nil, err
	}
	if err := ensureCodeMap(ctx, o, ref, progress); err != nil {
		return nil, err
	}
	key = cacheKey(ref, info.HeadOid, o) // a build changes the map version
	policy, gitattrs, err := sourceConfig(src)
	if err != nil {
		return nil, err
	}
	pipe, err := buildPipeline(o, policy, gitattrs)
	if err != nil {
		return nil, err
	}
	pipe.Progress = progress
	if m := loadCodeMap(o.codemapDir); m != nil {
		pipe.CodeMap = &triage.CodeMap{Map: m, Repo: ref.Repo}
	}
	return t.runSource(ctx, key, info, src, pipe, o)
}

func (t *triager) runSource(ctx context.Context, key string, info *triage.PRInfo, src *triage.Source, pipe *triage.Pipeline, o options) (*PRResult, error) {
	start := time.Now()
	units := pipe.Run(ctx, src)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Before the overview, so confirmed comments are in it.
	threads := t.refreshThreads(ctx, info, units, o, pipe.Progress)

	r := &PRResult{
		Key: key, PR: info, CreatedAt: time.Now(), DurationMS: time.Since(start).Milliseconds(),
		Classifier: describe(o.classifier, o.classifyModel, o.classifyEffort), Summarizer: describe(o.summarizer, o.summaryModel, o.reviewEffort),
		Counts: (&triage.Report{Units: units}).Counts(), Threads: threads,
	}
	r.Impact, r.Likelihood, r.Attention = (&triage.Report{Units: units}).Scores()
	if pipe.CodeMap != nil {
		r.CodeMap = codeMapVersion(pipe.CodeMap.Map)
	}
	tiers := pipe.Presorter.Policy.Tiers
	_, r.ReviewBudget, _ = tiers.Budget("")
	r.Budgets = tiers.OrderedBudgets()
	if o.classifier == "openjev" {
		r.Classifier += " → " + describe(o.fallback, o.fallbackModel, o.classifyEffort)
	}
	if o.reviewTools && (o.summarizer == "codex" || o.summarizer == "claude-code") {
		r.Summarizer += " +repo tools"
	}
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
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return r, os.WriteFile(filepath.Join(t.results, key+".json"), b, 0o644)
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
		out = append(out, prSummary{Key: r.Key, PR: r.PR.PRRef, Title: r.PR.Title, State: r.PR.State, LocalPath: r.PR.LocalPath, HeadRef: r.PR.HeadRef,
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
	t.mu.Unlock()
	ctx := activity.With(context.Background(), j.log)
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

// finish records the job's outcome in its log.
func (j *job) finish(err error) { j.log.Close(err) }

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
		r, err := t.Run(ctx, ref, jo, progress)
		j.finish(err)
		t.mu.Lock()
		defer t.mu.Unlock()
		if err != nil {
			j.Status, j.Error = "error", err.Error()
			log.Printf("triage %s: %v", j.URL, err)
			return
		}
		j.Status, j.Key = "done", r.Key
		j.markCached(r)
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

func newServeHandler(o options) (http.Handler, error) {
	t, err := newTriager(o)
	if err != nil {
		return nil, err
	}
	rv, err := newReviews(o, t.fetcher)
	if err != nil {
		return nil, err
	}
	static, _ := fs.Sub(uiFS, "ui")
	mux := http.NewServeMux()
	rv.routes(mux, t)
	newUISettings(o.cache).routes(mux)
	treemapRoute(mux, o)
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		d := resolveProviders(o)
		writeJSON(w, 200, map[string]any{
			"classifier": d.classifier, "classify_model": d.classifyModel,
			"summarizer": d.summarizer, "summary_model": d.summaryModel,
			"classify_effort": d.classifyEffort, "review_effort": d.reviewEffort,
			"review_dry_run": o.reviewDryRun,
			"review_tools":   o.reviewTools,
			"summary_lang":   o.summaryLang,
			"review_budget":  orDefault(o.reviewBudget, triage.DefaultBudget),
			"recursive_fix":  true,
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
		writeJSON(w, 202, t.startIndex(jo))
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
		writeJSON(w, 202, j)
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
		if b, err := json.Marshal(res); err == nil {
			if err = os.WriteFile(filepath.Join(t.results, res.Key+".json"), b, 0644); err != nil {
				log.Printf("save published PR link: %v", err)
			}
		}
		if ref, err := triage.ParsePRRef(url); err == nil {
			localRef := triage.PRRef{Owner: "local", Repo: localPathID(res.PR.LocalPath)}
			if ds, err := rv.load(localRef); err == nil && len(ds) > 0 {
				if err := rv.save(ref, ds); err != nil {
					log.Printf("copy local review notes: %v", err)
				}
			}
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
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 202, j)
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

	return mux, nil
}

// defaultAddr is a fixed port, so the UI's URL stays the same across
// restarts.
const defaultAddr = "127.0.0.1:8765"

func runServe(ctx context.Context, o options) error {
	mux, err := newServeHandler(o)
	if err != nil {
		return err
	}
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
	go func() {
		<-ctx.Done()
		log.Printf("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func openBrowser(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "darwin":
		return exec.CommandContext(ctx, "open", url).Run()
	case "windows":
		return exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url).Run()
	default:
		return exec.CommandContext(ctx, "xdg-open", url).Run()
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
		fmt.Printf("#%-5d human=%-3d skim=%-3d none=%-3d %5.1fs  %s\n", ref.Number,
			r.Counts[triage.BucketHuman], r.Counts[triage.BucketSkim], r.Counts[triage.BucketNone],
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
	ID            string      `json:"id"`
	Reason        string      `json:"reason"`
	Live          bool        `json:"live"`
	Models        []llm.Model `json:"models"`
	ClassifyModel string      `json:"classify_model,omitempty"`
	SummaryModel  string      `json:"summary_model,omitempty"`
	Summarize     bool        `json:"summarize"` // openjev only classifies
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
			ClassifyModel: classifyDefaults[c.Provider], SummaryModel: summaryDefaults[c.Provider], Summarize: c.Provider != "openjev"}
		// A default the list doesn't show (hidden or older) is still offered.
		for _, m := range []string{pc.SummaryModel, pc.ClassifyModel} {
			if m != "" && !hasModel(pc.Models, m) {
				pc.Models = append([]llm.Model{{ID: m}}, pc.Models...)
			}
		}
		// Ollama's defaults are whatever is pulled if the preset isn't.
		if c.Provider == "ollama" && len(c.Models) > 0 && !hasModel(c.Models, llm.OllamaQwen35_9B) {
			pc.Models, pc.ClassifyModel, pc.SummaryModel = c.Models, c.Models[0].ID, c.Models[0].ID
		}
		avail = append(avail, pc)
	}
	return map[string]any{
		"providers": avail, "unavailable": missing,
		"classifier": d.classifier, "classify_model": d.classifyModel,
		"summarizer": d.summarizer, "summary_model": d.summaryModel,
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
