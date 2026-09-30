// Package indexer builds and queries a code map: impact
// (how much a bad change can break) and likelihood (how often code breaks).
//
//	codemap build  [-repo a,b] [-force]   extract stale repos, rank, write the map
//	codemap rank                          re-rank from cached graphs (after rule edits)
//	codemap lookup <repo/path[:sym|:L1-L2]>...
//	codemap lookup -repo R -diff FILE|-   assess every hunk of a unified diff
//	codemap top    [-level file] [-repo R] [-n 20] [-by impact|likelihood|rank|rollback|fixes]
package indexer

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/codemap"
	"github.com/amitbet/pr-manager/internal/appdirs"
)

// Run executes the code-map command using the same implementation as pr-manager.
func Run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: pr-manager codemap build|rank|lookup|top [flags]")
	}
	switch args[0] {
	case "build":
		return cmdBuild(args[1:], true)
	case "rank":
		return cmdBuild(args[1:], false)
	case "lookup":
		return cmdLookup(args[1:])
	case "top":
		return cmdTop(args[1:])
	default:
		return fmt.Errorf("unknown codemap command %q", args[0])
	}
}

// workspaceRoot finds a workspace containing code/ and optional repos.yaml:
// -workspace, then $PR_MANAGER_WORKSPACE, then the
// working directory and its parents.
func workspaceRoot(flagVal string) (string, error) {
	isWS := func(d string) bool {
		st, err := os.Stat(filepath.Join(d, "code"))
		return err == nil && st.IsDir()
	}
	if flagVal != "" {
		if !isWS(flagVal) {
			return "", fmt.Errorf("workspace %s must contain a code directory", flagVal)
		}
		return filepath.Abs(flagVal)
	}
	for _, d := range []string{os.Getenv("PR_MANAGER_WORKSPACE")} {
		if d != "" && isWS(d) {
			return filepath.Abs(d)
		}
	}
	d, _ := os.Getwd()
	for {
		if isWS(d) {
			return d, nil
		}
		p := filepath.Dir(d)
		if p == d {
			return "", fmt.Errorf("no workspace found (use -C for one checkout, -workspace or PR_MANAGER_WORKSPACE)")
		}
		d = p
	}
}

