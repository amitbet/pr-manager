package codemap

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/amitbet/pr-manager/codemap/decls"
)

// Map is a loaded codemap directory. Repo shards load lazily.
type Map struct {
	Dir   string
	Meta  Meta
	Repos map[string]Record

	mu     sync.Mutex
	shards map[string]*shard
	rules  []PathRule
	warned map[string]bool // repos whose shard failed to read, logged once
}

type shard struct {
	byID    map[string]*Record
	symbols map[string][]*Record // file path -> symbol records
	bySym   map[string][]*Record // file path + "\x00" + NormSym -> records
}

func Open(dir string) (*Map, error) {
	m := &Map{Dir: dir, Repos: map[string]Record{}, shards: map[string]*shard{}}
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m.Meta); err != nil {
		return nil, fmt.Errorf("meta.json: %w", err)
	}
	m.rules = m.Meta.Rules
	for i := range m.rules {
		m.rules[i].compile()
	}
	recs, err := readJSONL(filepath.Join(dir, "repos.jsonl"))
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		m.Repos[r.Repo] = *r
	}
	return m, nil
}

func readJSONL(p string) ([]*Record, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []*Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return out, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, &r)
	}
	return out, sc.Err()
}

func (m *Map) shard(repo string) *shard {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.shards[repo]; ok {
		return s
	}
	s := &shard{byID: map[string]*Record{}, symbols: map[string][]*Record{}, bySym: map[string][]*Record{}}
	recs, err := readJSONL(filepath.Join(m.Dir, repo+".jsonl"))
	for _, r := range recs {
		s.byID[r.ID] = r
		if r.Level == "symbol" {
			s.symbols[r.Path] = append(s.symbols[r.Path], r)
			k := r.Path + "\x00" + NormSym(r.Sym)
			s.bySym[k] = append(s.bySym[k], r)
		}
	}
	// A missing shard is a repo the map doesn't cover. Any other error
	// leaves a partial shard: use it, but read again next time rather than
	// caching it for good.
	if err != nil && !os.IsNotExist(err) {
		if !m.warned[repo] {
			if m.warned == nil {
				m.warned = map[string]bool{}
			}
			m.warned[repo] = true
			log.Printf("codemap: reading %s shard: %v", repo, err)
		}
		return s
	}
	m.shards[repo] = s
	return s
}

// Query identifies code to assess. Path is repo-relative. Sym uses the
// pr-manager unit format: Func, (*T).M, type T, var X, const X. Lines are
// on the indexed (base) side; 0 means unknown.
type Query struct {
	Repo      string `json:"repo"`
	Path      string `json:"path"`
	Sym       string `json:"sym,omitempty"`
	StartLine int    `json:"start,omitempty"`
	EndLine   int    `json:"end,omitempty"`
	// NewFile marks a file the change adds. A map built after the change
	// may already index it, but nothing depended on it before, so its own
	// records are skipped and the estimate comes from its directory.
	NewFile bool `json:"new_file,omitempty"`
}

// Result is the assessment for one query.
type Result struct {
	Query Query `json:"query"`
	// Matched is the level that answered: symbol | file | dir | repo | none.
	Matched string `json:"matched"`
	// Basis is the most specific record(s) found; for line queries that
	// span several symbols, all of them, highest impact first.
	Basis []*Record `json:"basis,omitempty"`
	// Chain is the fallback hierarchy above Basis: file, dirs..., repo.
	Chain []*Record `json:"chain,omitempty"`

	Impact      int      `json:"impact"`
	ImpactLevel string   `json:"impact_level"`
	Rollback    int      `json:"rollback"`
	PathRules   []Tag    `json:"path_rules,omitempty"`
	Notes       []string `json:"notes,omitempty"`
}

