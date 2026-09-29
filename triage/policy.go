package triage

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type Bucket string

const (
	BucketHuman Bucket = "human"
	BucketSkim  Bucket = "skim"
	BucketNone  Bucket = "none"
)

// rank orders buckets by review effort; escalation only ever moves up.
func (b Bucket) rank() int {
	switch b {
	case BucketNone:
		return 0
	case BucketSkim:
		return 1
	default:
		return 2
	}
}

func (b Bucket) Valid() bool {
	return b == BucketHuman || b == BucketSkim || b == BucketNone
}

// Up returns the next bucket up.
func (b Bucket) Up() Bucket {
	if b == BucketNone {
		return BucketSkim
	}
	return BucketHuman
}

func maxBucket(a, b Bucket) Bucket {
	if a.rank() >= b.rank() {
		return a
	}
	return b
}

type Thresholds struct {
	// Minimum confidence to accept a "none" / "skim" answer. Below it
	// the unit goes up one bucket.
	None float64 `yaml:"none"`
	Skim float64 `yaml:"skim"`
}

// Policy is loaded from .triage.yaml in the repo root and merged over
// the defaults. The forced-human list is policy, not a model decision.
type Policy struct {
	Generated  []string   `yaml:"generated"`
	ForceHuman []string   `yaml:"force_human"`
	Thresholds Thresholds `yaml:"thresholds"`
	// MaxUnitChars caps the diff text sent per unit. Truncated units can
	// never be classified "none".
	MaxUnitChars int `yaml:"max_unit_chars"`
	// ReviewContextChars caps the other units' diffs shown with each unit
	// in the review prompt.
	ReviewContextChars int `yaml:"review_context_chars"`
	// Tiers moves units using code-map impact, likelihood and review attention.
	Tiers TierPolicy `yaml:"tiers"`
	// Grouping reviews related units in one call instead of one each.
	Grouping GroupPolicy `yaml:"grouping"`
	// Lint runs static analysis over the lines the PR adds.
	Lint LintPolicy `yaml:"lint"`
}

// LintPolicy configures the static analysis run over a PR's added lines.
type LintPolicy struct {
	Enabled bool `yaml:"enabled"`
	// Tools names the external linters to run. Empty is auto: every known
	// tool the repository is configured for and that is installed. Naming
	// one runs it whether or not the repository has a config for it.
	Tools []string `yaml:"tools"`
	// Skip names tools never to run, so auto can be narrowed without
	// listing everything else.
	Skip []string `yaml:"skip"`
	// Secrets is the built-in credential scan over added lines. It needs
	// nothing installed, so it is on whenever linting is.
	Secrets bool `yaml:"secrets"`
	// TimeoutSec caps each tool; MaxPerUnit caps what one unit carries
	// into its prompt and its row on the Issues tab (0: no cap).
	TimeoutSec int `yaml:"timeout_sec"`
	MaxPerUnit int `yaml:"max_per_unit"`
}

const (
	DefaultLintTimeoutSec = 120
	DefaultLintPerUnit    = 10
)

func DefaultLintPolicy() LintPolicy {
	return LintPolicy{Enabled: true, Secrets: true, TimeoutSec: DefaultLintTimeoutSec, MaxPerUnit: DefaultLintPerUnit}
}

// KnownLinters names the external tools Set accepts.
func KnownLinters() []string {
	out := make([]string, len(knownLinters))
	for i, l := range knownLinters {
		out[i] = l.name
	}
	return out
}

// Set applies the -lint flag: "off", "auto", or a comma-separated list of
// tool names. The built-in secret scan stays on for auto and for a list;
// only "off" turns everything off.
func (p *LintPolicy) Set(spec string) error {
	spec = strings.TrimSpace(spec)
	switch strings.ToLower(spec) {
	case "", "auto", "on", "true":
		p.Enabled, p.Tools = true, nil
		return nil
	case "off", "none", "false":
		p.Enabled = false
		return nil
	}
	known := map[string]bool{}
	for _, n := range KnownLinters() {
		known[n] = true
	}
	var tools []string
	for _, n := range strings.Split(spec, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if !known[n] {
			return fmt.Errorf("unknown linter %q (want one of %s, or auto/off)", n, strings.Join(KnownLinters(), ", "))
		}
		tools = append(tools, n)
	}
	p.Enabled, p.Tools = true, tools
	return nil
}

