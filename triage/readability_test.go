package triage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Readability exploration for review steps: how readable is each step when
// a grouping is shown to a human as one unit? No model calls. Skipped
// unless PR_READABILITY=1; writes experiments/grouping-readability/results.

const readabilityDir = "../experiments/grouping-readability"

type rpr struct {
	name  string
	units []*Unit
}

// loadReadabilityPRs is the five fetched sweep PRs plus
// perfectscale/psc-coroot-node-agent#43, rebuilt from its cached result.
func loadReadabilityPRs(t *testing.T) []rpr {
	t.Helper()
	var out []rpr
	if b, err := os.ReadFile(filepath.Join(tuningDir, "data/index.json")); err == nil {
		var prs []tuningPR
		if err := json.Unmarshal(b, &prs); err != nil {
			t.Fatal(err)
		}
		policy := DefaultPolicy()
		for _, pr := range prs {
			raw, err := os.ReadFile(filepath.Join(pr.Dir, "pr.diff"))
			if err != nil {
				t.Fatal(err)
			}
			src, err := FromDiff(string(raw), filepath.Join(pr.Dir, "head"), filepath.Join(pr.Dir, "base"))
			if err != nil {
				t.Fatal(err)
			}
			units := BuildUnits(src.Files, src.Content, policy.MaxUnitChars)
			var keep []*Unit
			for _, u := range (&Presorter{Policy: policy}).Presort(units, src) {
				if len(u.Hunks) > 0 {
					keep = append(keep, u)
				}
			}
			out = append(out, rpr{fmt.Sprintf("%s#%d", pr.Repo, pr.Number), keep})
		}
	}
	matches, _ := filepath.Glob("../.cache/results/perfectscale__psc-coroot-node-agent__43__*.json")
	sort.Strings(matches)
	if len(matches) > 0 {
		b, err := os.ReadFile(matches[len(matches)-1])
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Files []struct {
				Units []struct {
					ID, File, Symbol string
					Status           FileStatus
					Decision         Decision
					Hunks            []Hunk
				}
			}
		}
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		var keep []*Unit
		for _, f := range r.Files {
			for _, x := range f.Units {
				if len(x.Hunks) == 0 || (x.Decision.Source == "rule" && x.Decision.Bucket == BucketNone) {
					continue
				}
				keep = append(keep, &Unit{ID: x.ID, File: x.File, Symbol: x.Symbol, Status: x.Status, Hunks: x.Hunks})
			}
		}
		out = append(out, rpr{"perfectscale/psc-coroot-node-agent#43", keep})
	}
	if len(out) == 0 {
		t.Skip("no PRs: run experiments/grouping-tuning/fetch.py")
	}
	return out
}

// rctx holds what every strategy and metric needs about one PR.
type rctx struct {
	units    []*Unit
	changed  []int   // +/- lines per unit
	refs     [][]int // refs[i]: units whose declared symbol unit i's new code names
	typeLike []bool  // a type, var, const or class-level declaration
}

var rreCache = map[string]*regexp.Regexp{}

func rmentions(text, name string) bool {
	if len(name) < 3 || !identRe.MatchString(name) {
		return false
	}
	re, ok := rreCache[name]
	if !ok {
		re = regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
		rreCache[name] = re
	}
	return re.MatchString(text)
}

// newSide is the code a reader reads as the change: added and context
// lines. Removed lines name the old code, not what the step depends on.
func newSide(u *Unit) string {
	var sb strings.Builder
	for _, h := range u.Hunks {
		for _, l := range h.Lines {
			if len(l) > 0 && (l[0] == '+' || l[0] == ' ') {
				sb.WriteString(l[1:])
				sb.WriteByte('\n')
			}
		}
	}
	return sb.String()
}