func (m *Map) pathRules(repo, p string) ([]Tag, *int, int) {
	cat := m.Meta.Repos[repo].Category
	var tags []Tag
	var cp *int
	floor := 0
	for i := range m.rules {
		r := &m.rules[i]
		if r.Matches(repo, cat, p) {
			if r.Score > 0 || r.Cap == nil {
				tags = append(tags, Tag{ID: r.ID, Score: r.Score})
			}
			if r.Cap != nil && (cp == nil || *r.Cap < *cp) {
				v := *r.Cap
				cp = &v
			}
			floor = max(floor, r.Floor)
		}
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].Score > tags[j].Score })
	return tags, cp, floor
}

func (m *Map) level(d int) string {
	switch {
	case d >= m.Meta.Levels["critical"]:
		return "critical"
	case d >= m.Meta.Levels["high"]:
		return "high"
	case d >= m.Meta.Levels["medium"]:
		return "medium"
	}
	return "low"
}

// Lookup resolves a query through symbol -> file -> dir -> repo.
func (m *Map) Lookup(q Query) Result {
	q.Path = strings.TrimPrefix(path.Clean("/"+q.Path), "/")
	res := Result{Query: q, Matched: "none"}
	if _, ok := m.Meta.Repos[q.Repo]; !ok {
		res.Notes = append(res.Notes, "repo not indexed: "+q.Repo)
		res.ImpactLevel = "low"
		return res
	}
	s := m.shard(q.Repo)
	file := s.byID[q.Repo+"/"+q.Path]
	if q.NewFile {
		file, q.Sym, q.StartLine, q.EndLine = nil, "", 0, 0
	}

	// Symbol by name first (robust to line drift), then by line overlap.
	if q.Sym != "" {
		sym := NormSym(q.Sym)
		recs := s.bySym[q.Path+"\x00"+sym]
		// A TS class member without its own record (only its class and
		// this.m reach it) counts as its class.
		if len(recs) == 0 && decls.IsTSPath(q.Path) {
			if i := strings.LastIndexByte(sym, '.'); i > 0 {
				if recs = s.bySym[q.Path+"\x00"+sym[:i]]; len(recs) > 0 {
					res.Notes = append(res.Notes, "member not in map; using its class "+sym[:i])
				}
			}
		}
		if len(recs) > 0 {
			res.Basis = append(res.Basis, recs...)
			res.Matched = "symbol"
		} else if file != nil {
			res.Notes = append(res.Notes, "symbol not in map (below emit threshold or new); using file")
		}
	}
	if res.Matched == "none" && q.StartLine > 0 {
		end := q.EndLine
		if end < q.StartLine {
			end = q.StartLine
		}
		for _, r := range s.symbols[q.Path] {
			if len(r.Lines) == 2 && r.Lines[0] <= end && r.Lines[1] >= q.StartLine {
				res.Basis = append(res.Basis, r)
			}
		}
		if len(res.Basis) > 0 {
			res.Matched = "symbol"
			// Enclosing ranges (e.g. a type and its doc block) can overlap;
			// keep the highest impact first.
			sort.Slice(res.Basis, func(i, j int) bool { return res.Basis[i].Impact > res.Basis[j].Impact })
		}
	}
	if file != nil {
		if res.Matched == "none" {
			res.Matched = "file"
			res.Basis = []*Record{file}
		} else {
			res.Chain = append(res.Chain, file)
		}
	}
	dir := path.Dir(q.Path)
	if dir == "." {
		dir = ""
	}
	for {
		if dir != "" {
			if d := s.byID[q.Repo+"/"+dir+"/"]; d != nil {
				if res.Matched == "none" {
					res.Matched = "dir"
					res.Basis = []*Record{d}
				} else {
					res.Chain = append(res.Chain, d)
				}
			}
		}
		if dir == "" {
			break
		}
		dir = path.Dir(dir)
		if dir == "." {
			dir = ""
		}
	}
	if repo, ok := m.Repos[q.Repo]; ok {
		r := repo
		if res.Matched == "none" {
			res.Matched = "repo"
			res.Basis = []*Record{&r}
		} else {
			res.Chain = append(res.Chain, &r)
		}
	}

	tags, cp, floor := m.pathRules(q.Repo, q.Path)
	res.PathRules = tags
	if len(res.Basis) == 0 {
		// Nothing in the map covers this repo: only the path rules speak.
		res.Impact = floor
		if cp != nil && res.Impact > *cp {
			res.Impact = *cp
		}
		res.Notes = append(res.Notes, "repo not in map; estimated from path rules")
		res.ImpactLevel = m.level(res.Impact)
		return res
	}
	top := res.Basis[0]
	switch res.Matched {
	case "symbol", "file":
		res.Impact, res.Rollback = top.Impact, top.Rollback
		if res.Matched == "file" && q.Sym == "" && q.StartLine > 0 {
			res.Notes = append(res.Notes, "lines do not overlap an indexed symbol; using file")
		}
	default:
		// File is not in the map: new, a test, or an asset. New code has no
		// dependents yet, so only half the ancestor's rank carries over;
		// rollback comes from the path rules and the ancestor.
		if q.NewFile {
			res.Notes = append(res.Notes, "new file; estimated from "+top.ID+" and path rules")
		} else {
			res.Notes = append(res.Notes, "file not in map; estimated from "+top.ID+" and path rules")
		}
		rb := top.Rollback
		if cp != nil {
			rb = 0 // tests/docs: the ancestor's contracts do not apply
		}
		for _, t := range tags {
			if t.Score > rb {
				rb = t.Score
			}
		}
		d := int(math.Round(m.Meta.Weights.Rank*top.Rank*0.5 + m.Meta.Weights.Rollback*float64(rb)))
		res.Impact, res.Rollback = max(d, floor), rb
	}
	if cp == nil {
		cp = top.ImpactCap
	}
	if cp != nil && res.Impact > *cp {
		res.Impact = *cp
		res.Notes = append(res.Notes, fmt.Sprintf("impact capped at %d by path rule", *cp))
	}
	res.ImpactLevel = m.level(res.Impact)
	return res
}

