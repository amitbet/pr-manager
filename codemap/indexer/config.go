package indexer

import (
	_ "embed"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/amitbet/pr-manager/codemap"
	"github.com/amitbet/pr-manager/codemap/githist"
)

type Config struct {
	Output string `yaml:"output"`
	Cache  string `yaml:"cache"`

	Rank struct {
		Damping          float64           `yaml:"damping"`
		VirtualConsumers []VirtualConsumer `yaml:"virtual_consumers"`
	} `yaml:"rank"`

	Impact struct {
		RankWeight     float64        `yaml:"rank_weight"`
		RollbackWeight float64        `yaml:"rollback_weight"`
		Levels         map[string]int `yaml:"levels"`
	} `yaml:"impact"`

	History struct {
		Days         int    `yaml:"days"`
		HalfLifeDays int    `yaml:"half_life_days"`
		BulkFiles    int    `yaml:"bulk_files"`
		Fix          string `yaml:"fix"`
		NotFix       string `yaml:"not_fix"`
		Revert       string `yaml:"revert"`
		CoChange     struct {
			MinCommits int     `yaml:"min_commits"`
			MinConf    float64 `yaml:"min_conf"`
			Max        int     `yaml:"max"`
		} `yaml:"cochange"`
	} `yaml:"history"`

	// Likelihood point rules; unset fields keep codemap.DefaultLikelihoodWeights.
	Likelihood codemap.LikelihoodWeights `yaml:"likelihood"`

	Symbols struct {
		// A symbol gets its own record when it has at least this many callers
		// outside its file, or when it carries a rollback tag >= min_tag_score.
		// Everything else falls back to its file record at lookup time.
		MinExternalCallers int  `yaml:"min_external_callers"`
		MinTagScore        int  `yaml:"min_tag_score"`
		EmitExported       bool `yaml:"emit_exported"`
	} `yaml:"symbols"`

	Contracts struct {
		// Extra client -> spec bindings the heuristics cannot infer.
		GeneratedClients []GeneratedClient `yaml:"generated_clients"`
	} `yaml:"contracts"`

	Rollback struct {
		Default   int        `yaml:"default"`
		MultiTag  int        `yaml:"multi_tag_bonus"`
		Rules     []RuleSpec `yaml:"rules"`
		Sinks     []SinkSpec `yaml:"sinks"`
		Propagate struct {
			MaxHops int     `yaml:"max_hops"`
			Decay   float64 `yaml:"decay"`
		} `yaml:"propagate"`
	} `yaml:"rollback"`

	rules []*Rule
	sinks []*Sink
	hist  githist.Config
}

type VirtualConsumer struct {
	Name   string   `yaml:"name"`
	Weight float64  `yaml:"weight"` // share of total teleport mass
	Specs  []string `yaml:"specs"`  // globs on "<repo>/<spec path>"
}

type GeneratedClient struct {
	Repo  string `yaml:"repo"`
	Dir   string `yaml:"dir"`   // client code dir in Repo
	Spec  string `yaml:"spec"`  // "<repo>/<spec path>"
	Style string `yaml:"style"` // "orval" (camelCase op functions) | "oapi-codegen"
}

type RuleSpec struct {
	ID     string    `yaml:"id"`
	Score  int       `yaml:"score"`
	Cap    *int      `yaml:"impact_cap"`
	Floor  int       `yaml:"impact_floor"`
	Reason string    `yaml:"reason"`
	Match  MatchSpec `yaml:"match"`
}

type MatchSpec struct {
	Path     []string `yaml:"path"`
	NotPath  []string `yaml:"not_path"`
	Repo     []string `yaml:"repo"`
	Category []string `yaml:"category"`
	Kind     []string `yaml:"kind"`
	Tags     []string `yaml:"tags"`
	Sym      string   `yaml:"sym"`
	Exported *bool    `yaml:"exported"`
}

type SinkSpec struct {
	ID     string   `yaml:"id"`
	Score  int      `yaml:"score"`
	Reason string   `yaml:"reason"`
	Callee []string `yaml:"callee"`
	// Only count calls made from these repo categories (empty = any).
	Category []string `yaml:"category"`
}

type Rule struct {
	RuleSpec
	path, notPath []*regexp.Regexp
	sym           *regexp.Regexp
}

type Sink struct {
	SinkSpec
	re []*regexp.Regexp
}

//go:embed default.yaml
var defaultConfig []byte