func cmdBuild(args []string, extract bool) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	repoFlag := fs.String("repo", "", "comma-separated repos to re-extract even if cached")
	force := fs.Bool("force", false, "re-extract every repo")
	parallel := fs.Int("parallel", max(1, runtime.NumCPU()/3), "repos extracted concurrently")
	cfgPath := fs.String("config", "", "config file (default embedded rules); output and cache paths in it are relative to its directory")
	wsFlag := fs.String("workspace", "", "workspace directory containing code/<repo> (default $PR_MANAGER_WORKSPACE)")
	checkout := fs.String("C", "", "index a single git checkout instead of a workspace")
	outFlag := fs.String("output", "", "override map output directory")
	cacheFlag := fs.String("cache", "", "override graph cache directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *parallel < 1 {
		return fmt.Errorf("parallel must be at least 1")
	}
	if *checkout != "" && *wsFlag != "" {
		return fmt.Errorf("use either -C or -workspace")
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	cfgDir := filepath.Dir(*cfgPath)
	if *cfgPath == "" {
		cfgDir, err = appdirs.CacheDir()
		if err != nil {
			return err
		}
	}
	cacheDir := filepath.Join(cfgDir, cfg.Cache)
	outDir := filepath.Join(cfgDir, cfg.Output)
	if *cacheFlag != "" {
		cacheDir = *cacheFlag
	}
	if *outFlag != "" {
		outDir = *outFlag
	}
	var repos []*RepoInfo
	if *checkout != "" {
		root, e := filepath.Abs(*checkout)
		if e != nil {
			return e
		}
		repos = []*RepoInfo{{Name: filepath.Base(root), Root: root, Category: "unlisted"}}
	} else {
		ws, e := workspaceRoot(*wsFlag)
		if e != nil {
			return e
		}
		repos, err = discoverRepos(ws)
		if err != nil {
			return err
		}
	}
	forceSet := map[string]bool{}
	for _, r := range strings.Split(*repoFlag, ",") {
		if r = strings.TrimSpace(r); r != "" {
			forceSet[r] = true
		}
	}
	t0 := time.Now()
	var graphs map[string]*Graph
	if extract {
		graphs, err = runExtraction(repos, cacheDir, forceSet, *force, *parallel)
		if err != nil {
			return err
		}
	} else {
		graphs = map[string]*Graph{}
		for _, ri := range repos {
			g, err := readGraph(cachePath(cacheDir, ri.Name))
			if err != nil {
				return fmt.Errorf("%s: no cached graph, run build first", ri.Name)
			}
			graphs[ri.Name] = g
		}
	}
	t1 := time.Now()
	w := buildWorld(cfg, repos, graphs)

	e := &emitter{w: w, cfg: cfg}
	e.syms = identityLevel(w)
	w.rankLevel(e.syms)
	e.files = w.collapse("file", func(n *Node) (string, string) {
		if n.File == "" {
			return "", ""
		}
		return n.Repo + "/" + n.File, n.Repo
	})
	w.rankLevel(e.files)
	e.dirs = w.collapse("dir", func(n *Node) (string, string) { return n.Repo + "/" + n.Dir, n.Repo })
	w.rankLevel(e.dirs)
	e.repos = w.collapse("repo", func(n *Node) (string, string) { return n.Repo, n.Repo })
	w.rankLevel(e.repos)
	e.rb = w.symbolRollback()
	e.rev = newReverser(w, e.files)
	e.run()
	if err := writeOutput(outDir, e, cfg, w.stats); err != nil {
		return err
	}
	for _, name := range sortedKeys(graphs) {
		for _, warn := range graphs[name].Warnings {
			fmt.Fprintf(os.Stderr, "warn: %s: %s\n", name, warn)
		}
	}
	st, _ := json.Marshal(w.stats)
	fmt.Fprintf(os.Stderr, "stats: %s\n", st)
	fmt.Fprintf(os.Stderr, "done: extract %s, rank+emit %s -> %s\n", t1.Sub(t0).Round(time.Second), time.Since(t1).Round(time.Second), outDir)
	return nil
}

// identityLevel ranks individual nodes. Internal nodes (interface methods,
// module bodies, stubs) carry rank but are not part of the percentile
// population.
func identityLevel(w *world) *level {
	lv := &level{name: "symbol", gidx: map[string]int{}, member: make([]int, len(w.nodes))}
	lv.groups = make([]string, len(w.nodes))
	lv.real = make([]bool, len(w.nodes))
	lv.repo = make([]string, len(w.nodes))
	for i, n := range w.nodes {
		lv.member[i] = i
		lv.groups[i] = n.Key
		lv.gidx[n.Key] = i
		_, virt := w.virtual[i]
		lv.real[i] = !virt && !n.Internal
		lv.repo[i] = n.Repo
	}
	lv.out = w.out
	return lv
}

func defaultMapDir() (string, error) {
	root, err := appdirs.CacheDir()
	return filepath.Join(root, "codemap"), err
}