func isTypeLike(u *Unit) bool {
	s := u.Symbol
	if strings.HasSuffix(u.File, ".go") {
		return strings.HasPrefix(s, "type ") || strings.HasPrefix(s, "var ") || strings.HasPrefix(s, "const ")
	}
	// Java, C#, TS: a class-level unit has no member part.
	return s != "" && !strings.Contains(s, ".") && !strings.Contains(s, "(")
}

func newRctx(units []*Unit) *rctx {
	c := &rctx{units: units, changed: make([]int, len(units)), refs: make([][]int, len(units)), typeLike: make([]bool, len(units))}
	text := make([]string, len(units))
	name := make([]string, len(units))
	for i, u := range units {
		a, d := u.Added()
		c.changed[i] = a + d
		text[i] = newSide(u)
		name[i] = shortName(u.Symbol)
		c.typeLike[i] = isTypeLike(u)
	}
	for i := range units {
		for j := range units {
			if i != j && rmentions(text[i], name[j]) {
				c.refs[i] = append(c.refs[i], j)
			}
		}
	}
	return c
}

type rstep struct{ members, included []int }

// ---- strategies ----

func sDecl(c *rctx) []rstep {
	out := make([]rstep, len(c.units))
	for i := range c.units {
		out[i] = rstep{members: []int{i}}
	}
	return out
}

func byFile(c *rctx) [][]int {
	var order []string
	idx := map[string][]int{}
	for i, u := range c.units {
		if _, ok := idx[u.File]; !ok {
			order = append(order, u.File)
		}
		idx[u.File] = append(idx[u.File], i)
	}
	out := make([][]int, len(order))
	for k, f := range order {
		out[k] = idx[f]
	}
	return out
}

func sFile(c *rctx) []rstep {
	var out []rstep
	for _, g := range byFile(c) {
		out = append(out, rstep{members: g})
	}
	return out
}

// sFileCapped splits a file into runs of adjacent units, a new run
// starting when the next unit would push it past limit changed lines.
func sFileCapped(limit int) func(*rctx) []rstep {
	return func(c *rctx) []rstep {
		var out []rstep
		for _, g := range byFile(c) {
			var cur []int
			n := 0
			for _, i := range g {
				if len(cur) > 0 && n+c.changed[i] > limit {
					out = append(out, rstep{members: cur})
					cur, n = nil, 0
				}
				cur = append(cur, i)
				n += c.changed[i]
			}
			if len(cur) > 0 {
				out = append(out, rstep{members: cur})
			}
		}
		return out
	}
}

// withDefs adds, to each step, the changed declarations its members use
// that live in another step, shown for reference. pred picks which kinds.
func withDefs(base func(*rctx) []rstep, pred func(c *rctx, j int) bool) func(*rctx) []rstep {
	return func(c *rctx) []rstep {
		steps := base(c)
		for k := range steps {
			in := map[int]bool{}
			for _, m := range steps[k].members {
				in[m] = true
			}
			seen := map[int]bool{}
			for _, m := range steps[k].members {
				for _, j := range c.refs[m] {
					if !in[j] && !seen[j] && pred(c, j) {
						seen[j] = true
						steps[k].included = append(steps[k].included, j)
					}
				}
			}
			sort.Ints(steps[k].included)
		}
		return steps
	}
}

func anyDef(*rctx, int) bool      { return true }
func typeDef(c *rctx, j int) bool { return c.typeLike[j] }
func fromGroups(c *rctx, gs []*ReviewGroup) []rstep {
	pos := map[*Unit]int{}
	for i, u := range c.units {
		pos[u] = i
	}
	out := make([]rstep, len(gs))
	for k, g := range gs {
		for _, u := range g.Members {
			out[k].members = append(out[k].members, pos[u])
		}
	}
	return out
}

func sLinkShipped(c *rctx) []rstep {
	return fromGroups(c, buildGroups(c.units, defaultParams(DefaultGroupPolicy())))
}

type redge struct{ score, i, j int }

