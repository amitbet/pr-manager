// pr-manager sorts a branch's diff into: needs human review, skim the
// generated summary, or no review needed.
//
//	pr-manager [flags]                       triage base...head in -C dir
//	pr-manager -pr URL [flags]               triage a GitHub PR
//	pr-manager eval -fixtures DIR [flags]    score against labeled past PRs
//	pr-manager serve [-addr host:port]       web UI (make ui)
//	pr-manager prs -repo o/r -author login   triage an author's PRs into the UI cache
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/amitbet/pr-manager/codemap"
	"github.com/amitbet/pr-manager/codemap/indexer"
	"github.com/amitbet/pr-manager/internal/appdirs"
	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

type options struct {
	dir, base, head, diffFile, policy, out string
	outFile, pr                            string
	classifier, classifyModel              string
	fallback, fallbackModel                string
	summarizer, summaryModel               string
	openjevURL                             string
	codemapDir, mapRepo                    string
	codemapConfig                          string
	codeRoot, org                          string // code map sources, see codemap_build.go
	classifyEffort, reviewEffort           string
	// translator, translateModel and translateEffort pick the model that
	// translates to -summary-lang; see translator.
	translator, translateModel, translateEffort string
	reviewTools                                 bool
	// incremental lets a re-run of the same PR keep the review of units
	// whose diff and surroundings did not move.
	incremental bool
	// groupReview is only consulted when the flag was given; otherwise
	// grouping follows the policy file.
	groupReview, groupReviewSet bool
	// classifyBatch overrides classify_batch.max_units when > 0.
	classifyBatch int
	// classifyCache keeps classifier decisions under the cache directory.
	classifyCache bool
	// lint is the -lint spec: auto, off or a tool list. lintSet marks the
	// flag as given, so otherwise the policy file decides.
	lint, lintSpecDefault          string
	lintSet                        bool
	summaryLang                    string
	reviewBudget                   string
	concurrency, reviewConcurrency int
	failOnHuman                    bool
	fixtures                       string
	judge                          bool
	// serve / prs
	addr, cache         string
	repo, author, state string
	limit               int
	force               bool
	reviewDryRun        bool
}

var subcommands = map[string]bool{"eval": true, "serve": true, "prs": true, "index": true}