func loadConfig(p string) (*Config, error) {
	b := defaultConfig
	var err error
	if p != "" {
		b, err = os.ReadFile(p)
		if err != nil {
			return nil, err
		}
	}
	c := Config{Likelihood: codemap.DefaultLikelihoodWeights()}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if p != "" {
		if err := fillFromDefault(&c, b); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	if c.Rank.Damping == 0 {
		c.Rank.Damping = 0.85
	}
	if c.Rank.Damping <= 0 || c.Rank.Damping >= 1 {
		return nil, fmt.Errorf("rank.damping must be between 0 and 1 (exclusive), got %v", c.Rank.Damping)
	}
	if c.Impact.RankWeight == 0 && c.Impact.RollbackWeight == 0 {
		c.Impact.RankWeight, c.Impact.RollbackWeight = 0.55, 0.45
	}
	c.hist = githist.DefaultConfig()
	h := &c.History
	if h.Days > 0 {
		c.hist.Days = h.Days
	}
	if h.HalfLifeDays > 0 {
		c.hist.HalfLife = h.HalfLifeDays
	}
	if h.BulkFiles > 0 {
		c.hist.Bulk = h.BulkFiles
	}
	for _, re := range []struct {
		src string
		dst **regexp.Regexp
	}{{h.Fix, &c.hist.Fix}, {h.NotFix, &c.hist.NotFix}, {h.Revert, &c.hist.Revert}} {
		if re.src == "" {
			continue
		}
		if *re.dst, err = regexp.Compile(re.src); err != nil {
			return nil, fmt.Errorf("history: %w", err)
		}
	}
	h.Days = c.hist.Days
	if h.CoChange.MinCommits == 0 {
		h.CoChange.MinCommits = 3
	}
	if h.CoChange.MinConf == 0 {
		h.CoChange.MinConf = 0.3
	}
	if h.CoChange.Max == 0 {
		h.CoChange.Max = 5
	}
	if c.Rollback.Propagate.MaxHops == 0 {
		c.Rollback.Propagate.MaxHops = 2
	}
	for _, rs := range c.Rollback.Rules {
		r := &Rule{RuleSpec: rs}
		for _, g := range rs.Match.Path {
			r.path = append(r.path, globRe(g))
		}
		for _, g := range rs.Match.NotPath {
			r.notPath = append(r.notPath, globRe(g))
		}
		if rs.Match.Sym != "" {
			if r.sym, err = regexp.Compile(rs.Match.Sym); err != nil {
				return nil, fmt.Errorf("rule %s: %w", rs.ID, err)
			}
		}
		c.rules = append(c.rules, r)
	}
	for _, ss := range c.Rollback.Sinks {
		s := &Sink{SinkSpec: ss}
		for _, pat := range ss.Callee {
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, fmt.Errorf("sink %s: %w", ss.ID, err)
			}
			s.re = append(s.re, re)
		}
		c.sinks = append(c.sinks, s)
	}
	return &c, nil
}

// fillFromDefault completes a user config (raw is its YAML) from the
// embedded default: impact levels it leaves out, and whole top-level
// sections it omits. Without levels every record would rate critical.
func fillFromDefault(c *Config, raw []byte) error {
	var def Config
	if err := yaml.Unmarshal(defaultConfig, &def); err != nil {
		return fmt.Errorf("default config: %w", err)
	}
	var top map[string]any
	if err := yaml.Unmarshal(raw, &top); err != nil {
		return err
	}
	if c.Output == "" {
		c.Output = def.Output
	}
	if c.Cache == "" {
		c.Cache = def.Cache
	}
	if c.Impact.Levels == nil {
		c.Impact.Levels = map[string]int{}
	}
	for k, v := range def.Impact.Levels {
		if _, ok := c.Impact.Levels[k]; !ok {
			c.Impact.Levels[k] = v
		}
	}
	if _, ok := top["symbols"]; !ok {
		c.Symbols = def.Symbols
	}
	if _, ok := top["rollback"]; !ok {
		c.Rollback = def.Rollback
	}
	return nil
}

func globRe(g string) *regexp.Regexp { return codemap.GlobRe(g) }

func anyMatch(res []*regexp.Regexp, s string) bool {
	for _, r := range res {
		if r.MatchString(s) {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Subject is what a rule is evaluated against: a symbol, a file, or a
// directory. Symbol-only conditions (kind, tags, sym, exported) never match
// files or directories.
type Subject struct {
	Repo, Category, Path string
	IsSymbol             bool
	Kind, Sym            string
	Exported             bool
	Tags                 []string
}

func (r *Rule) matches(s Subject) bool {
	m := r.Match
	if len(m.Repo) > 0 && !contains(m.Repo, s.Repo) {
		return false
	}
	if len(m.Category) > 0 && !contains(m.Category, s.Category) {
		return false
	}
	if len(r.path) > 0 && !anyMatch(r.path, s.Path) {
		return false
	}
	if len(r.notPath) > 0 && anyMatch(r.notPath, s.Path) {
		return false
	}
	symOnly := len(m.Kind) > 0 || len(m.Tags) > 0 || r.sym != nil || m.Exported != nil
	if symOnly && !s.IsSymbol {
		return false
	}
	if len(m.Kind) > 0 && !contains(m.Kind, s.Kind) {
		return false
	}
	if len(m.Tags) > 0 {
		ok := false
		for _, t := range m.Tags {
			if contains(s.Tags, t) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if r.sym != nil && !r.sym.MatchString(s.Sym) {
		return false
	}
	if m.Exported != nil && *m.Exported != s.Exported {
		return false
	}
	return true
}
