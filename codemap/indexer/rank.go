package indexer

import (
	"fmt"
	"math"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// world is the union of all repo graphs plus cross-repo contract edges.
type world struct {
	cfg      *Config
	repos    map[string]*RepoInfo
	graphs   map[string]*Graph
	modules  []modRef
	nodes    []*Node
	idx      map[string]int
	virtual  map[int]float64 // node -> teleport share
	out      [][]wedge
	in       [][]int32
	extOut   map[int][]string // edges to third-party keys, for sink rules
	stats    map[string]any
	fileInfo map[string]FileInfo // "repo/path" -> info
}

type modRef struct {
	Module
	repo string
}

type wedge struct {
	to   int32
	w    float64
	kind string
}

func buildWorld(cfg *Config, repos []*RepoInfo, graphs map[string]*Graph) *world {
	w := &world{cfg: cfg, repos: map[string]*RepoInfo{}, graphs: graphs, idx: map[string]int{},
		virtual: map[int]float64{}, extOut: map[int][]string{}, stats: map[string]any{}, fileInfo: map[string]FileInfo{}}
	for _, r := range repos {
		w.repos[r.Name] = r
	}
	names := sortedKeys(graphs)
	for _, name := range names {
		g := graphs[name]
		for _, m := range g.Modules {
			w.modules = append(w.modules, modRef{m, name})
		}
		for _, n := range g.Nodes {
			if _, dup := w.idx[n.Key]; dup {
				continue
			}
			w.idx[n.Key] = len(w.nodes)
			w.nodes = append(w.nodes, n)
		}
		for _, f := range g.Files {
			w.fileInfo[name+"/"+f.Path] = f
		}
	}
	sort.Slice(w.modules, func(i, j int) bool { return len(w.modules[i].Path) > len(w.modules[j].Path) })

	type rawEdge struct {
		from, to int
		n        int
		kind     string
	}
	var raw []rawEdge
	stubs, ext := 0, 0
	for _, name := range names {
		for _, e := range graphs[name].Edges {
			fi, ok := w.idx[e.From]
			if !ok {
				continue
			}
			ti, ok := w.idx[e.To]
			if !ok {
				ti = w.stub(e.To)
				if ti < 0 {
					w.extOut[fi] = append(w.extOut[fi], e.To)
					ext++
					continue
				}
				stubs++
			}
			raw = append(raw, rawEdge{fi, ti, e.N, e.Kind})
		}
	}
	for _, e := range w.contractEdges() {
		raw = append(raw, rawEdge{e[0], e[1], 1, "contract"})
	}
	for _, vc := range cfg.Rank.VirtualConsumers {
		vi := w.addNode(&Node{Key: "virtual:" + vc.Name, Kind: "virtual", Repo: "", Sym: vc.Name, Internal: true})
		w.virtual[vi] = vc.Weight
		globs := make([]*regexp.Regexp, 0, len(vc.Specs))
		for _, g := range vc.Specs {
			globs = append(globs, globRe(g))
		}
		hits := 0
		for i, n := range w.nodes {
			if n.Kind == "api-op" && anyMatch(globs, n.Repo+"/"+n.File) {
				raw = append(raw, rawEdge{vi, i, 1, "contract"})
				hits++
			}
		}
		w.stats["virtual:"+vc.Name] = hits
	}
	w.out = make([][]wedge, len(w.nodes))
	w.in = make([][]int32, len(w.nodes))
	for _, e := range raw {
		if e.from == e.to {
			continue
		}
		wt := 1 + math.Log(float64(max(1, e.n)))
		w.out[e.from] = append(w.out[e.from], wedge{int32(e.to), wt, e.kind})
		w.in[e.to] = append(w.in[e.to], int32(e.from))
	}
	w.stats["nodes"] = len(w.nodes)
	w.stats["edges"] = len(raw)
	w.stats["stub_nodes"] = stubs
	w.stats["third_party_edges"] = ext
	return w
}

func (w *world) addNode(n *Node) int {
	if i, ok := w.idx[n.Key]; ok {
		return i
	}
	w.idx[n.Key] = len(w.nodes)
	w.nodes = append(w.nodes, n)
	return len(w.nodes) - 1
}

// stub creates a placeholder for a Go symbol that belongs to a workspace
// module but is not present at the indexed commit (consumer pins an older or
// newer version). It keeps rank flowing into the right package.
func (w *world) stub(key string) int {
	if !strings.HasPrefix(key, "go:") {
		return -1
	}
	rest := key[3:]
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return -1
	}
	pkg, sym := rest[:colon], rest[colon+1:]
	for _, m := range w.modules {
		if pkg == m.Path || strings.HasPrefix(pkg, m.Path+"/") {
			dir := path.Join(m.Dir, strings.TrimPrefix(strings.TrimPrefix(pkg, m.Path), "/"))
			if dir == "." {
				dir = ""
			}
			return w.addNode(&Node{Key: key, Kind: "stub", Repo: m.repo, Dir: dir, Sym: sym, Internal: true})
		}
	}
	return -1
}