func cmdLookup(args []string) error {
	fs := flag.NewFlagSet("lookup", flag.ContinueOnError)
	mapDir := fs.String("map", "", "codemap directory")
	repo := fs.String("repo", "", "repo for -diff (paths in the diff are repo-relative)")
	diff := fs.String("diff", "", "unified diff file, or - for stdin")
	full := fs.Bool("full", false, "include the whole fallback chain")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *mapDir == "" {
		var err error
		*mapDir, err = defaultMapDir()
		if err != nil {
			return err
		}
	}
	m, err := codemap.Open(*mapDir)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if *diff != "" {
		var r io.Reader = os.Stdin
		if *diff != "-" {
			f, err := os.Open(*diff)
			if err != nil {
				return err
			}
			defer f.Close()
			r = f
		}
		hunks, err := codemap.ParseDiff(r)
		if err != nil {
			return err
		}
		rep := m.LookupDiff(*repo, hunks)
		if !*full {
			for i := range rep.Hunks {
				trim(&rep.Hunks[i].Result)
			}
		}
		return enc.Encode(rep)
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("lookup needs targets or -diff")
	}
	for _, t := range fs.Args() {
		res := m.Lookup(codemap.ParseTarget(t))
		if !*full {
			trim(&res)
		}
		if err := enc.Encode(res); err != nil {
			return err
		}
	}
	return nil
}

// trim keeps the chain short for humans: the first two ancestors.
func trim(r *codemap.Result) {
	if len(r.Chain) > 2 {
		r.Chain = append(r.Chain[:1], r.Chain[len(r.Chain)-1])
	}
}

func cmdTop(args []string) error {
	fs := flag.NewFlagSet("top", flag.ContinueOnError)
	mapDir := fs.String("map", "", "codemap directory")
	lvl := fs.String("level", "file", "repo | dir | file | symbol")
	repo := fs.String("repo", "", "limit to one repo")
	n := fs.Int("n", 20, "rows")
	by := fs.String("by", "impact", "impact | likelihood | rank | rollback | fixes | deps")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *mapDir == "" {
		var err error
		*mapDir, err = defaultMapDir()
		if err != nil {
			return err
		}
	}
	m, err := codemap.Open(*mapDir)
	if err != nil {
		return err
	}
	var recs []codemap.Record
	if *lvl == "repo" {
		for _, r := range m.Repos {
			recs = append(recs, r)
		}
	} else {
		var repos []string
		if *repo != "" {
			repos = []string{*repo}
		} else {
			for r := range m.Meta.Repos {
				repos = append(repos, r)
			}
		}
		for _, r := range repos {
			all, err := codemap.ReadShard(*mapDir, r)
			if err != nil {
				return err
			}
			for _, rec := range all {
				if rec.Level == *lvl {
					recs = append(recs, *rec)
				}
			}
		}
	}
	key := func(r codemap.Record) float64 {
		switch *by {
		case "rank":
			return r.PR
		case "rollback":
			return float64(r.Rollback)*1000 + r.Rank
		case "deps":
			return float64(len(r.DepRepos))*1e6 + float64(r.DepFiles)
		case "likelihood":
			return float64(r.Likelihood)*1000 + float64(r.Impact)
		case "fixes":
			if r.Hist == nil {
				return 0
			}
			return r.Hist.RecentFixes*1000 + float64(r.Hist.Fixes)
		}
		return float64(r.Impact)*1000 + r.Rank
	}
	sort.Slice(recs, func(i, j int) bool {
		if ki, kj := key(recs[i]), key(recs[j]); ki != kj {
			return ki > kj
		}
		return recs[i].ID < recs[j].ID
	})
	fmt.Printf("%-7s %-6s %-5s %-8s %-6s %-6s %-9s %-6s %s\n", "impact", "likely", "rank", "rollback", "calls", "deps", "dep_repos", "fixes", "id")
	for i, r := range recs {
		if i >= *n {
			break
		}
		tags := ""
		if len(r.RollbackTags) > 0 {
			tags = " [" + r.RollbackTags[0].ID + "]"
		}
		fixes := "-"
		if r.Hist != nil {
			fixes = fmt.Sprint(r.Hist.Fixes)
		}
		fmt.Printf("%-7d %-6d %-5.1f %-8d %-6d %-6d %-9d %-6s %s%s\n", r.Impact, r.Likelihood, r.Rank, r.Rollback, r.Callers, r.DepFiles, len(r.DepRepos), fixes, r.ID, tags)
	}
	return nil
}