func DefaultPolicy() Policy {
	return Policy{
		Generated: []string{
			"go.sum", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "Cargo.lock", "poetry.lock", "uv.lock", "packages.lock.json",
			"vendor/", "node_modules/",
			"*.pb.go", "*_pb2.py", "*.pb.gw.go", "*_grpc.pb.go", "*.g.cs", "*.g.i.cs", "*.Designer.cs",
			"*_mock.go", "mock_*.go", "mocks/",
			"zz_generated*", "*.gen.go", "*_gen.go",
			"*.min.js", "*.snap",
		},
		ForceHuman: []string{
			"migrations/", "**/migrations/**", "*.sql",
			"**/auth/**", "**/rbac/**", "*rbac*.yaml",
			"Dockerfile", "*.Dockerfile", "docker-compose*.yml",
			".github/workflows/", "Makefile",
			"*.tf", "*.tfvars",
			"charts/", "**/values*.yaml", "**/crds/**", "*_types.go",
			"go.mod",
		},
		Thresholds:         Thresholds{None: 0.9, Skim: 0.7},
		Lint:               DefaultLintPolicy(),
		MaxUnitChars:       24000,
		ReviewContextChars: DefaultReviewContextChars,
		Tiers:              DefaultTierPolicy(),
		Grouping:           DefaultGroupPolicy(),
	}
}

// LoadPolicy reads path (if it exists) and appends its lists to the defaults.
func LoadPolicy(file string) (Policy, error) {
	b, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return DefaultPolicy(), nil
	}
	if err != nil {
		return DefaultPolicy(), err
	}
	return ParsePolicy(b)
}

// ParsePolicy parses .triage.yaml content and merges it over the defaults.
func ParsePolicy(b []byte) (Policy, error) {
	p := DefaultPolicy()
	var user Policy
	if err := yaml.Unmarshal(b, &user); err != nil {
		return p, err
	}
	p.Generated = append(p.Generated, user.Generated...)
	p.ForceHuman = append(p.ForceHuman, user.ForceHuman...)
	if user.Thresholds.None > 0 {
		p.Thresholds.None = user.Thresholds.None
	}
	if user.Thresholds.Skim > 0 {
		p.Thresholds.Skim = user.Thresholds.Skim
	}
	if user.MaxUnitChars > 0 {
		p.MaxUnitChars = user.MaxUnitChars
	}
	if user.ReviewContextChars > 0 {
		p.ReviewContextChars = user.ReviewContextChars
	}
	// Grouping.Enabled defaults to true, so an absent key and an explicit
	// false must be told apart.
	var grouping struct {
		Grouping struct {
			Enabled    *bool `yaml:"enabled"`
			MaxChars   *int  `yaml:"max_chars"`
			MaxMembers *int  `yaml:"max_members"`
		} `yaml:"grouping"`
	}
	if err := yaml.Unmarshal(b, &grouping); err != nil {
		return p, err
	}
	if grouping.Grouping.Enabled != nil {
		p.Grouping.Enabled = *grouping.Grouping.Enabled
	}
	if n := grouping.Grouping.MaxChars; n != nil {
		if *n <= 0 {
			return p, fmt.Errorf("grouping.max_chars: want > 0, got %d", *n)
		}
		p.Grouping.MaxChars = *n
	}
	if n := grouping.Grouping.MaxMembers; n != nil {
		if *n < 0 {
			return p, fmt.Errorf("grouping.max_members: want >= 0 (0: no cap), got %d", *n)
		}
		p.Grouping.MaxMembers = *n
	}

	// Lint.Enabled and Lint.Secrets default to true, so an absent key and
	// an explicit false must be told apart here too.
	var lint struct {
		Lint struct {
			Enabled    *bool    `yaml:"enabled"`
			Tools      []string `yaml:"tools"`
			Skip       []string `yaml:"skip"`
			Secrets    *bool    `yaml:"secrets"`
			TimeoutSec *int     `yaml:"timeout_sec"`
			MaxPerUnit *int     `yaml:"max_per_unit"`
		} `yaml:"lint"`
	}
	if err := yaml.Unmarshal(b, &lint); err != nil {
		return p, err
	}
	known := map[string]bool{}
	for _, n := range KnownLinters() {
		known[n] = true
	}
	for _, n := range append(append([]string{}, lint.Lint.Tools...), lint.Lint.Skip...) {
		if !known[n] {
			return p, fmt.Errorf("lint: unknown linter %q (want one of %s)", n, strings.Join(KnownLinters(), ", "))
		}
	}
	if lint.Lint.Enabled != nil {
		p.Lint.Enabled = *lint.Lint.Enabled
	}
	if lint.Lint.Secrets != nil {
		p.Lint.Secrets = *lint.Lint.Secrets
	}
	p.Lint.Tools, p.Lint.Skip = lint.Lint.Tools, lint.Lint.Skip
	if n := lint.Lint.TimeoutSec; n != nil {
		if *n <= 0 {
			return p, fmt.Errorf("lint.timeout_sec: want > 0, got %d", *n)
		}
		p.Lint.TimeoutSec = *n
	}
	if n := lint.Lint.MaxPerUnit; n != nil {
		if *n < 0 {
			return p, fmt.Errorf("lint.max_per_unit: want >= 0 (0: no cap), got %d", *n)
		}
		p.Lint.MaxPerUnit = *n
	}

	// Tiers: fields the file sets override the defaults, the rest stay,
	// down to single budget fields.
	var tiers struct {
		Tiers struct {
			ReviewBudget string `yaml:"review_budget"`
			Budgets      map[string]struct {
				Trust       *float64 `yaml:"trust"`
				Human, Skim *int
				LiftFloors  *bool `yaml:"lift_floors"`
			} `yaml:"budgets"`
			KindWeights    map[string]float64 `yaml:"kind_weights"`
			CriticalImpact *int               `yaml:"critical_impact"`
		} `yaml:"tiers"`
	}
	if err := yaml.Unmarshal(b, &tiers); err != nil {
		return p, err
	}
	t := tiers.Tiers
	if t.ReviewBudget != "" {
		p.Tiers.ReviewBudget = t.ReviewBudget
	}
	for name, ub := range t.Budgets {
		bud := p.Tiers.Budgets[name]
		if ub.Trust != nil {
			bud.Trust = *ub.Trust
		}
		if ub.Human != nil {
			bud.Human = *ub.Human
		}
		if ub.Skim != nil {
			bud.Skim = *ub.Skim
		}
		if ub.LiftFloors != nil {
			bud.LiftFloors = *ub.LiftFloors
		}
		if bud.Trust < 0 || bud.Trust > 1 || bud.Skim > bud.Human {
			return p, fmt.Errorf("tiers.budgets.%s: want 0 <= trust <= 1 and skim <= human, got %+v", name, bud)
		}
		p.Tiers.Budgets[name] = bud
	}
	for k, w := range t.KindWeights {
		p.Tiers.KindWeights[k] = w
	}
	if t.CriticalImpact != nil {
		p.Tiers.CriticalImpact = *t.CriticalImpact
	}
	if _, _, err := p.Tiers.Budget(""); err != nil {
		return p, fmt.Errorf("tiers.review_budget: %w", err)
	}
	return p, nil
}

