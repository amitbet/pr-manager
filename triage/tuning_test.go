package triage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amitbet/pr-manager/llm"
)

// Grouping parameter sweep. Lives in the package because it measures the
// real prompt assembly, which is unexported. It is skipped unless
// PR_TUNING=1, makes no network or model calls, and writes its numbers to
// experiments/grouping-tuning/results. See that directory's README.

const tuningDir = "../experiments/grouping-tuning"

type tuningPR struct {
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Dir       string `json:"dir"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// reviewSet is one PR's units as the review stage would see them: rule
// skips dropped, everything else forced to human so that classification
// cannot confound the grouping measurement.
type reviewSet struct {
	pr     tuningPR
	all    []*Unit
	review []*Unit
	src    *Source
}

func loadTuningPRs(t *testing.T) []reviewSet {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(tuningDir, "data/index.json"))
	if err != nil {
		t.Skipf("no fetched PRs: %v (run experiments/grouping-tuning/fetch.py)", err)
	}
	var prs []tuningPR
	if err := json.Unmarshal(b, &prs); err != nil {
		t.Fatal(err)
	}
	policy := DefaultPolicy()
	var out []reviewSet
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
		rest := (&Presorter{Policy: policy}).Presort(units, src)
		for _, u := range rest {
			u.Decision = Decision{Bucket: BucketHuman, Source: "none", Reason: "forced for the sweep"}
		}
		setReviewContext(units, src.BaseContent, policy.ReviewContextChars)
		var review []*Unit
		for _, u := range rest {
			if len(u.Hunks) > 0 {
				review = append(review, u)
			}
		}
		out = append(out, reviewSet{pr: pr, all: units, review: review, src: src})
	}
	return out
}

// strongLinks counts the pairs a reviewer would most want together: one
// unit naming the other's symbol, or a test named after it. Measured with
// fixed weights so it stays a constant yardstick across configurations.
func strongLinks(units []*Unit) [][2]int {
	name := make([]string, len(units))
	diff := make([]string, len(units))
	for i, u := range units {
		name[i], diff[i] = shortName(u.Symbol), u.Diff()
	}
	var out [][2]int
	for i := range units {
		for j := i + 1; j < len(units); j++ {
			if mentions(diff[i], name[j]) || mentions(diff[j], name[i]) ||
				testFor(name[i], name[j]) || testFor(name[j], name[i]) {
				out = append(out, [2]int{i, j})
			}
		}
	}
	return out
}

type sweepRow struct {
	PR         string `json:"pr"`
	Config     string `json:"config"`
	MaxChars   int    `json:"max_chars"`
	MaxMembers int    `json:"max_members"`
	Units      int    `json:"units"`
	Calls      int    `json:"calls"`
	Prompt     int    `json:"prompt_chars"`
	Largest    int    `json:"largest_group"`
	Median     int    `json:"median_group"`
	Singletons int    `json:"singletons"`
	LinksKept  int    `json:"strong_links_kept"`
	LinksTotal int    `json:"strong_links_total"`
}

// promptChars is what the review stage would actually send for these
// groups: the grouped prompt plus system text, or the per-unit prompt for
// a group of one, exactly as AnalyzeGroup picks between them.
func promptChars(s *Summarizer, groups []*ReviewGroup) int {
	n := 0
	for _, g := range groups {
		if len(g.Members) == 1 {
			u := g.Members[0]
			n += len(analyzeSystem) + len(s.prompt(u, ruleNote(u)))
			continue
		}
		n += len(analyzeSystem) + len(groupSystem) + len(s.groupPrompt(g))
	}
	return n
}

func measure(rs reviewSet, label string, p groupParams, groups []*ReviewGroup) sweepRow {
	s := &Summarizer{Policy: DefaultPolicy()}
	setGroupContext(groups, rs.all, rs.src.BaseContent, DefaultPolicy().ReviewContextChars)

	owner := map[*Unit]int{}
	sizes := make([]int, 0, len(groups))
	row := sweepRow{PR: fmt.Sprintf("%s#%d", rs.pr.Repo, rs.pr.Number), Config: label,
		MaxChars: p.maxChars, MaxMembers: p.maxMembers, Units: len(rs.review), Calls: len(groups)}
	for i, g := range groups {
		sizes = append(sizes, len(g.Members))
		row.Largest = max(row.Largest, len(g.Members))
		if len(g.Members) == 1 {
			row.Singletons++
		}
		for _, u := range g.Members {
			owner[u] = i
		}
	}
	sort.Ints(sizes)
	if len(sizes) > 0 {
		row.Median = sizes[len(sizes)/2]
	}
	for _, l := range strongLinks(rs.review) {
		row.LinksTotal++
		if owner[rs.review[l[0]]] == owner[rs.review[l[1]]] {
			row.LinksKept++
		}
	}
	row.Prompt = promptChars(s, groups)
	return row
}

func TestGroupingSweep(t *testing.T) {
	if os.Getenv("PR_TUNING") == "" {
		t.Skip("set PR_TUNING=1 to run the grouping parameter sweep")
	}
	sets := loadTuningPRs(t)
	var rows []sweepRow
	for _, rs := range sets {
		t.Logf("%s#%d: %d units (%d after rule skips) %s",
			rs.pr.Repo, rs.pr.Number, len(rs.all), len(rs.review), rs.pr.Title)

		// Grouping off: one review call per unit.
		rows = append(rows, measure(rs, "off", groupParams{}, soloGroups(rs.review)))

		for _, chars := range []int{2000, 3000, 4000, 6000, 8000, 12000, 16000, 24000, 32000} {
			for _, members := range []int{0, 3, 4, 6, 8, 12} {
				p := defaultParams(GroupPolicy{MaxChars: chars})
				p.maxMembers = members
				label := fmt.Sprintf("chars=%d members=%d", chars, members)
				rows = append(rows, measure(rs, label, p, buildGroups(rs.review, p)))
			}
		}
		// Weight variants at the caps that matter, to see whether the
		// same-file bonus and the test bonus earn their place.
		for _, chars := range []int{6000, 12000} {
			for _, v := range []struct {
				name            string
				ref, test, file int
				smallPair       int
			}{
				{"no-samefile", 4, 3, 0, smallPairChars},
				{"no-test", 4, 0, 1, smallPairChars},
				{"refs-only", 4, 0, 0, smallPairChars},
				{"wide-samefile", 4, 3, 1, 6000},
			} {
				p := groupParams{maxChars: chars, ref: v.ref, test: v.test, sameFile: v.file, smallPair: v.smallPair}
				label := fmt.Sprintf("chars=%d %s", chars, v.name)
				rows = append(rows, measure(rs, label, p, buildGroups(rs.review, p)))
			}
		}
	}
	out := filepath.Join(tuningDir, "results")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(rows, "", "  ")
	if err := os.WriteFile(filepath.Join(out, "sweep.json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d rows to %s/sweep.json", len(rows), out)
}

// ---- Phase 2: run the finalist configurations through a real reviewer ----

// cachedLLM records every call and replays it from disk, so a long
// validation run can be resumed and re-analysed without paying again.
type cachedLLM struct {
	inner llm.LLMTool
	dir   string
	mu    sync.Mutex
	calls []callRecord
}

type callRecord struct {
	Tool    string  `json:"tool"`
	Prompt  int     `json:"prompt_chars"`
	In      int     `json:"input_tokens"`
	Out     int     `json:"output_tokens"`
	Seconds float64 `json:"seconds"`
	Cached  bool    `json:"cached"`
	Err     string  `json:"error,omitempty"`
}

func (c *cachedLLM) Name() string    { return c.inner.Name() }
func (c *cachedLLM) ModelID() string { return c.inner.ModelID() }

func (c *cachedLLM) Call(ctx context.Context, q llm.LLMRequest) (*llm.LLMResponse, error) {
	key, _ := json.Marshal(q)
	sum := sha256.Sum256(key)
	path := filepath.Join(c.dir, hex.EncodeToString(sum[:])[:32]+".json")
	tool := ""
	if len(q.Tools) > 0 {
		tool = q.Tools[0].Name
	}
	prompt := 0
	for _, m := range q.Messages {
		prompt += len(m.Content)
	}
	if b, err := os.ReadFile(path); err == nil {
		var resp llm.LLMResponse
		if json.Unmarshal(b, &resp) == nil {
			c.record(callRecord{Tool: tool, Prompt: prompt, In: resp.Usage.InputTokens,
				Out: resp.Usage.OutputTokens, Cached: true})
			return &resp, nil
		}
	}
	start := time.Now()
	resp, err := c.inner.Call(ctx, q)
	rec := callRecord{Tool: tool, Prompt: prompt, Seconds: time.Since(start).Seconds()}
	if err != nil {
		rec.Err = err.Error()
		c.record(rec)
		return resp, err
	}
	rec.In, rec.Out = resp.Usage.InputTokens, resp.Usage.OutputTokens
	c.record(rec)
	if b, err := json.Marshal(resp); err == nil {
		_ = os.WriteFile(path, b, 0o644)
	}
	return resp, nil
}

func (c *cachedLLM) record(r callRecord) {
	c.mu.Lock()
	c.calls = append(c.calls, r)
	c.mu.Unlock()
}

type armResult struct {
	PR           string         `json:"pr"`
	Arm          string         `json:"arm"`
	Units        int            `json:"units"`
	Groups       int            `json:"groups"`
	ReviewCalls  int            `json:"review_calls"`
	GroupCalls   int            `json:"group_calls"`
	SoloCalls    int            `json:"solo_calls"`
	CriticCalls  int            `json:"critic_calls"`
	Errors       int            `json:"errors"`
	InputTokens  int            `json:"input_tokens"`
	OutputTokens int            `json:"output_tokens"`
	Seconds      float64        `json:"seconds"`
	Reviewed     int            `json:"units_reviewed"`
	Unreviewed   []string       `json:"units_unreviewed,omitempty"`
	Issues       int            `json:"issues"`
	BySeverity   map[string]int `json:"issues_by_severity"`
	FocusItems   int            `json:"focus_items"`
	SummaryWords int            `json:"summary_words"`
	IssueTitles  []string       `json:"issue_titles"`
	IssueAnchors []string       `json:"issue_anchors"`
}

func runArm(t *testing.T, rs reviewSet, arm string, groups []*ReviewGroup, model llm.LLMTool, cacheDir string) armResult {
	t.Helper()
	// Fresh units: a previous arm already wrote summaries onto them.
	rec := &cachedLLM{inner: model, dir: cacheDir}
	s := &Summarizer{LLM: rec, Policy: DefaultPolicy()}
	setGroupContext(groups, rs.all, rs.src.BaseContent, DefaultPolicy().ReviewContextChars)

	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, g := range groups {
		wg.Add(1)
		go func(g *ReviewGroup) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.AnalyzeGroup(context.Background(), g)
		}(g)
	}
	wg.Wait()

	out := armResult{PR: fmt.Sprintf("%s#%d", rs.pr.Repo, rs.pr.Number), Arm: arm,
		Units: len(rs.review), Groups: len(groups), BySeverity: map[string]int{}}
	for _, c := range rec.calls {
		out.InputTokens += c.In
		out.OutputTokens += c.Out
		out.Seconds += c.Seconds
		if c.Err != "" {
			out.Errors++
		}
		switch {
		case strings.HasPrefix(c.Tool, "submit_group_"):
			out.GroupCalls++
		case c.Tool == "submit_analysis":
			out.SoloCalls++
		default:
			out.CriticCalls++
		}
	}
	out.ReviewCalls = out.GroupCalls + out.SoloCalls
	for _, u := range rs.review {
		if u.Reviewed {
			out.Reviewed++
		} else {
			out.Unreviewed = append(out.Unreviewed, u.ID)
		}
		out.FocusItems += len(u.Focus)
		out.SummaryWords += len(strings.Fields(u.Summary))
		for _, is := range u.Issues {
			out.Issues++
			out.BySeverity[is.Severity]++
			out.IssueTitles = append(out.IssueTitles, is.Title)
			out.IssueAnchors = append(out.IssueAnchors, u.ID)
		}
	}
	return out
}

func TestGroupingValidate(t *testing.T) {
	if os.Getenv("PR_TUNING_LLM") == "" {
		t.Skip("set PR_TUNING_LLM=1 to run the finalist validation (makes model calls)")
	}
	cacheDir := filepath.Join(tuningDir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	model := &llm.ClaudeCodeCLI{Model: "claude-haiku-4-5"}

	arms := []struct {
		name   string
		params *groupParams
	}{
		{"off", nil},
		{"chars=6000 members=6", &groupParams{maxChars: 6000, maxMembers: 6, ref: 4, test: 3, sameFile: 1, smallPair: smallPairChars}},
		{"chars=12000 members=8", &groupParams{maxChars: 12000, maxMembers: 8, ref: 4, test: 3, sameFile: 1, smallPair: smallPairChars}},
		{"chars=12000 members=0", &groupParams{maxChars: 12000, ref: 4, test: 3, sameFile: 1, smallPair: smallPairChars}},
		{"chars=32000 members=0", &groupParams{maxChars: 32000, ref: 4, test: 3, sameFile: 1, smallPair: smallPairChars}},
	}
	var results []armResult
	for _, a := range arms {
		for _, rs := range loadTuningPRs(t) { // reload so each arm starts from clean units
			groups := soloGroups(rs.review)
			if a.params != nil {
				groups = buildGroups(rs.review, *a.params)
			}
			r := runArm(t, rs, a.name, groups, model, cacheDir)
			results = append(results, r)
			t.Logf("%-22s %-32s groups=%3d calls=%3d(solo %d) reviewed=%d/%d issues=%d err=%d",
				a.name, r.PR, r.Groups, r.ReviewCalls, r.SoloCalls, r.Reviewed, r.Units, r.Issues, r.Errors)
			b, _ := json.MarshalIndent(results, "", "  ")
			_ = os.WriteFile(filepath.Join(tuningDir, "results/validate.json"), append(b, '\n'), 0o644)
		}
	}
}
