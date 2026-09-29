package indexer

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/amitbet/pr-manager/codemap"
	"github.com/amitbet/pr-manager/codemap/githist"
	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/internal/sitter"
)

type RepoInfo struct {
	Name     string
	Category string
	Role     string
	Root     string // absolute, symlinks resolved
	Tracked  []string
	Modules  []Module
}

type reposFile struct {
	Repositories []struct {
		Name     string `yaml:"name"`
		Category string `yaml:"category"`
		Role     string `yaml:"role"`
	} `yaml:"repositories"`
}

// discoverRepos lists repos from repos.yaml that are present under code/,
// plus any extra checkouts in code/ (category "unlisted").
func discoverRepos(ws string) ([]*RepoInfo, error) {
	var rf reposFile
	if b, err := os.ReadFile(filepath.Join(ws, "repos.yaml")); err == nil {
		if err := yaml.Unmarshal(b, &rf); err != nil {
			return nil, fmt.Errorf("repos.yaml: %w", err)
		}
	}
	known := map[string]*RepoInfo{}
	for _, r := range rf.Repositories {
		known[r.Name] = &RepoInfo{Name: r.Name, Category: r.Category, Role: r.Role}
	}
	entries, err := os.ReadDir(filepath.Join(ws, "code"))
	if err != nil {
		return nil, err
	}
	var out []*RepoInfo
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		root, err := codemap.ResolveLink(filepath.Join(ws, "code", name))
		if err != nil {
			continue
		}
		if st, err := os.Stat(filepath.Join(root, ".git")); err != nil || st == nil {
			continue
		}
		ri := known[name]
		if ri == nil {
			ri = &RepoInfo{Name: name, Category: "unlisted"}
		}
		ri.Root = root
		out = append(out, ri)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := proc.Command("git", append([]string{"-C", dir}, args...)...)
	b, err := cmd.Output()
	return string(b), err
}

