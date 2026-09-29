package indexer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/codemap"
)

type scored struct {
	tags  []codemap.Tag
	cap   *int
	floor int
}

func (s *scored) add(id string, score, via int) {
	for i, t := range s.tags {
		if t.ID == id {
			if score > t.Score {
				s.tags[i] = codemap.Tag{ID: id, Score: score, Via: via}
			}
			return
		}
	}
	s.tags = append(s.tags, codemap.Tag{ID: id, Score: score, Via: via})
}

func (s *scored) setCap(c *int) {
	if c != nil && (s.cap == nil || *c < *s.cap) {
		v := *c
		s.cap = &v
	}
}

func (s *scored) merge(o *scored) {
	for _, t := range o.tags {
		s.add(t.ID, t.Score, t.Via)
	}
	s.floor = max(s.floor, o.floor)
}

// score folds tags into one 0-100 value: the strongest tag plus a small bonus
// for each additional independent hard-to-undo tag.
func (s *scored) score(cfg *Config) int {
	sort.Slice(s.tags, func(i, j int) bool {
		if s.tags[i].Score != s.tags[j].Score {
			return s.tags[i].Score > s.tags[j].Score
		}
		return s.tags[i].ID < s.tags[j].ID
	})
	v := cfg.Rollback.Default
	if len(s.tags) > 0 && s.tags[0].Score > v {
		v = s.tags[0].Score
	}
	for _, t := range s.tags[min(1, len(s.tags)):] {
		if t.Score >= 40 {
			v += cfg.Rollback.MultiTag
		}
	}
	return min(v, 100)
}

func (w *world) applyRules(s *scored, subj codemap.Record, isSym bool, n *Node) {
	cat := ""
	if ri := w.repos[subj.Repo]; ri != nil {
		cat = ri.Category
	}
	p := subj.Path
	sj := Subject{Repo: subj.Repo, Category: cat, Path: p, IsSymbol: isSym}
	if n != nil {
		sj.Kind, sj.Sym, sj.Exported, sj.Tags = n.Kind, n.Sym, n.Exported, n.Tags
	}
	for _, r := range w.cfg.rules {
		if r.matches(sj) {
			if r.Score > 0 || r.Cap == nil {
				s.add(r.ID, r.Score, 0)
			}
			s.setCap(r.Cap)
			s.floor = max(s.floor, r.Floor)
		}
	}
}