// linkEdges scores unit pairs exactly as buildGroups does.
func linkEdges(c *rctx) []redge {
	p := defaultParams(DefaultGroupPolicy())
	diff := make([]string, len(c.units))
	name := make([]string, len(c.units))
	for i, u := range c.units {
		diff[i], name[i] = u.Diff(), shortName(u.Symbol)
	}
	var out []redge
	for i := range c.units {
		for j := i + 1; j < len(c.units); j++ {
			s := 0
			if rmentions(diff[i], name[j]) {
				s += p.ref
			}
			if rmentions(diff[j], name[i]) {
				s += p.ref
			}
			if testFor(name[i], name[j]) || testFor(name[j], name[i]) {
				s += p.test
			}
			if c.units[i].File == c.units[j].File && len(diff[i])+len(diff[j]) < p.smallPair {
				s += p.sameFile
			}
			if s > 0 {
				out = append(out, redge{s, i, j})
			}
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].score != out[b].score {
			return out[a].score > out[b].score
		}
		if out[a].i != out[b].i {
			return out[a].i < out[b].i
		}
		return out[a].j < out[b].j
	})
	return out
}

// sLinkSeams clusters by the same links, but cuts at seams: build the
// maximum spanning forest with no cap, then split any tree over limit
// changed lines at its weakest edge, preferring the most even split
// among equally weak edges, until every part fits.
func sLinkSeams(limit int) func(*rctx) []rstep {
	return func(c *rctx) []rstep {
		parent := make([]int, len(c.units))
		for i := range parent {
			parent[i] = i
		}
		var find func(int) int
		find = func(x int) int {
			for parent[x] != x {
				parent[x] = parent[parent[x]]
				x = parent[x]
			}
			return x
		}
		var tree []redge
		for _, e := range linkEdges(c) {
			if a, b := find(e.i), find(e.j); a != b {
				parent[a] = b
				tree = append(tree, e)
			}
		}
		comps := map[int][]int{}
		for i := range c.units {
			comps[find(i)] = append(comps[find(i)], i)
		}
		size := func(ns []int) int {
			n := 0
			for _, i := range ns {
				n += c.changed[i]
			}
			return n
		}
		reach := func(start int, es []redge) map[int]bool {
			adj := map[int][]int{}
			for _, e := range es {
				adj[e.i] = append(adj[e.i], e.j)
				adj[e.j] = append(adj[e.j], e.i)
			}
			seen := map[int]bool{start: true}
			q := []int{start}
			for len(q) > 0 {
				x := q[0]
				q = q[1:]
				for _, y := range adj[x] {
					if !seen[y] {
						seen[y] = true
						q = append(q, y)
					}
				}
			}
			return seen
		}
		var split func(ns []int, es []redge) [][]int
		split = func(ns []int, es []redge) [][]int {
			if len(ns) == 1 || size(ns) <= limit {
				return [][]int{ns}
			}
			weakest := es[0].score
			for _, e := range es {
				weakest = min(weakest, e.score)
			}
			best, bestGap := -1, 1<<30
			for k, e := range es {
				if e.score != weakest {
					continue
				}
				rest := append(append([]redge{}, es[:k]...), es[k+1:]...)
				side := reach(e.i, rest)
				a := 0
				for _, n := range ns {
					if side[n] {
						a += c.changed[n]
					}
				}
				if gap := abs(2*a - size(ns)); gap < bestGap {
					best, bestGap = k, gap
				}
			}
			rest := append(append([]redge{}, es[:best]...), es[best+1:]...)
			side := reach(es[best].i, rest)
			var A, B []int
			var eA, eB []redge
			for _, n := range ns {
				if side[n] {
					A = append(A, n)
				} else {
					B = append(B, n)
				}
			}
			for _, e := range rest {
				if side[e.i] {
					eA = append(eA, e)
				} else {
					eB = append(eB, e)
				}
			}
			return append(split(A, eA), split(B, eB)...)
		}
		var groups [][]int
		for _, ns := range comps {
			var es []redge
			in := map[int]bool{}
			for _, n := range ns {
				in[n] = true
			}
			for _, e := range tree {
				if in[e.i] {
					es = append(es, e)
				}
			}
			groups = append(groups, split(ns, es)...)
		}
		for _, g := range groups {
			sort.Ints(g)
		}
		sort.Slice(groups, func(a, b int) bool { return groups[a][0] < groups[b][0] })
		out := make([]rstep, len(groups))
		for k, g := range groups {
			out[k] = rstep{members: g}
		}
		return out
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// ---- measurement ----

type rstepOut struct {
	Members     []string `json:"members"`
	Included    []string `json:"included,omitempty"`
	Changed     int      `json:"changed"`      // +/- lines the reader reads, members and included
	Own         int      `json:"own"`          // of those, the step's own changes
	Ref         int      `json:"ref"`          // of those, included definitions
	MemberFiles int      `json:"member_files"` // files the step's own changes span
	Files       int      `json:"files"`        // including files of included definitions
	Targets     int      `json:"targets"`      // distinct changed declarations the members name
	Dangling    int      `json:"dangling"`     // of those, not visible in this step
}

type rrow struct {
	PR           string     `json:"pr"`
	Strategy     string     `json:"strategy"`
	TotalChanged int        `json:"total_changed"`
	Steps        []rstepOut `json:"steps"`
}

func measureSteps(c *rctx, steps []rstep) []rstepOut {
	out := make([]rstepOut, len(steps))
	for k, s := range steps {
		vis := map[int]bool{}
		mf := map[string]bool{}
		af := map[string]bool{}
		o := rstepOut{}
		for _, m := range s.members {
			vis[m] = true
			mf[c.units[m].File] = true
			af[c.units[m].File] = true
			o.Members = append(o.Members, c.units[m].ID)
			o.Changed += c.changed[m]
			o.Own += c.changed[m]
		}
		for _, j := range s.included {
			vis[j] = true
			af[c.units[j].File] = true
			o.Included = append(o.Included, c.units[j].ID)
			o.Changed += c.changed[j]
			o.Ref += c.changed[j]
		}
		targets := map[int]bool{}
		for _, m := range s.members {
			for _, j := range c.refs[m] {
				targets[j] = true
			}
		}
		o.Targets = len(targets)
		for j := range targets {
			if !vis[j] {
				o.Dangling++
			}
		}
		o.MemberFiles, o.Files = len(mf), len(af)
		out[k] = o
	}
	return out
}

func TestGroupingReadability(t *testing.T) {
	if os.Getenv("PR_READABILITY") == "" {
		t.Skip("set PR_READABILITY=1 to run the readability exploration")
	}
	strategies := []struct {
		name string
		fn   func(*rctx) []rstep
	}{
		{"decl (today)", sDecl},
		{"file", sFile},
		{"file-300", sFileCapped(300)},
		{"file+types", withDefs(sFile, typeDef)},
		{"file+defs", withDefs(sFile, anyDef)},
		{"file-300+defs", withDefs(sFileCapped(300), anyDef)},
		{"link (shipped)", sLinkShipped},
		{"link-seams-300", sLinkSeams(300)},
		{"link-seams-300+defs", withDefs(sLinkSeams(300), anyDef)},
	}
	var rows []rrow
	for _, pr := range loadReadabilityPRs(t) {
		c := newRctx(pr.units)
		total := 0
		for _, n := range c.changed {
			total += n
		}
		t.Logf("%s: %d units, %d changed lines", pr.name, len(pr.units), total)
		for _, s := range strategies {
			rows = append(rows, rrow{PR: pr.name, Strategy: s.name, TotalChanged: total, Steps: measureSteps(c, s.fn(c))})
		}
	}
	dir := filepath.Join(readabilityDir, "results")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(rows, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "steps.json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d strategy runs to %s/steps.json", len(rows), dir)
}