// ParseTarget accepts "repo/path", "repo/path:Sym", "repo/path:12" or
// "repo/path:12-40". A leading "code/" is stripped.
func ParseTarget(t string) Query {
	t = strings.TrimPrefix(t, "code/")
	var q Query
	repo, rest, _ := strings.Cut(t, "/")
	q.Repo = repo
	p, suffix, has := strings.Cut(rest, ":")
	q.Path = p
	if has {
		a, b, rng := strings.Cut(suffix, "-")
		if n, err := strconv.Atoi(a); err == nil {
			q.StartLine = n
			q.EndLine = n
			if rng {
				if m, err := strconv.Atoi(b); err == nil {
					q.EndLine = m
				}
			}
		} else {
			q.Sym = suffix
		}
	}
	return q
}

// Hunk is one diff hunk mapped onto the base side.
type Hunk struct {
	OldPath  string `json:"old_path"`
	NewPath  string `json:"new_path"`
	OldStart int    `json:"old_start"`
	OldLines int    `json:"old_lines"`
	NewStart int    `json:"new_start"`
	NewLines int    `json:"new_lines"`
	Header   string `json:"header,omitempty"`
}

// ParseDiff reads a unified diff (git diff format). Inside a hunk, lines
// are counted off its header, so a removed "-- comment" line is not taken
// for a "--- path" header.
func ParseDiff(r io.Reader) ([]Hunk, error) {
	var out []Hunk
	var oldP, newP string
	var remOld, remNew int // lines of the current hunk still to come
	br := bufio.NewReader(r)
	for {
		l, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return out, err
		}
		if l == "" && err == io.EOF {
			break
		}
		l = strings.TrimSuffix(strings.TrimSuffix(l, "\n"), "\r")
		if remOld > 0 || remNew > 0 {
			switch {
			case strings.HasPrefix(l, "-"):
				remOld--
				continue
			case strings.HasPrefix(l, "+"):
				remNew--
				continue
			case strings.HasPrefix(l, " "), l == "":
				remOld--
				remNew--
				continue
			case strings.HasPrefix(l, "\\"):
				continue
			}
			remOld, remNew = 0, 0 // a short hunk: read l as a header
		}
		switch {
		case strings.HasPrefix(l, "diff --git "):
			oldP, newP = "", ""
			parts := strings.Fields(l)
			if len(parts) >= 4 {
				oldP = strings.TrimPrefix(parts[2], "a/")
				newP = strings.TrimPrefix(parts[3], "b/")
			}
		case strings.HasPrefix(l, "--- "):
			p := diffHeaderPath(strings.TrimPrefix(l, "--- "))
			if p == "/dev/null" {
				oldP = ""
			} else {
				oldP = strings.TrimPrefix(p, "a/")
			}
		case strings.HasPrefix(l, "+++ "):
			p := diffHeaderPath(strings.TrimPrefix(l, "+++ "))
			if p == "/dev/null" {
				newP = ""
			} else {
				newP = strings.TrimPrefix(p, "b/")
			}
		case strings.HasPrefix(l, "@@ "):
			h := Hunk{OldPath: oldP, NewPath: newP}
			end := strings.Index(l[3:], " @@")
			if end < 0 {
				continue
			}
			spec := l[3 : 3+end]
			h.Header = strings.TrimSpace(l[3+end+3:])
			for _, f := range strings.Fields(spec) {
				a, b, _ := strings.Cut(f[1:], ",")
				start, _ := strconv.Atoi(a)
				n := 1
				if b != "" {
					n, _ = strconv.Atoi(b)
				}
				if f[0] == '-' {
					h.OldStart, h.OldLines = start, n
				} else {
					h.NewStart, h.NewLines = start, n
				}
			}
			remOld, remNew = h.OldLines, h.NewLines
			out = append(out, h)
		}
		if err == io.EOF {
			break
		}
	}
	return out, nil
}