// symbolRollback computes per-node rollback tags: rules on the node itself,
// sink calls (direct or within max_hops), and API contract implementation.
func (w *world) symbolRollback() []*scored {
	res := make([]*scored, len(w.nodes))
	for i, n := range w.nodes {
		s := &scored{}
		if n.Repo != "" && !strings.HasPrefix(n.Key, "virtual:") {
			w.applyRules(s, codemap.Record{Repo: n.Repo, Path: n.File}, true, n)
		}
		res[i] = s
	}
	// Direct sink hits: any out-edge (workspace or third-party) whose target
	// key matches a sink pattern.
	for _, sk := range w.cfg.sinks {
		var frontier []int
		hit := make([]int, len(w.nodes))
		for i := range hit {
			hit[i] = -1
		}
		for i, n := range w.nodes {
			// Sinks are about calls; a type or interface that merely mentions
			// PatchOptions in a signature does not write anything.
			if n.Kind == "type" || n.Kind == "iface-method" || n.Kind == "const" || n.Kind == "api-op" {
				continue
			}
			if len(sk.Category) > 0 {
				if ri := w.repos[n.Repo]; ri == nil || !contains(sk.Category, ri.Category) {
					continue
				}
			}
			// The sink method itself (e.g. a sqlc write query) counts as direct.
			direct := anyMatch(sk.re, n.Key)
			for _, e := range w.out[i] {
				if direct {
					break
				}
				if anyMatch(sk.re, w.nodes[e.to].Key) {
					direct = true
					break
				}
			}
			if !direct {
				for _, k := range w.extOut[i] {
					if anyMatch(sk.re, k) {
						direct = true
						break
					}
				}
			}
			if direct {
				hit[i] = 0
				frontier = append(frontier, i)
			}
		}
		for hop := 1; hop <= w.cfg.Rollback.Propagate.MaxHops; hop++ {
			var next []int
			for _, i := range frontier {
				for _, p := range w.in[i] {
					if hit[p] >= 0 {
						continue
					}
					hit[p] = hop
					next = append(next, int(p))
				}
			}
			frontier = next
		}
		for i, h := range hit {
			if h < 0 || w.nodes[i].Internal {
				continue
			}
			sc := int(math.Round(float64(sk.Score) * (1 - w.cfg.Rollback.Propagate.Decay*float64(h))))
			res[i].add(sk.ID, sc, h)
		}
	}
	// Implementing an API operation inherits the operation's contract tags.
	for i, n := range w.nodes {
		if n.Kind != "api-op" {
			continue
		}
		opTags := res[i].tags
		if len(opTags) == 0 {
			continue
		}
		frontier := []int{i}
		seen := map[int]bool{i: true}
		for hop := 1; hop <= 3; hop++ {
			var next []int
			for _, a := range frontier {
				for _, e := range w.out[a] {
					if e.kind != "contract" && e.kind != "impl" {
						continue
					}
					b := int(e.to)
					if seen[b] {
						continue
					}
					seen[b] = true
					next = append(next, b)
					for _, t := range opTags {
						if t.Score >= 40 {
							res[b].add("serves:"+t.ID, t.Score-5, 0)
						}
					}
				}
			}
			frontier = next
		}
	}
	return res
}

type emitter struct {
	w       *world
	cfg     *Config
	syms    *level // node level (identity collapse)
	files   *level
	dirs    *level
	repos   *level
	rb      []*scored
	rev     *reverser
	records map[string][]codemap.Record // repo -> records
	repoRec []codemap.Record
	hist    map[string]*repoHistory
}

func (e *emitter) impact(rank float64, rollback int, floor int, cap *int) (int, string) {
	d := int(math.Round(e.cfg.Impact.RankWeight*rank + e.cfg.Impact.RollbackWeight*float64(rollback)))
	d = max(d, floor)
	if cap != nil && d > *cap {
		d = *cap
	}
	return d, scoreLevel(e.cfg, d)
}