// repoRemote is the identity of the checkout's origin remote
// (codemap.RemoteID), "" without one.
func repoRemote(root string) string {
	if root == "" {
		return ""
	}
	out, err := gitOut(root, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return codemap.RemoteID(strings.TrimSpace(out))
}

func (ri *RepoInfo) loadFiles() error {
	out, err := gitOut(ri.Root, "ls-files", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || seen[l] {
			continue
		}
		if _, err := os.Lstat(filepath.Join(ri.Root, l)); err != nil {
			continue // deleted in worktree
		}
		seen[l] = true
		ri.Tracked = append(ri.Tracked, l)
	}
	sort.Strings(ri.Tracked)
	for _, p := range ri.Tracked {
		if path.Base(p) != "go.mod" || strings.Contains(p, "vendor/") || strings.Contains(p, "testdata/") || strings.Contains(p, "node_modules/") {
			continue
		}
		f, err := os.Open(filepath.Join(ri.Root, p))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "module ") {
				mp := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module")), `"`)
				d := path.Dir(p)
				if d == "." {
					d = ""
				}
				ri.Modules = append(ri.Modules, Module{Path: mp, Dir: d})
				break
			}
		}
		f.Close()
	}
	return nil
}

// fingerprint changes whenever HEAD, the working tree, or the extractor
// changes. Unchanged repos reuse their cached graph.
func (ri *RepoInfo) fingerprint(allMods []Module) (commit string, dirty bool, fp string) {
	head, _ := gitOut(ri.Root, "rev-parse", "HEAD")
	commit = strings.TrimSpace(head)
	status, _ := gitOut(ri.Root, "status", "--porcelain", "--untracked-files=all")
	h := sha256.New()
	fmt.Fprintf(h, "v%d %s %s\n%s\n%s\n", extractorVersion, runtime.Version(), depsHash(), commit, status)
	if strings.TrimSpace(status) != "" {
		dirty = true
		diff, _ := gitOut(ri.Root, "diff", "HEAD")
		h.Write([]byte(diff))
		for _, l := range strings.Split(status, "\n") {
			if strings.HasPrefix(l, "?? ") {
				if b, err := os.ReadFile(filepath.Join(ri.Root, strings.TrimSpace(l[3:]))); err == nil {
					h.Write(b)
				}
			}
		}
	}
	for _, m := range allMods {
		fmt.Fprintf(h, "%s\n", m.Path)
	}
	return commit, dirty, hex.EncodeToString(h.Sum(nil))[:16]
}

// depsHash ties cached graphs and parse caches to the versions of the
// modules the indexer is built with and the selected parser backend, so a
// dependency upgrade re-extracts. Rebuilding the indexer does not: bump
// extractorVersion when an extractor's or parser's output changes.
var depsHash = sync.OnceValue(func() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	h := sha256.New()
	fmt.Fprintln(h, sitter.Backend)
	for _, d := range bi.Deps {
		if d.Replace != nil {
			d = d.Replace
		}
		fmt.Fprintf(h, "%s@%s %s\n", d.Path, d.Version, d.Sum)
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
})

func cachePath(cacheDir, repo string) string {
	return filepath.Join(cacheDir, "graphs", repo+".json.gz")
}

func readGraph(p string) (*Graph, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	var g Graph
	if err := json.NewDecoder(zr).Decode(&g); err != nil {
		return nil, err
	}
	return &g, nil
}

func writeGraph(p string, g *Graph) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	if err := json.NewEncoder(zw).Encode(g); err != nil {
		f.Close()
		return err
	}
	zw.Close()
	f.Close()
	return os.Rename(tmp, p)
}

// extractRepo runs every extractor that applies to the repo.
//
// Re-extraction is incremental where the result can't differ from a full
// run: files whose path and content are unchanged reuse their parse from
// cacheDir (Java, Python, C#, Rust), and history appends the commits since
// prev's HEAD. Name resolution, edges and Go type-checking always cover the
// whole repo. cacheDir "" or full disables both.
func extractRepo(ri *RepoInfo, allMods []Module, commit string, dirty bool, fp string, prev *Graph, cacheDir string, full bool) (*Graph, *parseCache) {
	g := &Graph{Version: extractorVersion, Repo: ri.Name, Commit: commit, Dirty: dirty, Fingerprint: fp, Modules: ri.Modules}
	var pc *parseCache
	if cacheDir != "" {
		pc = &parseCache{dir: filepath.Join(cacheDir, "parse", ri.Name), fresh: full}
		parseCaches.Store(ri.Root, pc)
		defer parseCaches.Delete(ri.Root)
	}
	if full {
		prev = nil
	}
	// The extractors are independent: each fills its own graph, and the
	// parts are joined in this order so the result does not depend on
	// which finishes first.
	extractors := []func(repo, root string, tracked []string, g *Graph){
		func(repo, root string, _ []string, g *Graph) {
			if len(ri.Modules) > 0 {
				extractGo(repo, root, ri.Modules, allMods, g)
			}
		},
		extractTS, extractJava, extractPy, extractCS, extractRust, extractGenAll, extractSpecs,
	}
	parts := make([]*Graph, len(extractors))
	var wg sync.WaitGroup
	for i, extract := range extractors {
		parts[i] = &Graph{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			extract(ri.Name, ri.Root, ri.Tracked, parts[i])
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		loadHistory(ri.Root, g, prev)
	}()
	wg.Wait()
	for _, part := range parts {
		g.Nodes = append(g.Nodes, part.Nodes...)
		g.Edges = append(g.Edges, part.Edges...)
		g.Files = append(g.Files, part.Files...)
		g.Specs = append(g.Specs, part.Specs...)
		g.URLRefs = append(g.URLRefs, part.URLRefs...)
		g.Warnings = append(g.Warnings, part.Warnings...)
	}
	// Every other tracked file is recorded so path rules and fallback lookups
	// can see it (migrations, helm charts, CRDs, SQL, CI).
	have := map[string]bool{}
	for _, f := range g.Files {
		have[f.Path] = true
	}
	for _, p := range ri.Tracked {
		if have[p] || !isIndexableAux(p) {
			continue
		}
		lang := strings.TrimPrefix(path.Ext(p), ".")
		if lang == "" {
			lang = path.Base(p)
		}
		lines := 0
		if b, err := os.ReadFile(filepath.Join(ri.Root, p)); err == nil {
			lines = strings.Count(string(b), "\n") + 1
		}
		g.Files = append(g.Files, FileInfo{Path: p, Lang: lang, Lines: lines})
	}
	sort.Slice(g.Files, func(i, j int) bool { return g.Files[i].Path < g.Files[j].Path })
	return g, pc
}

// historyDays is how far back extraction reads git history. The scoring
// window (history.days in the scoring config) can be anything up to this without
// re-extracting.
const historyDays = 730

// loadHistory records HEAD's commits of the last historyDays. A repo whose
// history can't be read (shallow clone, no git) gets none and scores on
// complexity alone.
//
// When prev was extracted at an ancestor of HEAD, only the commits since
// then are read and prev's are kept, trimmed to the window: the same list a
// full read returns. After a rebase or reset it reads everything.
func loadHistory(root string, g *Graph, prev *Graph) {
	head, err := githist.HeadTime(root, "HEAD")
	if err != nil {
		g.Warnings = append(g.Warnings, "history: "+err.Error())
		return
	}
	since := head.AddDate(0, 0, -historyDays)
	if prev != nil && prev.Commit != "" && !prev.HeadTime.IsZero() && githist.IsAncestor(root, prev.Commit, "HEAD") {
		if cs, err := githist.Log(root, prev.Commit+"..HEAD", since, historyLimit); err == nil {
			g.HeadTime, g.History = head, mergeHistory(cs, prev.History, since)
			return
		}
	}
	cs, err := githist.Log(root, "HEAD", since, historyLimit)
	if err != nil {
		g.Warnings = append(g.Warnings, "history: "+err.Error())
		return
	}
	g.HeadTime, g.History = head, cs
}

const historyLimit = 50000

// mergeHistory puts the new commits (newest first, as git log lists them)
// before the old ones still in the window, and keeps the newest
// historyLimit, as one git log over the whole range would.
func mergeHistory(fresh, old []githist.Commit, since time.Time) []githist.Commit {
	out := append([]githist.Commit(nil), fresh...)
	for _, c := range old {
		if !c.Time.Before(since) {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out[:min(len(out), historyLimit)]
}

// isIndexableAux picks non-code files worth their own record. Tests, assets
// and docs are left to lookup-time path rules.
func isIndexableAux(p string) bool {
	if strings.HasSuffix(p, "_test.go") || isTSTestLike(p) || strings.Contains(p, "testdata/") || strings.Contains(p, "node_modules/") || isPyVendored(p) || isCSBuildOutput(p) || isRsVendored(p) {
		return false
	}
	switch path.Ext(p) {
	case ".sql", ".proto", ".yaml", ".yml", ".tpl", ".graphql", ".properties", ".toml", ".gradle", ".kts", ".csproj", ".sln", ".props", ".targets":
		return true
	case ".go":
		return true // go files skipped by build constraints still get a record
	}
	switch path.Base(p) {
	case "Dockerfile", "Makefile", "go.mod", "Chart.yaml", "package.json", "pom.xml", "Pipfile", "setup.cfg", "packages.config", "global.json", "nuget.config", "NuGet.config", "rust-toolchain",
		"CMakeLists.txt", "Gemfile", "Rakefile", "composer.json", "build.sbt", "Podfile":
		return true
	}
	if b := path.Base(p); strings.HasPrefix(b, "requirements") && strings.HasSuffix(b, ".txt") {
		return true
	}
	return false
}

type extractJob struct {
	ri     *RepoInfo
	commit string
	dirty  bool
	fp     string
	prev   *Graph // the last extraction, for incremental history
	full   bool   // forced: no reuse
}

func runExtraction(repos []*RepoInfo, cacheDir string, force map[string]bool, forceAll bool, parallel int) (map[string]*Graph, error) {
	var allMods []Module
	for _, ri := range repos {
		if err := ri.loadFiles(); err != nil {
			return nil, fmt.Errorf("%s: %w", ri.Name, err)
		}
		allMods = append(allMods, ri.Modules...)
	}
	graphs := map[string]*Graph{}
	var jobs []extractJob
	for _, ri := range repos {
		commit, dirty, fp := ri.fingerprint(allMods)
		full := forceAll || force[ri.Name]
		prev, err := readGraph(cachePath(cacheDir, ri.Name))
		if err != nil || prev.Version != extractorVersion {
			prev = nil
		}
		if !full && prev != nil && prev.Fingerprint == fp {
			graphs[ri.Name] = prev
			continue
		}
		jobs = append(jobs, extractJob{ri, commit, dirty, fp, prev, full})
	}
	if len(jobs) == 0 {
		fmt.Fprintf(os.Stderr, "extract: all %d repos cached\n", len(repos))
		return graphs, nil
	}
	var mu sync.Mutex
	sem := make(chan struct{}, max(1, parallel))
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j extractJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			t0 := time.Now()
			g, pc := extractRepo(j.ri, allMods, j.commit, j.dirty, j.fp, j.prev, cacheDir, j.full)
			if err := writeGraph(cachePath(cacheDir, j.ri.Name), g); err != nil {
				fmt.Fprintf(os.Stderr, "  %s: cache write: %v\n", j.ri.Name, err)
			}
			reuse := ""
			if pc != nil && pc.seen.Load() > 0 {
				reuse = fmt.Sprintf("  (reused %d of %d parses)", pc.reused.Load(), pc.seen.Load())
			}
			mu.Lock()
			graphs[j.ri.Name] = g
			fmt.Fprintf(os.Stderr, "extract: %-24s %6d nodes %7d edges %5d files  %s%s\n", j.ri.Name, len(g.Nodes), len(g.Edges), len(g.Files), time.Since(t0).Round(100*time.Millisecond), reuse)
			mu.Unlock()
		}(j)
	}
	wg.Wait()
	return graphs, nil
}

func rel(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

func appendUniq(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