var version = "dev"
var commit = "none"
var date = "unknown"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-version":
			fmt.Printf("pr-manager %s (commit %s, built %s)\n", version, commit, date)
			return
		case "codemap":
			if err := indexer.Run(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "codemap:", err)
				os.Exit(1)
			}
			return
		}
	}
	cacheRoot, err := appdirs.CacheDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pr-manager:", err)
		os.Exit(1)
	}
	var o options
	fs := flag.NewFlagSet("pr-manager", flag.ExitOnError)
	fs.StringVar(&o.dir, "C", ".", "git repository to triage")
	fs.StringVar(&o.base, "base", "origin/main", "base ref (diff is base...head)")
	fs.StringVar(&o.head, "head", "HEAD", "head ref")
	fs.StringVar(&o.diffFile, "diff", "", "read the diff from this file ('-' for stdin) instead of git; head files are read from -C")
	fs.StringVar(&o.pr, "pr", "", "triage a GitHub PR (URL or owner/repo#N) instead of a local branch")
	fs.StringVar(&o.policy, "policy", "", "policy file (default <C>/.triage.yaml)")
	fs.StringVar(&o.out, "out", "md", "output format: md|json")
	fs.StringVar(&o.outFile, "o", "", "write the report to this file instead of stdout")
	fs.StringVar(&o.classifier, "classifier", "auto", "places units when -summarizer is off (with a summarizer, the review call does): auto|codex|claude-code|openjev|openai-api|claude-api|bedrock|vertex|foundry|azure-openai|ollama|off (auto: codex subscription, else claude-code subscription, else a configured cloud (CLAUDE_CODE_USE_BEDROCK/VERTEX/FOUNDRY, AZURE_OPENAI_ENDPOINT), else OPENAI_API_KEY, else ANTHROPIC_API_KEY, else ollama; openai and anthropic still work as old names)")
	fs.StringVar(&o.classifyModel, "classify-model", "", "classifier model (default per provider)")
	fs.StringVar(&o.fallback, "fallback", "auto", "with -classifier openjev: provider for units OpenJev isn't sure about (off = human)")
	fs.StringVar(&o.fallbackModel, "fallback-model", "", "fallback model")
	fs.StringVar(&o.summarizer, "summarizer", "auto", "auto|codex|claude-code|openai-api|claude-api|bedrock|vertex|foundry|azure-openai|ollama|off")
	fs.StringVar(&o.summaryModel, "summary-model", "", "analyze model: triages, summarizes and reviews each unit in one call (default per provider: a stronger model than the classifier)")
	fs.StringVar(&o.classifyEffort, "classify-effort", "low", "reasoning effort for classify (openai, codex, claude-code): none|minimal|low|medium|high|xhigh ('' = model default)")
	fs.StringVar(&o.reviewEffort, "review-effort", "medium", "reasoning effort for analyze (openai, codex, claude-code; '' = model default)")
	fs.BoolVar(&o.reviewTools, "review-tools", true, "let the codex/claude-code reviewer read the repo at the PR head (a git worktree) and the Go module cache; slower, catches claims about code outside the diff (-review-tools=false to turn off)")
	fs.BoolVar(&o.incremental, "incremental", true, "when a PR is triaged again after a push, keep the review of every unit whose diff, and whose callers, callees, file-mates and moved code, did not change; the rest of the pipeline still runs on the whole diff (-incremental=false, or -force, reviews everything)")
	fs.BoolVar(&o.groupReview, "group-review", true, "review related change units together in one call instead of one call each: fewer, larger review tasks and much less repeated context (-group-review=false to turn off; grouping in .triage.yaml overrides when this flag is not given)")
	fs.StringVar(&o.lint, "lint", "auto", "static analysis over the lines the PR adds, fed to the review prompt, the likelihood score and the Issues tab: auto (the linters the repository is configured for and that are installed, plus the built-in secret scan), off, or a comma-separated list of "+strings.Join(triage.KnownLinters(), ",")+" (lint in .triage.yaml decides when this flag is not given)")
	fs.StringVar(&o.translator, "translator", "auto", "provider that translates to -summary-lang (auto: the reviewer's provider, else the classifier's)")
	fs.StringVar(&o.translateModel, "translate-model", "", "translation model (default per provider: the fastest that translates well)")
	fs.StringVar(&o.translateEffort, "translate-effort", "auto", "reasoning effort for translation (openai, codex, claude-code; auto = per provider, '' = model default)")
	fs.StringVar(&o.summaryLang, "summary-lang", "", "language to translate summaries, review notes and issue text into, e.g. Hebrew or Japanese (default English: no translation)")
	fs.StringVar(&o.reviewBudget, "review-budget", "", "how much goes to human review: "+strings.Join(triage.BudgetNames, "|")+" (default: tiers.review_budget in the policy, else "+triage.DefaultBudget+")")
	fs.StringVar(&o.openjevURL, "openjev-url", "", "OpenJev server (default $OPENJEV_BASE_URL or http://127.0.0.1:8771)")
	fs.StringVar(&o.codemapDir, "codemap", filepath.Join(cacheRoot, "codemap"), "code map directory for impact, file history and tier moves (off to disable; build with pr-manager codemap build)")
	fs.StringVar(&o.codemapConfig, "codemap-config", "", "code-map scoring config (default embedded rules)")
	fs.StringVar(&o.codeRoot, "code-root", os.Getenv("PR_MANAGER_CODE_ROOT"), "code map: local directory of git checkouts (DIR/<repo> or DIR/<org>/<repo>) to index from disk instead of cloning (default $PR_MANAGER_CODE_ROOT)")
	fs.StringVar(&o.org, "org", os.Getenv("PR_MANAGER_ORG"), "code map: GitHub or GitHub Enterprise org or user whose repos `pr-manager index` clones and indexes: name, host/name or https://host/name (default $PR_MANAGER_ORG)")
	fs.StringVar(&o.mapRepo, "map-repo", "", "repo name in the code map for local runs (default: basename of the -C checkout)")
	fs.IntVar(&o.concurrency, "j", 8, "parallel classify calls")
	fs.IntVar(&o.classifyBatch, "classify-batch", 0, "change units per classify call (0: classify_batch.max_units in .triage.yaml, else 6; 1: one call per unit)")
	fs.BoolVar(&o.classifyCache, "classify-cache", true, "keep classifier decisions under -cache, so a unit whose diff did not change is not classified again (-classify-cache=false to turn off; eval never uses it)")
	fs.IntVar(&o.reviewConcurrency, "review-j", 16, "parallel analyze calls (0 = same as -j)")
	fs.BoolVar(&o.failOnHuman, "fail-on-human", false, "exit 2 if any unit needs human review")
	fs.StringVar(&o.fixtures, "fixtures", "testdata/eval", "eval: directory of NAME.json cases")
	fs.BoolVar(&o.judge, "judge", false, "eval: score summary faithfulness with OpenJev")
	fs.StringVar(&o.addr, "addr", defaultAddr, "serve: listen address; the default port falls back to an available one when taken (port 0 always picks one)")
	fs.StringVar(&o.cache, "cache", cacheRoot, "serve/prs/-pr: repo clones and cached results")
	fs.StringVar(&o.repo, "repo", "", "prs: owner/repo, or host/owner/repo for GitHub Enterprise")
	fs.StringVar(&o.author, "author", "@me", "prs: PR author")
	fs.StringVar(&o.state, "state", "all", "prs: open|closed|merged|all")
	fs.IntVar(&o.limit, "limit", 20, "prs: max PRs")
	fs.BoolVar(&o.reviewDryRun, "review-dry-run", false, "serve: return the review payload instead of posting it to GitHub")
	fs.BoolVar(&o.force, "force", false, "prs/-pr: re-run PRs that already have a cached result")

	// The subcommand may come before or after the flags.
	args, sub := os.Args[1:], ""
	if len(args) > 0 && subcommands[args[0]] {
		sub, args = args[0], args[1:]
	}
	_ = fs.Parse(args)
	if sub == "" && subcommands[fs.Arg(0)] {
		sub = fs.Arg(0)
		_ = fs.Parse(fs.Args()[1:])
	}

	fs.Visit(func(f *flag.Flag) {
		if f.Name == "group-review" {
			o.groupReviewSet = true
		}
		if f.Name == "lint" {
			o.lintSet = true
		}
	})

	if sub == "" && len(os.Args) == 1 && desktopBuild {
		sub = "desktop"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = nil
	switch sub {
	case "eval":
		err = runEval(ctx, o)
	case "serve":
		err = runServe(ctx, o)
	case "desktop":
		err = runDesktop(ctx, o)
	case "prs":
		err = runPRs(ctx, o)
	case "index":
		err = runIndex(ctx, o)
	default:
		err = runTriage(ctx, o)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pr-manager:", err)
		if err == errHuman {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

var errHuman = fmt.Errorf("units need human review")

// runIndex builds the code map from -code-root and -org.
func runIndex(ctx context.Context, o options) error {
	last := ""
	res, err := indexSources(ctx, o, func(stage string, done, total int) {
		if stage != last {
			fmt.Fprintf(os.Stderr, "%s...\n", stage)
			last = stage
		}
	})
	if err != nil {
		return err
	}
	fmt.Printf("linked %d, cloned %d, updated %d, failed %d; %d repos in the map at %s\n",
		len(res.Linked), len(res.Cloned), len(res.Updated), len(res.Failed), res.Repos, o.codemapDir)
	for _, f := range res.Failed {
		fmt.Println("  failed:", f)
	}
	return nil
}

func runTriage(ctx context.Context, o options) error {
	o = resolveProviders(o)
	var report *triage.Report
	if o.pr != "" {
		ref, err := triage.ParsePRRef(o.pr)
		if err != nil {
			return err
		}
		t, err := newTriager(o)
		if err != nil {
			return err
		}
		r, err := t.Run(ctx, ref, jobOptions{Force: o.force}, func(string, int, int) {})
		if err != nil {
			return err
		}
		report = &triage.Report{Base: r.PR.BaseOid[:10], Head: r.PR.HeadOid[:10]}
		for _, f := range r.Files {
			for _, u := range f.Units {
				report.Units = append(report.Units, u.Unit)
			}
		}
		if err := triage.Rebucket(report.Units, r.tierPolicy(), o.reviewBudget); err != nil {
			return err
		}
		if !triage.IsEnglish(o.summaryLang) {
			tr, err := t.translation(ctx, r, jobOptions{})
			applyTranslation(o, report.Units, tr, err)
		}
	} else {
		var src *triage.Source
		var err error
		if o.diffFile != "" {
			var raw string
			if raw, err = readDiff(o.diffFile); err != nil {
				return err
			}
			// Head file contents come from the working tree in -C.
			src, err = triage.FromDiff(raw, o.dir, "")
		} else {
			src, err = triage.FromGit(o.dir, o.base, o.head)
		}
		if err != nil {
			return err
		}
		policy, gitattrs, err := dirConfig(o)
		if err != nil {
			return err
		}
		pipe, err := buildPipeline(o, policy, gitattrs)
		if err != nil {
			return err
		}
		if m := loadCodeMap(o.codemapDir); m != nil {
			pipe.CodeMap = &triage.CodeMap{Map: m, Repo: localRepoName(o, m)}
		}
		report = &triage.Report{Base: o.base, Head: o.head, Units: pipe.Run(ctx, src)}
		if !triage.IsEnglish(o.summaryLang) {
			tr, err := translateUnits(ctx, o, report.Units)
			applyTranslation(o, report.Units, tr, err)
		}
	}

	w := io.Writer(os.Stdout)
	if o.outFile != "" {
		f, err := os.Create(o.outFile)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	if o.out == "json" {
		if err := triage.RenderJSON(w, report); err != nil {
			return err
		}
	} else {
		triage.RenderMarkdown(w, report)
	}
	if o.failOnHuman && report.Counts()[triage.BucketHuman] > 0 {
		return errHuman
	}
	return nil
}

func runEval(ctx context.Context, o options) error {
	o = resolveProviders(o)
	policy, gitattrs, err := dirConfig(o)
	if err != nil {
		return err
	}
	pipe, err := buildPipeline(o, policy, gitattrs)
	if err != nil {
		return err
	}
	// Eval measures the model, not the cache.
	pipe.Decisions = nil
	cases, err := triage.LoadEvalCases(o.fixtures)
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		return fmt.Errorf("no *.json cases in %s", o.fixtures)
	}
	var judge *triage.Judge
	if o.judge {
		judge = &triage.Judge{Jev: &llm.OpenJev{BaseURL: o.openjevURL}}
	}
	res, err := triage.RunEval(ctx, pipe, cases, judge)
	if err != nil {
		return err
	}
	res.Print(os.Stdout)
	return nil
}

// Default models per provider, best first: a small fast model classifies,
// a stronger one summarizes and reviews. A provider that lists its models
// gets the first one it has, else the newest of the same family (see
// llm.PickModel), so a gateway or an older account without the newest
// model still gets a default that runs.
var (
	classifyDefaults = map[string][]string{
		"codex": {llm.CodexSmall, "gpt-5.6-luna"}, "claude-code": {llm.ClaudeCodeSmall},
		llm.ClaudeAPI: {llm.AnthropicClaudeHaiku45}, llm.OpenAIAPI: {llm.OpenAIGPT54Mini, llm.OpenAIGPT54Nano}, "ollama": {llm.OllamaQwen35_9B},
		llm.Bedrock: {llm.BedrockHaiku45}, llm.Vertex: {llm.VertexHaiku45}, llm.Foundry: {llm.FoundryHaiku45},
		llm.AzureOpenAI: {orDefault(os.Getenv("AZURE_OPENAI_CLASSIFY_DEPLOYMENT"), llm.OpenAIGPT54Mini)},
	}
	summaryDefaults = map[string][]string{
		"codex": {llm.CodexLarge, llm.OpenAIGPT56Sol}, "claude-code": {llm.ClaudeCodeLarge},
		llm.ClaudeAPI: {llm.AnthropicClaudeSonnet5}, llm.OpenAIAPI: {llm.OpenAIGPT6Sol, llm.OpenAIGPT56Sol, llm.OpenAIGPT54}, "ollama": {llm.OllamaQwen35_9B},
		llm.Bedrock: {llm.BedrockSonnet5}, llm.Vertex: {llm.VertexSonnet5}, llm.Foundry: {llm.FoundrySonnet5},
		llm.AzureOpenAI: {orDefault(os.Getenv("AZURE_OPENAI_REVIEW_DEPLOYMENT"), llm.OpenAIGPT6Sol)},
	}
	// Translation: the fastest that translates well in a benchmark on
	// cached PRs, Sonnet over Haiku on Claude (Haiku was the slowest through
	// the CLI); elsewhere the classifier's small model.
	translateDefaults = map[string][]string{
		"codex": {llm.CodexTranslate, llm.CodexSmall}, "claude-code": {llm.ClaudeCodeTranslate, "claude-sonnet-5"},
		llm.ClaudeAPI: {llm.ClaudeCodeTranslate, llm.AnthropicClaudeSonnet5},
		llm.Bedrock:   {llm.BedrockSonnet5}, llm.Vertex: {llm.VertexSonnet5}, llm.Foundry: {llm.FoundrySonnet5},
	}
	translateEfforts = map[string]string{"codex": "minimal", "claude-code": "low", llm.OpenAIAPI: "minimal", llm.AzureOpenAI: "minimal"}
)

// modelCatalog is what a provider can run, for picking its default model.
var modelCatalog = func(provider string) llm.Catalog { // tests replace it
	return llm.ProviderCatalog(context.Background(), provider)
}

// defaultModel is provider's default for a role: the first of prefs, its
// defaults for the role, that it can run.
func defaultModel(provider string, prefs []string) string {
	if len(prefs) == 0 {
		return ""
	}
	return llm.PickModel(modelCatalog(provider), prefs...)
}

// translatePrefs is provider's translation defaults, then its classifier's.
func translatePrefs(provider string) []string {
	return append(slices.Clone(translateDefaults[provider]), classifyDefaults[provider]...)
}

// resolveProviders replaces "auto" with the best provider that has
// credentials and fills per-provider default models. A coding-agent
// subscription on this machine comes first (Codex, then Claude Code), then
// a cloud account the machine is explicitly set up for (Bedrock, Vertex AI,
// Foundry, Azure OpenAI; see llm.ConfiguredCloud), then an API key (OpenAI,
// then Anthropic), then local Ollama.
func resolveProviders(o options) options {
	auto := "ollama"
	switch cloud := llm.ConfiguredCloud(); {
	case llm.HasSubscription("codex"):
		auto = "codex"
	case llm.HasSubscription("claude-code"):
		auto = "claude-code"
	case cloud != "":
		auto = cloud
	case os.Getenv("OPENAI_API_KEY") != "":
		auto = llm.OpenAIAPI
	case os.Getenv("ANTHROPIC_API_KEY") != "":
		auto = llm.ClaudeAPI
	}
	pick := func(p string) string {
		if p == "auto" || p == "" {
			return auto
		}
		return llm.ProviderID(p)
	}
	o.classifier, o.fallback, o.summarizer = pick(o.classifier), pick(o.fallback), pick(o.summarizer)
	if o.classifyModel == "" {
		o.classifyModel = defaultModel(o.classifier, classifyDefaults[o.classifier])
	}
	if o.fallbackModel == "" {
		o.fallbackModel = defaultModel(o.fallback, classifyDefaults[o.fallback])
	}
	if o.summaryModel == "" {
		o.summaryModel = defaultModel(o.summarizer, summaryDefaults[o.summarizer])
	}
	// Translation follows the reviewer, else the classifier: whichever
	// provider the run already uses. OpenJev can't translate.
	if o.translator == "auto" || o.translator == "" {
		o.translator = ""
		for _, p := range []string{o.summarizer, o.classifier, o.fallback} {
			if p != "off" && p != "openjev" {
				o.translator = p
				break
			}
		}
	} else {
		o.translator = llm.ProviderID(o.translator)
	}
	if o.translateModel == "" {
		o.translateModel = defaultModel(o.translator, translatePrefs(o.translator))
	}
	if o.translateEffort == "auto" {
		o.translateEffort = orDefault(translateEfforts[o.translator], "low")
	}
	return o
}

// dirConfig loads .triage.yaml and .gitattributes from the -C checkout.
func dirConfig(o options) (triage.Policy, []string, error) {
	policyFile := o.policy
	if policyFile == "" {
		policyFile = filepath.Join(o.dir, ".triage.yaml")
	}
	policy, err := triage.LoadPolicy(policyFile)
	if err != nil {
		return policy, nil, fmt.Errorf("policy %s: %w", policyFile, err)
	}
	return policy, triage.LoadGitattributesGenerated(filepath.Join(o.dir, ".gitattributes")), nil
}

// sourceConfig loads .triage.yaml and .gitattributes. A remote PR's head is
// the change under review and could exempt itself, so by default they come
// from the merge base and the head's .triage.yaml can only add force_human
// (see triage.ParsePRPolicy). trustHead is for the user's own checkout: its
// head config applies as is, and a head .triage.yaml that doesn't parse is
// an error.
func sourceConfig(src *triage.Source, trustHead bool) (triage.Policy, []string, error) {
	read := func(get triage.ContentFunc, file string) []byte {
		if get == nil {
			return nil
		}
		b, err := get(file)
		if err != nil {
			return nil
		}
		return b
	}
	attrsFrom := src.BaseContent
	var policy triage.Policy
	var err error
	if trustHead {
		attrsFrom = src.Content
		policy = triage.DefaultPolicy()
		if b := read(src.Content, ".triage.yaml"); len(b) > 0 {
			policy, err = triage.ParsePolicy(b)
		}
	} else {
		policy, err = triage.ParsePRPolicy(read(src.BaseContent, ".triage.yaml"), read(src.Content, ".triage.yaml"))
	}
	if err != nil {
		return policy, nil, fmt.Errorf(".triage.yaml: %w", err)
	}
	var gitattrs []string
	if b := read(attrsFrom, ".gitattributes"); b != nil {
		gitattrs = triage.ParseGitattributesGenerated(b)
	}
	return policy, gitattrs, nil
}

// buildPipeline expects resolved providers (see resolveProviders).
func buildPipeline(o options, policy triage.Policy, gitattrs []string) (*triage.Pipeline, error) {
	if o.reviewBudget != "" {
		if _, _, err := policy.Tiers.Budget(o.reviewBudget); err != nil {
			return nil, err
		}
		policy.Tiers.ReviewBudget = o.reviewBudget
	}
	if o.groupReviewSet {
		policy.Grouping.Enabled = o.groupReview
	}
	if o.lintSet {
		if err := policy.Lint.Set(o.lint); err != nil {
			return nil, err
		}
	}
	if o.classifyBatch > 0 {
		policy.ClassifyBatch.MaxUnits = o.classifyBatch
	}
	pipe := &triage.Pipeline{
		Presorter:         &triage.Presorter{Policy: policy, GitattributesGenerated: gitattrs},
		Lint:              &triage.Linter{Policy: policy.Lint},
		Concurrency:       o.concurrency,
		ReviewConcurrency: o.reviewConcurrency,
	}
	// classifyKey names the model that decides, for the decision store.
	classifyKey := ""
	llmClassifier := func(provider, model string) (triage.Classifier, error) {
		if provider == "off" {
			return nil, nil
		}
		l, err := llm.New(provider, model)
		if err != nil {
			return nil, err
		}
		llm.SetEffort(l, o.classifyEffort)
		classifyKey = l.Name() + "/" + l.ModelID() + "@" + o.classifyEffort
		return &triage.LLMClassifier{LLM: l, Policy: policy}, nil
	}
	var err error
	// With a reviewer, the review call places every unit it reads, so a
	// separate classifier would only pay for a second, weaker opinion.
	switch {
	case o.summarizer != "off":
	case o.classifier == "openjev":
		fb, err := llmClassifier(o.fallback, o.fallbackModel)
		if err != nil {
			return nil, err
		}
		pipe.Classifier = &triage.JevClassifier{
			Jev:      &llm.OpenJev{BaseURL: o.openjevURL},
			Policy:   policy,
			Fallback: fb,
			Accept:   triage.DefaultJevAccept(),
		}
		classifyKey = "openjev " + o.openjevURL + " → " + classifyKey
	default:
		if pipe.Classifier, err = llmClassifier(o.classifier, o.classifyModel); err != nil {
			return nil, err
		}
	}
	if pipe.Classifier != nil && o.classifyCache && o.cache != "" {
		pipe.Decisions = decisionDir(filepath.Join(o.cache, "decisions"))
		pipe.ClassifyKey = fmt.Sprint(classifyKey, " ", policy.Thresholds, " ", policy.MaxUnitChars)
	}
	if o.summarizer != "off" {
		l, err := llm.New(o.summarizer, o.summaryModel)
		if err != nil {
			return nil, err
		}
		llm.SetEffort(l, o.reviewEffort)
		critic, err := llm.New(o.summarizer, o.summaryModel)
		if err != nil {
			return nil, err
		}
		llm.SetEffort(critic, o.reviewEffort)
		pipe.Summarizer = &triage.Summarizer{LLM: l, Critic: critic, Policy: policy, Tools: o.reviewTools}
	}
	pipe.Warn = func(msg string) { fmt.Fprintln(os.Stderr, "pr-manager:", msg) }
	return pipe, nil
}

var (
	codeMapMu     sync.Mutex
	codeMapLoaded bool
	codeMap       *codemap.Map
)

// loadCodeMap opens the code map once per process (again after
// reloadCodeMap). A missing map only disables impact and file history; triage still
// works.
func loadCodeMap(dir string) *codemap.Map {
	codeMapMu.Lock()
	defer codeMapMu.Unlock()
	if !codeMapLoaded {
		codeMap, codeMapLoaded = openCodeMap(dir), true
	}
	return codeMap
}

// reloadCodeMap drops the open map so the next load reads a rebuilt one.
func reloadCodeMap(dir string) *codemap.Map {
	codeMapMu.Lock()
	codeMapLoaded = false
	codeMapMu.Unlock()
	return loadCodeMap(dir)
}

func openCodeMap(dir string) *codemap.Map {
	if dir == "" || dir == "off" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil && !filepath.IsAbs(dir) {
		// Relative to the binary, so a pr-manager run from elsewhere finds it.
		if exe, err := os.Executable(); err == nil {
			if alt := filepath.Join(filepath.Dir(exe), dir); fileExists(filepath.Join(alt, "meta.json")) {
				dir = alt
			}
		}
	}
	m, err := codemap.Open(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pr-manager: code map disabled: %v\n", err)
		return nil
	}
	return m
}

// codeMapSettings is the code map's part of the settings hash (see
// settingsHash): whether there is a map, and its format. Not its build
// time: the map is rebuilt whenever a new repo is first triaged or Index
// is pressed, and a hash that moved with it would stop every earlier run
// of every repo from being carried over (incremental.go). Nothing carried
// depends on the map anyway; impact and likelihood are scored again on
// every run from the map as it is then.
func codeMapSettings(m *codemap.Map) string {
	if m == nil {
		return "nomap"
	}
	return fmt.Sprintf("map%d", m.Meta.Version)
}

// codeMapVersion is when the map was built: shown with a result, and a
// key for what is derived from one build (treemaps).
func codeMapVersion(m *codemap.Map) string {
	if m == nil {
		return "nomap"
	}
	return m.Meta.GeneratedAt
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// localRepoName is the map repo for -C runs: -map-repo, else the map's
// repo with the checkout's origin remote, else its directory name.
func localRepoName(o options, m *codemap.Map) string {
	if o.mapRepo != "" {
		return o.mapRepo
	}
	top, err := triage.Git(o.dir, "rev-parse", "--show-toplevel")
	if err != nil {
		abs, _ := filepath.Abs(o.dir)
		return filepath.Base(abs)
	}
	top = strings.TrimSpace(top)
	if ref := localRepoRef(top); ref.Owner != "local" {
		return codeMapRepo(m, ref)
	}
	return filepath.Base(top)
}

func readDiff(path string) (string, error) {
	var b []byte
	var err error
	if path == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}