// scoreLevel maps an impact or likelihood score to low | medium | high | critical.
func scoreLevel(cfg *Config, d int) string {
	switch {
	case d >= cfg.Impact.Levels["critical"]:
		return "critical"
	case d >= cfg.Impact.Levels["high"]:
		return "high"
	case d >= cfg.Impact.Levels["medium"]:
		return "medium"
	}
	return "low"
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func symID(n *Node) string {
	if n.File == "" {
		return n.Repo + "/" + n.Dir + "/:" + n.Sym
	}
	return n.Repo + "/" + n.File + ":" + n.Sym
}

func (e *emitter) run() {
	w := e.w
	e.records = map[string][]codemap.Record{}

	// Per-file symbol lists and rollback roll-ups.
	fileNodes := map[int][]int{}
	for i := range w.nodes {
		if f := e.files.member[i]; f >= 0 && e.files.real[f] {
			fileNodes[f] = append(fileNodes[f], i)
		}
	}
	dirFiles := map[string][]string{} // "repo/dir" -> file ids
	dirSubdirs := map[string]map[string]bool{}

	e.hist = map[string]*repoHistory{}
	for repo, g := range w.graphs {
		e.hist[repo] = newRepoHistory(g, e.cfg)
	}

	// ---- symbols ----
	symCount := 0
	fileRB := map[string]*scored{}
	fileCx := map[string]*codemap.Complexity{}
	fileSymCount := map[string]int{}
	fileGenerated := map[string]bool{}
	for i, n := range w.nodes {
		if n.Internal || n.File == "" || n.Kind == "virtual" {
			continue
		}
		fid := n.Repo + "/" + n.File
		s := e.rb[i]
		if fileRB[fid] == nil {
			fileRB[fid] = &scored{}
		}
		fileRB[fid].merge(s)
		fileSymCount[fid]++
		if n.Cyclo > 0 {
			if fileCx[fid] == nil {
				fileCx[fid] = &codemap.Complexity{}
			}
			cxAdd(fileCx[fid], n.Cyclo, n.Nest, n.Cyclo, 1)
		}
		callers, virt := w.directCallers(i)
		callerFiles := map[string]bool{}
		callerRepos := map[string]bool{}
		for _, c := range callers {
			cn := w.nodes[c]
			if cn.Repo+"/"+cn.File != fid {
				callerFiles[cn.Repo+"/"+cn.File] = true
			}
			if cn.Repo != n.Repo {
				callerRepos[cn.Repo] = true
			}
		}
		ext := len(callerFiles)
		rbScore := s.score(e.cfg)
		maxTag := 0
		if len(s.tags) > 0 {
			maxTag = s.tags[0].Score
		}
		emit := ext >= e.cfg.Symbols.MinExternalCallers || maxTag >= e.cfg.Symbols.MinTagScore ||
			(e.cfg.Symbols.EmitExported && n.Exported) || n.Kind == "api-op" || virt
		if !emit {
			continue
		}
		own := map[int32]bool{}
		if f := e.files.member[i]; f >= 0 {
			own[int32(f)] = true
		}
		rc := e.rev.run([]int{i}, n.Repo, own)
		rank := e.syms.pct[e.syms.member[i]]
		d, lvl := e.impact(rank, rbScore, s.floor, s.cap)
		fh := e.hist[n.Repo].file(n.File)
		var scx *codemap.Complexity
		if n.Cyclo > 0 {
			scx = &codemap.Complexity{Cyclo: n.Cyclo, Nest: n.Nest}
		}
		lk, lkLvl := e.likelihood(fh, scx)
		if contains(n.Tags, "generated") {
			lk, lkLvl = 0, "low" // regenerated, not edited: its defects live in the source
		}
		sort.Slice(callers, func(a, b int) bool {
			return e.syms.pr[e.syms.member[callers[a]]] > e.syms.pr[e.syms.member[callers[b]]]
		})
		var top []string
		for _, c := range callers {
			if len(top) == 5 {
				break
			}
			top = append(top, symID(w.nodes[c]))
		}
		facts := append([]string(nil), n.Tags...)
		if virt {
			facts = append(facts, "external-consumers")
		}
		rec := codemap.Record{
			ID: symID(n), Level: "symbol", Repo: n.Repo, Path: n.File, Sym: n.Sym, Kind: n.Kind,
			Lines: []int{n.Start, n.End}, Impact: d, ImpactLevel: lvl,
			Likelihood: lk, LikelihoodLevel: lkLvl, Hist: fh, Cx: scx,
			Rank: round1(rank), RankInRepo: round1(e.syms.pctRepo[e.syms.member[i]]),
			PR: round3(e.syms.pr[e.syms.member[i]] * float64(len(e.syms.groups))), RankSource: "graph",
			Rollback: rbScore, RollbackTags: s.tags,
			Callers: len(callers), CallerFiles: len(callerFiles), CallerRepos: sortedKeys(callerRepos),
			DepSyms: rc.syms, DepFiles: rc.files, DepRepos: rc.repos,
			TopCallers: top, Exported: n.Exported, Facts: facts, ImpactCap: s.cap,
		}
		if contains(n.Tags, "generated") {
			rec.Generated = true
			fileGenerated[fid] = true
		}
		e.records[n.Repo] = append(e.records[n.Repo], rec)
		symCount++
	}
	fmt.Fprintf(os.Stderr, "emit: %d symbol records\n", symCount)

	// ---- files ----
	type fileOut struct {
		rec codemap.Record
		rb  *scored
	}
	var filesOut []*fileOut
	for _, repo := range sortedKeys(w.graphs) {
		g := w.graphs[repo]
		for _, f := range g.Files {
			fid := repo + "/" + f.Path
			dir := path.Dir(f.Path)
			if dir == "." {
				dir = ""
			}
			dirFiles[repo+"/"+dir] = append(dirFiles[repo+"/"+dir], fid)
			s := &scored{}
			if r := fileRB[fid]; r != nil {
				s.merge(r) // tags and floor; a symbol's test cap does not cap its file
			}
			w.applyRules(s, codemap.Record{Repo: repo, Path: f.Path}, false, nil)
			rec := codemap.Record{ID: fid, Level: "file", Repo: repo, Path: f.Path, Kind: f.Lang,
				Lines: []int{1, f.Lines}, Generated: f.Generated || fileGenerated[fid], Symbols: fileSymCount[fid]}
			rec.Hist, rec.Cx, rec.CoChange = e.hist[repo].file(f.Path), fileCx[fid], e.hist[repo].partners(f.Path)
			rec.Likelihood, rec.LikelihoodLevel = e.likelihood(rec.Hist, rec.Cx)
			if rec.Generated {
				rec.Likelihood, rec.LikelihoodLevel = 0, "low"
			}
			if gi, ok := e.files.gidx[fid]; ok && len(fileNodes[gi]) > 0 {
				rec.Rank = round1(e.files.pct[gi])
				rec.RankInRepo = round1(e.files.pctRepo[gi])
				rec.PR = round3(e.files.pr[gi] * float64(len(e.files.groups)))
				rec.RankSource = "graph"
				own := map[int32]bool{int32(gi): true}
				rc := e.rev.run(fileNodes[gi], repo, own)
				rec.DepSyms, rec.DepFiles, rec.DepRepos = rc.syms, rc.files, rc.repos
				callers := map[int]bool{}
				for _, ni := range fileNodes[gi] {
					cs, virt := w.directCallers(ni)
					if virt {
						rec.Facts = appendUniq(rec.Facts, "external-consumers")
					}
					for _, c := range cs {
						if e.files.member[c] != gi {
							callers[c] = true
						}
					}
				}
				cf, cr := map[string]bool{}, map[string]bool{}
				for c := range callers {
					cn := w.nodes[c]
					cf[cn.Repo+"/"+cn.File] = true
					if cn.Repo != repo {
						cr[cn.Repo] = true
					}
				}
				rec.Callers, rec.CallerFiles, rec.CallerRepos = len(callers), len(cf), sortedKeys(cr)
				// Top dependent files by file rank.
				fr := make([]string, 0, len(cf))
				for k := range cf {
					fr = append(fr, k)
				}
				sort.Slice(fr, func(a, b int) bool {
					return e.files.pr[e.files.gidx[fr[a]]] > e.files.pr[e.files.gidx[fr[b]]]
				})
				if len(fr) > 5 {
					fr = fr[:5]
				}
				rec.TopCallers = fr
			}
			filesOut = append(filesOut, &fileOut{rec, s})
		}
	}

	// ---- dirs ----
	// Every ancestor of an indexed file gets a record. Directories with Go
	// packages or TS modules rank from the collapsed graph; purely
	// structural ones take the max of their children.
	type dirOut struct {
		rec      codemap.Record
		rbScores []int
		rb       *scored
		hasGraph bool
		lk       []int // file likelihoods under the dir
		hist     histAgg
		cx       codemap.Complexity
	}
	dirsOut := map[string]*dirOut{}
	var ensureDir func(repo, dir string) *dirOut
	ensureDir = func(repo, dir string) *dirOut {
		key := repo + "/" + dir
		if d, ok := dirsOut[key]; ok {
			return d
		}
		d := &dirOut{rec: codemap.Record{ID: repo + "/" + dir + "/", Level: "dir", Repo: repo, Path: dir}, rb: &scored{}}
		if dir == "" {
			d.rec.ID = repo + "/"
		}
		w.applyRules(d.rb, codemap.Record{Repo: repo, Path: dir + "/"}, false, nil)
		if gi, ok := e.dirs.gidx[key]; ok {
			d.rec.Rank = round1(e.dirs.pct[gi])
			d.rec.RankInRepo = round1(e.dirs.pctRepo[gi])
			d.rec.PR = round3(e.dirs.pr[gi] * float64(len(e.dirs.groups)))
			d.rec.RankSource = "graph"
			d.hasGraph = true
		}
		dirsOut[key] = d
		if dir != "" {
			parent := path.Dir(dir)
			if parent == "." {
				parent = ""
			}
			ensureDir(repo, parent)
			if dirSubdirs[repo+"/"+parent] == nil {
				dirSubdirs[repo+"/"+parent] = map[string]bool{}
			}
			dirSubdirs[repo+"/"+parent][key] = true
		}
		return d
	}
	for _, fo := range filesOut {
		dir := path.Dir(fo.rec.Path)
		if dir == "." {
			dir = ""
		}
		d := ensureDir(fo.rec.Repo, dir)
		d.rbScores = append(d.rbScores, fo.rb.score(e.cfg))
		d.rec.Symbols += fo.rec.Symbols
		d.lk = append(d.lk, fo.rec.Likelihood)
		d.hist.addFile(fo.rec.Hist, e.hist[fo.rec.Repo].stats(fo.rec.Path))
		if c := fo.rec.Cx; c != nil {
			cxAdd(&d.cx, c.Cyclo, c.Nest, c.Total, c.Funcs)
		}
	}
	// Dir transitive dependents: all nodes in the dir.
	dirNodes := map[string][]int{}
	for i, n := range w.nodes {
		if n.Internal && n.Kind != "stub" || n.Kind == "virtual" {
			continue
		}
		dirNodes[n.Repo+"/"+n.Dir] = append(dirNodes[n.Repo+"/"+n.Dir], i)
	}
	dirKeys := sortedKeys(dirsOut)
	// Deepest first so children are final before parents aggregate them.
	// Depth counts the dir's own path segments (the repo root is 0), not
	// slashes in the key: "repo/" and "repo/a" both have one.
	depth := func(key string) int {
		if p := dirsOut[key].rec.Path; p != "" {
			return strings.Count(p, "/") + 1
		}
		return 0
	}
	sort.SliceStable(dirKeys, func(a, b int) bool {
		if da, db := depth(dirKeys[a]), depth(dirKeys[b]); da != db {
			return da > db
		}
		return dirKeys[a] < dirKeys[b]
	})
	for _, key := range dirKeys {
		d := dirsOut[key]
		if !d.hasGraph {
			for sub := range dirSubdirs[key] {
				c := dirsOut[sub]
				if c.rec.Rank > d.rec.Rank {
					d.rec.Rank, d.rec.RankInRepo, d.rec.PR = c.rec.Rank, c.rec.RankInRepo, c.rec.PR
					d.rec.RankSource = "children"
				}
			}
		}
		for sub := range dirSubdirs[key] {
			c := dirsOut[sub]
			d.rbScores = append(d.rbScores, c.rbScores...)
			d.rec.Symbols += c.rec.Symbols
			d.lk = append(d.lk, c.lk...)
			d.hist.merge(&c.hist)
			cxAdd(&d.cx, c.cx.Cyclo, c.cx.Nest, c.cx.Total, c.cx.Funcs)
		}
		if ns := dirNodes[key]; len(ns) > 0 {
			rc := e.rev.run(ns, d.rec.Repo, nil)
			// exclude the dir itself from dep counts
			d.rec.DepSyms, d.rec.DepFiles, d.rec.DepRepos = rc.syms, rc.files, rc.repos
		}
	}
	for _, key := range dirKeys {
		d := dirsOut[key]
		base := d.rb.score(e.cfg)
		p75 := pctInt(d.rbScores, 0.75)
		rbv := max(base, p75)
		if len(d.rb.tags) == 0 {
			rbv = max(p75, e.cfg.Rollback.Default)
		}
		d.rec.Rollback = rbv
		d.rec.RollbackTags = d.rb.tags
		d.rec.Impact, d.rec.ImpactLevel = e.impact(d.rec.Rank, rbv, d.rb.floor, d.rb.cap)
		d.rec.ImpactCap = d.rb.cap
		// Like rollback, a directory's likelihood is its 75th-percentile
		// file, so one hot file does not light up a whole service.
		d.rec.Likelihood = pctInt(d.lk, 0.75)
		d.rec.LikelihoodLevel = scoreLevel(e.cfg, d.rec.Likelihood)
		d.rec.Hist, d.rec.Cx = d.hist.record(), cxPtr(d.cx)
		if d.rec.Path == "" {
			continue // the repo record covers the root
		}
		e.records[d.rec.Repo] = append(e.records[d.rec.Repo], d.rec)
	}

	// Files without graph nodes (SQL, helm, specs without ops, build files)
	// inherit their nearest ranked directory.
	for _, fo := range filesOut {
		if fo.rec.RankSource == "" {
			dir := path.Dir(fo.rec.Path)
			if dir == "." {
				dir = ""
			}
			for {
				if d, ok := dirsOut[fo.rec.Repo+"/"+dir]; ok && d.rec.RankSource != "" {
					fo.rec.Rank, fo.rec.RankInRepo = d.rec.Rank, d.rec.RankInRepo
					fo.rec.RankSource = "inherited:" + d.rec.ID
					break
				}
				if dir == "" {
					break
				}
				dir = path.Dir(dir)
				if dir == "." {
					dir = ""
				}
			}
		}
		fo.rec.Rollback = fo.rb.score(e.cfg)
		fo.rec.RollbackTags = fo.rb.tags
		fo.rec.ImpactCap = fo.rb.cap
		fo.rec.Impact, fo.rec.ImpactLevel = e.impact(fo.rec.Rank, fo.rec.Rollback, fo.rb.floor, fo.rb.cap)
		e.records[fo.rec.Repo] = append(e.records[fo.rec.Repo], fo.rec)
	}

	// ---- repos ----
	repoNodes := map[string][]int{}
	for i, n := range w.nodes {
		if n.Kind == "virtual" || n.Repo == "" {
			continue
		}
		repoNodes[n.Repo] = append(repoNodes[n.Repo], i)
	}
	for _, repo := range sortedKeys(w.graphs) {
		gi, ok := e.repos.gidx[repo]
		d := dirsOut[repo+"/"]
		rec := codemap.Record{ID: repo, Level: "repo", Repo: repo}
		if ri := w.repos[repo]; ri != nil {
			rec.Kind = ri.Category
		}
		if ok {
			rec.Rank = round1(e.repos.pct[gi])
			rec.PR = round3(e.repos.pr[gi] * float64(len(e.repos.groups)))
			rec.RankSource = "graph"
		}
		rc := e.rev.run(repoNodes[repo], repo, nil)
		rec.DepRepos = rc.repos
		rec.DepFiles = rc.files
		rec.DepSyms = rc.syms
		// Direct dependent repos.
		cr := map[string]bool{}
		for _, ni := range repoNodes[repo] {
			for _, p := range w.in[ni] {
				if r := w.nodes[p].Repo; r != "" && r != repo {
					cr[r] = true
				}
			}
		}
		rec.CallerRepos = sortedKeys(cr)
		rec.Callers = len(cr)
		if d != nil {
			rec.Symbols = d.rec.Symbols
			rec.Rollback = d.rec.Rollback
			rec.RollbackTags = d.rec.RollbackTags
			rec.ImpactCap = d.rec.ImpactCap
			rec.Likelihood, rec.LikelihoodLevel, rec.Hist, rec.Cx = d.rec.Likelihood, d.rec.LikelihoodLevel, d.rec.Hist, d.rec.Cx
		}
		rec.Impact, rec.ImpactLevel = e.impact(rec.Rank, rec.Rollback, 0, rec.ImpactCap)
		e.repoRec = append(e.repoRec, rec)
	}
}

func pctInt(v []int, q float64) int {
	if len(v) == 0 {
		return 0
	}
	s := append([]int(nil), v...)
	sort.Ints(s)
	i := int(math.Ceil(q*float64(len(s)))) - 1
	return s[max(0, min(i, len(s)-1))]
}

func writeOutput(outDir string, e *emitter, cfg *Config, stats map[string]any) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	// Drop shards of repos that are no longer indexed.
	old, _ := filepath.Glob(filepath.Join(outDir, "*.jsonl"))
	for _, p := range old {
		name := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		if _, ok := e.records[name]; !ok && name != "repos" {
			os.Remove(p)
		}
	}
	counts := map[string]int{}
	for repo, recs := range e.records {
		sort.Slice(recs, func(i, j int) bool {
			a, b := recs[i], recs[j]
			if a.Path != b.Path {
				return a.Path < b.Path
			}
			lo := map[string]int{"dir": 0, "file": 1, "symbol": 2}
			if lo[a.Level] != lo[b.Level] {
				return lo[a.Level] < lo[b.Level]
			}
			if len(a.Lines) > 0 && len(b.Lines) > 0 && a.Lines[0] != b.Lines[0] {
				return a.Lines[0] < b.Lines[0]
			}
			return a.ID < b.ID
		})
		for _, r := range recs {
			counts[r.Level]++
		}
		if err := writeJSONL(filepath.Join(outDir, repo+".jsonl"), recs); err != nil {
			return err
		}
	}
	sort.Slice(e.repoRec, func(i, j int) bool { return e.repoRec[i].Impact > e.repoRec[j].Impact })
	counts["repo"] = len(e.repoRec)
	if err := writeJSONL(filepath.Join(outDir, "repos.jsonl"), e.repoRec); err != nil {
		return err
	}
	meta := codemap.Meta{
		Version: 1, GeneratedAt: time.Now().UTC().Format(time.RFC3339), Repos: map[string]codemap.RepoMeta{},
		Counts: counts, Weights: codemap.Weights{Rank: cfg.Impact.RankWeight, Rollback: cfg.Impact.RollbackWeight},
		Levels: cfg.Impact.Levels, Reasons: map[string]string{}, Stats: stats,
		Likelihood: cfg.Likelihood, HistoryDays: cfg.History.Days,
	}
	for name, g := range e.w.graphs {
		cat, remote := "", ""
		if ri := e.w.repos[name]; ri != nil {
			cat, remote = ri.Category, repoRemote(ri.Root)
		}
		meta.Repos[name] = codemap.RepoMeta{Commit: g.Commit, Dirty: g.Dirty, Category: cat, Remote: remote}
	}
	for _, r := range cfg.rules {
		meta.Reasons[r.ID] = r.Reason
		m := r.Match
		if len(m.Kind) > 0 || len(m.Tags) > 0 || m.Sym != "" || m.Exported != nil {
			continue
		}
		meta.Rules = append(meta.Rules, codemap.PathRule{ID: r.ID, Score: r.Score, Cap: r.Cap, Floor: r.Floor, Path: m.Path, NotPath: m.NotPath, Repo: m.Repo, Category: m.Category})
	}
	for _, s := range cfg.sinks {
		meta.Reasons[s.ID] = s.Reason
	}
	b, _ := json.MarshalIndent(meta, "", "  ")
	return os.WriteFile(filepath.Join(outDir, "meta.json"), append(b, '\n'), 0o644)
}

func writeJSONL(p string, recs []codemap.Record) error {
	tmp := p + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	for i := range recs {
		if err := enc.Encode(&recs[i]); err != nil {
			f.Close()
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, p)
}