var clientMethodRe = regexp.MustCompile(`^\(\*?(Client|ClientWithResponses|ClientInterface|ClientWithResponsesInterface)\)\.(\w+?)(WithBody)?(WithResponse)?$`)
var clientFuncRe = regexp.MustCompile(`^(?:New(\w+?)Request(?:WithBody)?|Parse(\w+?)Response)$`)
var serverIfaceRe = regexp.MustCompile(`^\((StrictServerInterface|ServerInterface)\)\.(\w+)$`)

func normOp(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// contractEdges links callers of generated clients to OpenAPI operations and
// operations to the server interfaces that implement them. It also links TS
// URL literals and configured generated TS clients to operations.
func (w *world) contractEdges() [][2]int {
	var out [][2]int
	byRepoDir := map[string][]int{}
	for i, n := range w.nodes {
		byRepoDir[n.Repo+"|"+n.Dir] = append(byRepoDir[n.Repo+"|"+n.Dir], i)
	}
	linkedClients, linkedServers := 0, 0
	type opRef struct {
		idx  int
		op   SpecOp
		spec *Spec
		repo string
	}
	opsByRepo := map[string][]opRef{}
	for _, name := range sortedKeys(w.graphs) {
		g := w.graphs[name]
		for si := range g.Specs {
			sp := &g.Specs[si]
			specDir := path.Dir(sp.File)
			apiName := path.Base(specDir)
			ops := map[string]int{}
			for _, op := range sp.Ops {
				if i, ok := w.idx["api:"+name+"/"+sp.File+":"+op.ID]; ok {
					ops[normOp(op.ID)] = i
					opsByRepo[name] = append(opsByRepo[name], opRef{i, op, sp, name})
				}
			}
			// Generated Go client packages: <any>/client/<apiName>.
			var clientDirs []string
			for k := range byRepoDir {
				repo, dir, _ := strings.Cut(k, "|")
				if repo != name {
					continue
				}
				if path.Base(dir) == apiName && (strings.Contains(dir, "client") || dir == specDir) {
					clientDirs = append(clientDirs, dir)
				}
			}
			for _, cfgc := range w.cfg.Contracts.GeneratedClients {
				if cfgc.Spec == name+"/"+sp.File && cfgc.Style != "orval" {
					clientDirs = append(clientDirs, cfgc.Dir)
				}
			}
			serverFound := map[string]bool{}
			for _, cd := range clientDirs {
				for _, i := range byRepoDir[name+"|"+cd] {
					n := w.nodes[i]
					var op string
					if m := clientMethodRe.FindStringSubmatch(n.Sym); m != nil {
						op = m[2]
					} else if m := clientFuncRe.FindStringSubmatch(n.Sym); m != nil {
						op = m[1] + m[2]
					} else if m := serverIfaceRe.FindStringSubmatch(n.Sym); m != nil {
						if oi, ok := ops[normOp(m[2])]; ok {
							out = append(out, [2]int{oi, i})
							serverFound[normOp(m[2])] = true
							linkedServers++
						}
						continue
					}
					if op == "" {
						continue
					}
					if oi, ok := ops[normOp(op)]; ok {
						out = append(out, [2]int{i, oi})
						linkedClients++
					}
				}
			}
			// No generated server interface: link directly to same-named
			// methods in the spec's own package.
			for _, i := range byRepoDir[name+"|"+specDir] {
				n := w.nodes[i]
				if n.Kind != "method" {
					continue
				}
				_, m, ok := strings.Cut(n.Sym, ").")
				if !ok {
					continue
				}
				if oi, ok := ops[normOp(m)]; ok && !serverFound[normOp(m)] {
					out = append(out, [2]int{oi, i})
					linkedServers++
				}
			}
			// Configured generated TS clients (orval: camelCase op functions).
			for _, cfgc := range w.cfg.Contracts.GeneratedClients {
				if cfgc.Spec != name+"/"+sp.File || cfgc.Style != "orval" {
					continue
				}
				for k, idxs := range byRepoDir {
					repo, dir, _ := strings.Cut(k, "|")
					if repo != cfgc.Repo || !(dir == cfgc.Dir || strings.HasPrefix(dir, cfgc.Dir+"/")) {
						continue
					}
					for _, i := range idxs {
						if oi, ok := ops[normOp(w.nodes[i].Sym)]; ok {
							out = append(out, [2]int{i, oi})
							linkedClients++
						}
					}
				}
			}
		}
	}
	// TS URL literals -> operations of the named service.
	matched, unmatched := 0, 0
	for _, name := range sortedKeys(w.graphs) {
		for _, u := range w.graphs[name].URLRefs {
			fi, ok := w.idx[u.From]
			if !ok {
				continue
			}
			best, bestScore := []int{}, 0
			refSegs := splitSegs(u.Path)
			for _, o := range opsByRepo[u.Service] {
				s := matchURL(refSegs, o.spec.Servers, o.op.Path)
				if s > bestScore {
					best, bestScore = []int{o.idx}, s
				} else if s == bestScore && s > 0 {
					best = append(best, o.idx)
				}
			}
			if len(best) == 0 {
				unmatched++
				continue
			}
			matched++
			for _, oi := range best {
				out = append(out, [2]int{fi, oi})
			}
		}
	}
	w.stats["contract_client_links"] = linkedClients
	w.stats["contract_server_links"] = linkedServers
	w.stats["url_refs_matched"] = matched
	w.stats["url_refs_unmatched"] = unmatched
	return out
}

func splitSegs(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func segEq(a, b string) bool {
	return a == b || a == "{}" || strings.HasPrefix(b, "{") || strings.HasPrefix(a, "{")
}

// matchURL scores how well a URL literal (already stripped of /<service>)
// matches an operation. The op path must match completely, aligned to the
// end of the literal; matching server prefix segments add to the score.
func matchURL(ref []string, servers []string, opPath string) int {
	op := splitSegs(opPath)
	if len(op) == 0 || len(ref) < len(op) {
		return 0
	}
	off := len(ref) - len(op)
	literal := 0
	for i, s := range op {
		r := ref[off+i]
		if !segEq(r, s) {
			return 0
		}
		if r == s {
			literal++
		}
	}
	if literal == 0 {
		return 0 // all-wildcard match is noise
	}
	score := len(op)*10 + literal
	for _, sv := range servers {
		ss := splitSegs(sv)
		if len(ss) <= off {
			ok := true
			for i := range ss {
				if ref[off-len(ss)+i] != ss[i] {
					ok = false
					break
				}
			}
			if ok && len(ss) > 0 {
				score += 5 * len(ss)
				break
			}
		}
	}
	return score
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pagerank runs weighted PageRank. Edges point from dependent to
// dependency, so rank accumulates on code that many (important) things use.
func pagerank(n int, out [][]wedge, tele []float64, d float64) []float64 {
	outW := make([]float64, n)
	for i := range out {
		for _, e := range out[i] {
			outW[i] += e.w
		}
	}
	pr := make([]float64, n)
	copy(pr, tele)
	next := make([]float64, n)
	for iter := 0; iter < 200; iter++ {
		dangling := 0.0
		for i := 0; i < n; i++ {
			if outW[i] == 0 {
				dangling += pr[i]
			}
		}
		for i := range next {
			next[i] = (1-d)*tele[i] + d*dangling*tele[i]
		}
		for i := 0; i < n; i++ {
			if outW[i] == 0 {
				continue
			}
			share := d * pr[i] / outW[i]
			for _, e := range out[i] {
				next[e.to] += share * e.w
			}
		}
		diff := 0.0
		for i := range pr {
			diff += math.Abs(next[i] - pr[i])
		}
		pr, next = next, pr
		if diff < 1e-10 {
			break
		}
	}
	return pr
}

// level is a collapsed view of the node graph (file, dir, or repo).
type level struct {
	name    string
	groups  []string
	gidx    map[string]int
	member  []int // node -> group, -1 = not in this level
	real    []bool
	out     [][]wedge
	pr      []float64
	pct     []float64
	pctRepo []float64
	repo    []string
}

func (w *world) collapse(name string, groupOf func(n *Node) (string, string)) *level {
	lv := &level{name: name, gidx: map[string]int{}, member: make([]int, len(w.nodes))}
	for i, n := range w.nodes {
		if _, v := w.virtual[i]; v {
			g := "virtual:" + n.Sym
			lv.member[i] = lv.group(g, "", false)
			continue
		}
		g, repo := groupOf(n)
		if g == "" {
			lv.member[i] = -1
			continue
		}
		lv.member[i] = lv.group(g, repo, true)
	}
	acc := make([]map[int32]float64, len(lv.groups))
	for i := range w.out {
		a := lv.member[i]
		if a < 0 {
			continue
		}
		for _, e := range w.out[i] {
			b := lv.member[e.to]
			if b < 0 || a == b {
				continue
			}
			if acc[a] == nil {
				acc[a] = map[int32]float64{}
			}
			acc[a][int32(b)] += e.w
		}
	}
	lv.out = make([][]wedge, len(lv.groups))
	for a, m := range acc {
		for b, wt := range m {
			// Dampen fat group-to-group bundles so one heavy importer does
			// not dominate.
			lv.out[a] = append(lv.out[a], wedge{b, 1 + math.Log(wt), ""})
		}
	}
	return lv
}

func (lv *level) group(g, repo string, real bool) int {
	if i, ok := lv.gidx[g]; ok {
		return i
	}
	lv.gidx[g] = len(lv.groups)
	lv.groups = append(lv.groups, g)
	lv.real = append(lv.real, real)
	lv.repo = append(lv.repo, repo)
	return len(lv.groups) - 1
}

// teleport gives real nodes a uniform share and virtual consumers their
// configured share of the total.
func teleport(n int, real []bool, virtualShare map[int]float64) []float64 {
	t := make([]float64, n)
	vs := 0.0
	for _, s := range virtualShare {
		vs += s
	}
	// Shares over 0.9 are scaled down together, keeping their ratios, so
	// the vector still sums to 1.
	scale := 1.0
	if vs > 0.9 {
		scale = 0.9 / vs
		vs = 0.9
	}
	cnt := 0
	for i := 0; i < n; i++ {
		if real[i] {
			cnt++
		}
	}
	for i := 0; i < n; i++ {
		if real[i] && cnt > 0 {
			t[i] = (1 - vs) / float64(cnt)
		}
	}
	for i, s := range virtualShare {
		t[i] = s * scale
	}
	return t
}

// percentiles: share of population strictly below each value, 0-100.
func percentiles(vals []float64, include []bool) []float64 {
	var pop []float64
	for i, v := range vals {
		if include[i] {
			pop = append(pop, v)
		}
	}
	sort.Float64s(pop)
	out := make([]float64, len(vals))
	if len(pop) == 0 {
		return out
	}
	for i, v := range vals {
		if !include[i] {
			continue
		}
		below := sort.SearchFloat64s(pop, v*(1-1e-9))
		out[i] = 100 * float64(below) / float64(len(pop))
	}
	return out
}

func (w *world) rankLevel(lv *level) {
	vshare := map[int]float64{}
	for ni, s := range w.virtual {
		vshare[lv.member[ni]] = s
	}
	t := teleport(len(lv.groups), lv.real, vshare)
	lv.pr = pagerank(len(lv.groups), lv.out, t, w.cfg.Rank.Damping)
	lv.pct = percentiles(lv.pr, lv.real)
	lv.pctRepo = make([]float64, len(lv.groups))
	byRepo := map[string][]int{}
	for i := range lv.groups {
		if lv.real[i] {
			byRepo[lv.repo[i]] = append(byRepo[lv.repo[i]], i)
		}
	}
	for _, idxs := range byRepo {
		vals := make([]float64, len(idxs))
		inc := make([]bool, len(idxs))
		for k, i := range idxs {
			vals[k], inc[k] = lv.pr[i], true
		}
		p := percentiles(vals, inc)
		for k, i := range idxs {
			lv.pctRepo[i] = p[k]
		}
	}
	fmt.Fprintf(os.Stderr, "rank: %-6s %6d groups\n", lv.name, len(lv.groups))
}

// reach holds reverse-reachability results for one start set.
type reach struct {
	syms, files int
	repos       []string
}

// reverser computes transitive dependents with a reusable visit stamp.
type reverser struct {
	w      *world
	stamp  []int32
	cur    int32
	queue  []int32
	fileID []int32 // node -> file group id, -1 none
	nFiles int
}

func newReverser(w *world, files *level) *reverser {
	r := &reverser{w: w, stamp: make([]int32, len(w.nodes)), fileID: make([]int32, len(w.nodes))}
	for i := range w.nodes {
		r.fileID[i] = int32(files.member[i])
	}
	r.nFiles = len(files.groups)
	return r
}

func (r *reverser) run(starts []int, ownRepo string, ownFiles map[int32]bool) reach {
	r.cur++
	if r.cur == math.MaxInt32 {
		for i := range r.stamp {
			r.stamp[i] = 0
		}
		r.cur = 1
	}
	r.queue = r.queue[:0]
	for _, s := range starts {
		r.stamp[s] = r.cur
		r.queue = append(r.queue, int32(s))
	}
	files := map[int32]bool{}
	repos := map[string]bool{}
	syms := 0
	for qi := 0; qi < len(r.queue); qi++ {
		n := r.queue[qi]
		for _, p := range r.w.in[n] {
			if r.stamp[p] == r.cur {
				continue
			}
			r.stamp[p] = r.cur
			r.queue = append(r.queue, p)
			node := r.w.nodes[p]
			if node.Internal {
				continue
			}
			syms++
			if f := r.fileID[p]; f >= 0 && !ownFiles[f] {
				files[f] = true
			}
			if node.Repo != "" && node.Repo != ownRepo {
				repos[node.Repo] = true
			}
		}
	}
	return reach{syms: syms, files: len(files), repos: sortedKeys(repos)}
}

// directCallers returns real callers, skipping through internal nodes
// (interface methods, API operations, module bodies, virtual consumers).
func (w *world) directCallers(i int) (callers []int, virtual bool) {
	seen := map[int32]bool{int32(i): true}
	stack := []int32{int32(i)}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, p := range w.in[n] {
			if seen[p] {
				continue
			}
			seen[p] = true
			if _, v := w.virtual[int(p)]; v {
				virtual = true
				continue
			}
			pn := w.nodes[p]
			if pn.Internal || pn.Kind == "api-op" {
				stack = append(stack, p)
				continue
			}
			callers = append(callers, int(p))
		}
	}
	return callers, virtual
}