// diffHeaderPath is the path of a ---/+++ line: C-quoted by git for
// control characters and quotes, and followed by a tab and a timestamp
// (or git's lone tab after a name with spaces).
func diffHeaderPath(s string) string {
	if strings.HasPrefix(s, `"`) {
		if i := strings.LastIndex(s, `"`); i > 0 {
			if u, err := strconv.Unquote(s[:i+1]); err == nil {
				return u
			}
		}
	}
	s, _, _ = strings.Cut(s, "\t")
	return strings.TrimSpace(s)
}

// DiffReport is the assessment for a whole diff.
type DiffReport struct {
	Repo        string       `json:"repo"`
	Impact      int          `json:"impact"`
	ImpactLevel string       `json:"impact_level"`
	Hunks       []HunkResult `json:"hunks"`
}

type HunkResult struct {
	Hunk   Hunk   `json:"hunk"`
	Result Result `json:"result"`
}

// LookupDiff assesses each hunk. Hunks are matched on the old (base) side,
// which is what the map indexed; added files use the new path.
func (m *Map) LookupDiff(repo string, hunks []Hunk) DiffReport {
	rep := DiffReport{Repo: repo}
	for _, h := range hunks {
		q := Query{Repo: repo}
		if h.OldPath != "" {
			q.Path = h.OldPath
			q.StartLine = h.OldStart
			q.EndLine = h.OldStart + max(h.OldLines, 1) - 1
			if h.OldLines == 0 {
				// pure insertion after OldStart
				q.EndLine = h.OldStart + 1
			}
		} else {
			q.Path, q.NewFile = h.NewPath, true
		}
		// A leading code/<repo>/ prefix means a workspace-relative diff.
		if strings.HasPrefix(q.Path, "code/") {
			q2 := ParseTarget(q.Path)
			q.Repo, q.Path = q2.Repo, q2.Path
		}
		r := m.Lookup(q)
		rep.Hunks = append(rep.Hunks, HunkResult{Hunk: h, Result: r})
		if r.Impact > rep.Impact {
			rep.Impact = r.Impact
		}
	}
	rep.ImpactLevel = m.level(rep.Impact)
	return rep
}

// ReadShard returns every record of one repo shard.
func ReadShard(dir, repo string) ([]*Record, error) {
	return readJSONL(filepath.Join(dir, repo+".jsonl"))
}
