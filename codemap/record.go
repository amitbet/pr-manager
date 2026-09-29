// Package codemap reads the codemap files and answers two questions for a
// repo path, symbol, line range, or unified diff: how much damage a bad
// change there can do (impact) and how likely a change there is to go wrong
// (likelihood, from the code's history and complexity).
//
// It has no dependencies outside the standard library so a PR bot can copy
// or vendor it.
package codemap

import (
	"regexp"
	"strings"
)

// Record is one line of a codemap shard (<repo>.jsonl) or repos.jsonl.
//
// IDs are hierarchical:
//
//	repo    settings
//	dir     settings/api/v1/user/
//	file    settings/api/v1/user/server.go
//	symbol  settings/api/v1/user/server.go:(*Server).GetProfiles
//	api op  settings/api/v1/user/api.yaml:GetProfiles
type Record struct {
	ID    string `json:"id"`
	Level string `json:"level"` // repo | dir | file | symbol
	Repo  string `json:"repo"`
	Path  string `json:"path,omitempty"` // repo-relative file or dir
	Sym   string `json:"sym,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Lines []int  `json:"lines,omitempty"` // [start, end] at the indexed commit

	Impact      int    `json:"impact"`       // 0-100, blend of rank and rollback
	ImpactLevel string `json:"impact_level"` // low | medium | high | critical

	// Likelihood is 0-100 from the code's own history and complexity (see
	// PlaceLikelihood). A PR unit adds change factors on top.
	Likelihood      int         `json:"likelihood"`
	LikelihoodLevel string      `json:"likelihood_level"`
	Hist            *History    `json:"hist,omitempty"`
	Cx              *Complexity `json:"cx,omitempty"`
	// CoChange lists files that usually change in the same commit as this
	// one (files only).
	CoChange []Partner `json:"cochange,omitempty"`

	// Rank is the PageRank percentile among records of the same level across
	// the whole workspace (0 = nothing depends on it, 100 = most central).
	Rank       float64 `json:"rank"`
	RankInRepo float64 `json:"rank_in_repo"`
	// PR is raw PageRank scaled so 1.0 is the average node of this level.
	PR         float64 `json:"pr"`
	RankSource string  `json:"rank_source,omitempty"` // graph | inherited | children

	// Rollback is how hard it is to undo a bad change here (0 = redeploy
	// fixes it, 100 = data/contract damage survives the rollback).
	Rollback     int   `json:"rollback"`
	RollbackTags []Tag `json:"rollback_tags,omitempty"`

	// Direct users (interface and contract hops are skipped through).
	Callers     int      `json:"callers"`
	CallerFiles int      `json:"caller_files"`
	CallerRepos []string `json:"caller_repos,omitempty"`
	// Transitive users: everything that can reach this code.
	DepSyms  int      `json:"dep_syms"`
	DepFiles int      `json:"dep_files"`
	DepRepos []string `json:"dep_repos,omitempty"`

	TopCallers []string `json:"top_callers,omitempty"`
	Exported   bool     `json:"exported,omitempty"`
	Generated  bool     `json:"generated,omitempty"`
	Facts      []string `json:"facts,omitempty"`
	Symbols    int      `json:"symbols,omitempty"` // file/dir: symbols inside
	ImpactCap  *int     `json:"impact_cap,omitempty"`
}

// History is what git says about a file (or everything under a directory)
// within the map's history window.
type History struct {
	Commits int `json:"commits"`
	Fixes   int `json:"fixes"`   // commits whose subject reads like a bug fix
	Reverts int `json:"reverts"` // revert commits
	Authors int `json:"authors"` // distinct author emails
	// Recent and RecentFixes weight each commit by 0.5^(age/half-life), so
	// last month's fix counts about 1 and a fix from a year ago about 0.25.
	Recent      float64 `json:"recent"`
	RecentFixes float64 `json:"recent_fixes"`
	AgeDays     int     `json:"age_days"` // since the last change, at the indexed commit
}

// Complexity of a symbol, or the worst function in a file or directory.
type Complexity struct {
	Cyclo int `json:"cyclo"`           // cyclomatic complexity (max for files/dirs)
	Nest  int `json:"nest"`            // deepest control-flow nesting
	Total int `json:"total,omitempty"` // files/dirs: sum over functions
	Funcs int `json:"funcs,omitempty"` // files/dirs: functions measured
}

// Partner is a file that changed together with another in at least N
// commits; Conf is N over the other file's commits.
type Partner struct {
	Path string  `json:"path"`
	N    int     `json:"n"`
	Conf float64 `json:"conf"`
}

type Tag struct {
	ID    string `json:"id"`
	Score int    `json:"s"`
	Via   int    `json:"via,omitempty"` // call hops to the sink, 0 = direct
}

// Meta is codemap/meta.json.
type Meta struct {
	Version     int                 `json:"version"`
	GeneratedAt string              `json:"generated_at"`
	Repos       map[string]RepoMeta `json:"repos"`
	Counts      map[string]int      `json:"counts"`
	Weights     Weights             `json:"weights"`
	Likelihood  LikelihoodWeights   `json:"likelihood"`
	HistoryDays int                 `json:"history_days"`
	Levels      map[string]int      `json:"levels"`
	Rules       []PathRule          `json:"path_rules"`
	Reasons     map[string]string   `json:"reasons"`
	Stats       map[string]any      `json:"stats,omitempty"`
}

type RepoMeta struct {
	Commit   string `json:"commit"`
	Dirty    bool   `json:"dirty"`
	Category string `json:"category"`
	// Remote is the repo's identity from its origin remote (RepoID),
	// empty for a checkout without one or a map built before it was kept.
	Remote string `json:"remote,omitempty"`
}

type Weights struct {
	Rank     float64 `json:"rank"`
	Rollback float64 `json:"rollback"`
}

// PathRule is a rollback rule that only depends on repo/path, exported so
// lookups can classify files that are not in the map yet (new files, tests).
type PathRule struct {
	ID       string   `json:"id"`
	Score    int      `json:"score"`
	Cap      *int     `json:"impact_cap,omitempty"`
	Floor    int      `json:"impact_floor,omitempty"`
	Path     []string `json:"path,omitempty"`
	NotPath  []string `json:"not_path,omitempty"`
	Repo     []string `json:"repo,omitempty"`
	Category []string `json:"category,omitempty"`

	path, notPath []*regexp.Regexp
}

func (r *PathRule) compile() {
	r.path, r.notPath = nil, nil
	for _, g := range r.Path {
		r.path = append(r.path, GlobRe(g))
	}
	for _, g := range r.NotPath {
		r.notPath = append(r.notPath, GlobRe(g))
	}
}

func (r *PathRule) Matches(repo, category, p string) bool {
	if len(r.Repo) > 0 && !has(r.Repo, repo) {
		return false
	}
	if len(r.Category) > 0 && !has(r.Category, category) {
		return false
	}
	if len(r.path) > 0 && !matchAny(r.path, p) {
		return false
	}
	if len(r.notPath) > 0 && matchAny(r.notPath, p) {
		return false
	}
	return true
}

func has(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func matchAny(res []*regexp.Regexp, s string) bool {
	for _, r := range res {
		if r.MatchString(s) {
			return true
		}
	}
	return false
}

// GlobRe converts a path glob to an anchored regexp. "**/" matches zero or
// more directories, "*" stays within one segment, "{a,b}" is alternation.
func GlobRe(g string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch {
		case c == '*' && i+1 < len(g) && g[i+1] == '*':
			if i+2 < len(g) && g[i+2] == '/' {
				b.WriteString("(?:.*/)?")
				i += 2
			} else {
				b.WriteString(".*")
				i++
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '{':
			j := strings.IndexByte(g[i:], '}')
			if j < 0 {
				b.WriteString(regexp.QuoteMeta(string(c)))
				continue
			}
			alts := strings.Split(g[i+1:i+j], ",")
			for k := range alts {
				alts[k] = regexp.QuoteMeta(alts[k])
			}
			b.WriteString("(?:" + strings.Join(alts, "|") + ")")
			i += j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// NormSym makes Go receiver spellings comparable: "(*T).M" == "(T).M" == "T.M".
func NormSym(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "(") {
		if i := strings.Index(s, ")."); i > 0 {
			s = strings.TrimPrefix(s[1:i], "*") + "." + s[i+2:]
		}
	}
	return s
}