// LoadGitattributesGenerated returns patterns marked linguist-generated.
func LoadGitattributesGenerated(file string) []string {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	return ParseGitattributesGenerated(b)
}

func ParseGitattributesGenerated(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		for _, attr := range fields[1:] {
			if attr == "linguist-generated" || attr == "linguist-generated=true" {
				out = append(out, fields[0])
			}
		}
	}
	return out
}

// MatchAny reports the first pattern matching file, gitignore-style:
// patterns without a slash match the basename, "dir/" matches anything
// under a directory with that name, "**" crosses directories.
func MatchAny(patterns []string, file string) (string, bool) {
	for _, p := range patterns {
		if matchGlob(p, file) {
			return p, true
		}
	}
	return "", false
}

var (
	globMu    sync.Mutex
	globCache = map[string]*regexp.Regexp{}
)

func matchGlob(pattern, file string) bool {
	pattern = strings.TrimPrefix(pattern, "/")
	if strings.HasSuffix(pattern, "/") {
		dir := strings.TrimSuffix(pattern, "/")
		return file == dir || strings.HasPrefix(file, dir+"/") || strings.Contains(file, "/"+dir+"/")
	}
	if !strings.Contains(pattern, "/") {
		ok, _ := path.Match(pattern, path.Base(file))
		return ok
	}
	globMu.Lock()
	re, ok := globCache[pattern]
	if !ok {
		re = regexp.MustCompile("^" + globToRegexp(pattern) + "$")
		globCache[pattern] = re
	}
	globMu.Unlock()
	return re.MatchString(file)
}

func globToRegexp(p string) string {
	var sb strings.Builder
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				i++
				if i+1 < len(p) && p[i+1] == '/' {
					i++
					sb.WriteString("(.*/)?")
				} else {
					sb.WriteString(".*")
				}
			} else {
				sb.WriteString("[^/]*")
			}
		case '?':
			sb.WriteString("[^/]")
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return sb.String()
}
