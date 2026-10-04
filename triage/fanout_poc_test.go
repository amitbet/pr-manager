package triage

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amitbet/pr-manager/llm"
)

// Agent fan-out POC: does one Claude Code agent that hands the review
// tasks to subagents beat one `claude -p` per review call? Skipped unless
// PR_FANOUT_POC=1. See experiments/agent-fanout.

const fanoutDir = "../experiments/agent-fanout"

// fanoutLLM holds the first wave of review calls (one per group) until
// all of them have arrived, then answers them from a single agent run in
// which subagents write one JSON answer file per task. Everything else
// (critic calls, solo re-reviews after a bad group reply) and any task
// whose file is missing or unreadable goes to direct.
type fanoutLLM struct {
	direct    llm.LLMTool
	agent     *llm.ClaudeCodeCLI
	subModel  string // the Agent tool's model alias
	perAgent  int    // tasks per subagent
	expected  int
	workDir   string
	mu        sync.Mutex
	pending   []*fanReq
	flushed   bool
	agentSecs float64
	agentErr  string
	fallbacks int
}

type fanReq struct {
	req llm.LLMRequest
	ch  chan fanRes
}

type fanRes struct {
	resp *llm.LLMResponse
	err  error
}

func (f *fanoutLLM) Name() string    { return f.direct.Name() }
func (f *fanoutLLM) ModelID() string { return f.direct.ModelID() }

func (f *fanoutLLM) Call(ctx context.Context, q llm.LLMRequest) (*llm.LLMResponse, error) {
	name := ""
	if len(q.Tools) > 0 {
		name = q.Tools[0].Name
	}
	f.mu.Lock()
	if f.flushed || (name != "submit_analysis" && name != "submit_group_analysis") {
		f.mu.Unlock()
		return f.direct.Call(ctx, q)
	}
	r := &fanReq{req: q, ch: make(chan fanRes, 1)}
	f.pending = append(f.pending, r)
	if len(f.pending) == f.expected {
		f.flushed = true
		go f.run(ctx, f.pending)
	}
	f.mu.Unlock()
	res := <-r.ch
	return res.resp, res.err
}

const fanoutSystem = `You coordinate code-review subagents. You never review code yourself and never read the task files.`

var fanoutDoneTool = llm.ToolDefinition{
	Name:        "submit_fanout",
	Description: "Report which answer files exist.",
	InputSchema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"written": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}},
		"required":   []string{"written"},
	},
}

func (f *fanoutLLM) run(ctx context.Context, reqs []*fanReq) {
	tasks := filepath.Join(f.workDir, "tasks")
	out := filepath.Join(f.workDir, "out")
	_ = os.MkdirAll(tasks, 0o755)
	_ = os.MkdirAll(out, 0o755)
	ids := make([]string, len(reqs))
	for i, r := range reqs {
		ids[i] = fmt.Sprintf("T%d", i+1)
		var system, user []string
		for _, m := range r.req.Messages {
			if m.Role == "system" {
				system = append(system, m.Content)
			} else {
				user = append(user, m.Content)
			}
		}
		schema, _ := json.MarshalIndent(r.req.Tools[0].InputSchema, "", " ")
		body := fmt.Sprintf("# Review task %s\n\n## Instructions\n\n%s\n\n## Input\n\n%s\n\n## Answer\n\n%s Write it as one JSON object to out/%s.json with the Write tool: only the JSON, no prose or code fence. Do not run commands and do not read any other file. The object must match this JSON Schema:\n\n%s\n",
			ids[i], strings.Join(system, "\n\n"), strings.Join(user, "\n\n"), r.req.Tools[0].Description, ids[i], schema)
		_ = os.WriteFile(filepath.Join(tasks, ids[i]+".md"), []byte(body), 0o644)
	}
	var assign []string
	for i := 0; i < len(ids); i += f.perAgent {
		j := min(i+f.perAgent, len(ids))
		var files []string
		for _, id := range ids[i:j] {
			files = append(files, "tasks/"+id+".md")
		}
		assign = append(assign, "- "+strings.Join(files, ", "))
	}
	prompt := fmt.Sprintf(`The directory tasks/ holds %d review tasks. Each task file says which answer file to write under out/.

Launch one subagent per line below with the Agent tool (subagent_type "general-purpose", model %q), all in a single message so they run in parallel:
%s

Give each subagent exactly this instruction, with its files filled in: "Read <files>. For each file, in turn, follow its instructions and write the JSON answer file it names. Reply with only the word done."

When they have all finished, list out/ with Glob. If any answer file is missing, launch one more subagent per missing task, once. Then report the answer files that exist.`, len(ids), f.subModel, strings.Join(assign, "\n"))

	start := time.Now()
	_, _, err := llm.CallToolIn(ctx, f.agent, &llm.Workspace{Dir: f.workDir, Edit: true}, []llm.ChatMessage{
		{Role: "system", Content: fanoutSystem},
		{Role: "user", Content: prompt},
	}, fanoutDoneTool, 2048)
	f.mu.Lock()
	f.agentSecs = time.Since(start).Seconds()
	if err != nil {
		f.agentErr = err.Error()
	}
	f.mu.Unlock()

	for i, r := range reqs {
		var args map[string]any
		b, rerr := os.ReadFile(filepath.Join(out, ids[i]+".json"))
		if rerr == nil {
			rerr = json.Unmarshal([]byte(extractJSONObject(string(b))), &args)
		}
		if rerr != nil || args == nil {
			f.mu.Lock()
			f.fallbacks++
			f.mu.Unlock()
			go func(r *fanReq) {
				resp, err := f.direct.Call(ctx, r.req)
				r.ch <- fanRes{resp, err}
			}(r)
			continue
		}
		r.ch <- fanRes{resp: &llm.LLMResponse{
			ToolCalls:  []llm.ToolCall{{Name: r.req.Tools[0].Name, Arguments: args}},
			StopReason: llm.StopToolUse,
		}}
	}
}

func extractJSONObject(s string) string {
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}

// teeClaude is a claude wrapper that keeps each run's stream-json under
// dir, so cost, cache and per-model usage can be read for every arm.
func teeClaude(t *testing.T, dir string) string {
	t.Helper()
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "claude-tee.sh")
	script := fmt.Sprintf("#!/bin/bash\nset -o pipefail\nclaude \"$@\" | tee %q/run-$$-$RANDOM.jsonl\n", dir)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

type streamTotals struct {
	Runs        int                `json:"cli_runs"`
	CostUSD     float64            `json:"cost_usd"`
	Input       int                `json:"input_tokens"`
	CacheRead   int                `json:"cache_read_tokens"`
	CacheCreate int                `json:"cache_create_tokens"`
	Output      int                `json:"output_tokens"`
	APISeconds  float64            `json:"api_seconds"`
	RunSeconds  float64            `json:"run_seconds"`
	ByModel     map[string]float64 `json:"cost_by_model"`
}

// readTotals sums the result events of every run under dir. modelUsage
// includes subagents; the top-level usage is the main loop only.
func readTotals(dir string) streamTotals {
	tot := streamTotals{ByModel: map[string]float64{}}
	files, _ := filepath.Glob(filepath.Join(dir, "run-*.jsonl"))
	for _, p := range files {
		fh, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		// A run with subagents prints a result event each time the main
		// loop resumes, every one cumulative: only the last one counts.
		var last []byte
		for sc.Scan() {
			if strings.Contains(sc.Text(), `"type":"result"`) {
				last = append(last[:0], sc.Bytes()...)
			}
		}
		var ev struct {
			Cost       float64 `json:"total_cost_usd"`
			DurationMS float64 `json:"duration_ms"`
			APIMS      float64 `json:"duration_api_ms"`
			ModelUsage map[string]struct {
				Input       int     `json:"inputTokens"`
				Output      int     `json:"outputTokens"`
				CacheRead   int     `json:"cacheReadInputTokens"`
				CacheCreate int     `json:"cacheCreationInputTokens"`
				Cost        float64 `json:"costUSD"`
			} `json:"modelUsage"`
		}
		if last != nil && json.Unmarshal(last, &ev) == nil {
			tot.Runs++
			tot.CostUSD += ev.Cost
			tot.RunSeconds += ev.DurationMS / 1000
			tot.APISeconds += ev.APIMS / 1000
			for m, u := range ev.ModelUsage {
				tot.Input += u.Input
				tot.Output += u.Output
				tot.CacheRead += u.CacheRead
				tot.CacheCreate += u.CacheCreate
				tot.ByModel[m] += u.Cost
			}
		}
		fh.Close()
	}
	return tot
}

type fanoutResult struct {
	PR          string         `json:"pr"`
	Arm         string         `json:"arm"`
	Units       int            `json:"units"`
	Groups      int            `json:"groups"`
	WallSeconds float64        `json:"wall_seconds"`
	AgentSecs   float64        `json:"agent_seconds,omitempty"`
	AgentErr    string         `json:"agent_error,omitempty"`
	Fallbacks   int            `json:"fallbacks"`
	Reviewed    int            `json:"units_reviewed"`
	Failed      int            `json:"units_failed"`
	Issues      int            `json:"issues"`
	BySeverity  map[string]int `json:"issues_by_severity"`
	Buckets     map[string]int `json:"buckets"`
	Totals      streamTotals   `json:"totals"`
	IssueTitles []string       `json:"issue_titles"`
}

func TestAgentFanout(t *testing.T) {
	if os.Getenv("PR_FANOUT_POC") == "" {
		t.Skip("set PR_FANOUT_POC=1 to run the agent fan-out POC (makes model calls)")
	}
	reviewer, sub := "claude-sonnet-5-5", "sonnet"
	if os.Getenv("PR_FANOUT_REVIEWER") == "opus" {
		reviewer, sub = llm.ClaudeCodeLarge, "opus"
	}
	want := map[string]bool{"grafana/grafana": true, "kubernetes/kubernetes": true, "vercel/next.js": true}
	if s := os.Getenv("PR_FANOUT_PRS"); s != "" {
		want = map[string]bool{}
		for _, r := range strings.Split(s, ",") {
			want[r] = true
		}
	}
	arms := []struct {
		name      string
		agent     bool
		orch, sub string
		perAgent  int
	}{
		{name: "baseline"},
		{name: "baseline-b"},
		{name: "agent-1", agent: true, orch: llm.ClaudeCodeSmall, sub: sub, perAgent: 1},
		{name: "agent-4", agent: true, orch: llm.ClaudeCodeSmall, sub: sub, perAgent: 4},
		{name: "agent-1-sonnet", agent: true, orch: "claude-sonnet-5-5", sub: sub, perAgent: 1},
		{name: "agent-1-opus", agent: true, orch: "claude-opus-5-5", sub: sub, perAgent: 1},
	}
	if s := os.Getenv("PR_FANOUT_ARMS"); s != "" {
		keep := strings.Split(s, ",")
		arms = slicesFilter(arms, func(name string) bool {
			for _, k := range keep {
				if k == name {
					return true
				}
			}
			return false
		}, func(a struct {
			name      string
			agent     bool
			orch, sub string
			perAgent  int
		}) string {
			return a.name
		})
	}
	resultsPath := filepath.Join(fanoutDir, "results/runs.json")
	var results []fanoutResult
	if b, err := os.ReadFile(resultsPath); err == nil {
		_ = json.Unmarshal(b, &results)
	}
	stamp := time.Now().Format("150405")
	for _, rs := range loadTuningPRs(t) {
		if !want[rs.pr.Repo] {
			continue
		}
		for _, a := range arms {
			rs := reloadPR(t, rs.pr.Repo)
			groups := BuildReviewGroups(rs.review, DefaultGroupPolicy())
			setGroupContext(groups, rs.all, rs.src.BaseContent, DefaultPolicy().ReviewContextChars)
			logs, _ := filepath.Abs(filepath.Join(fanoutDir, "logs", stamp, strings.ReplaceAll(rs.pr.Repo, "/", "-"), a.name))
			bin := teeClaude(t, logs)
			direct := &llm.ClaudeCodeCLI{Model: reviewer, Binary: bin}
			var model llm.LLMTool = direct
			var fan *fanoutLLM
			if a.agent {
				work, _ := filepath.Abs(filepath.Join(logs, "work"))
				fan = &fanoutLLM{direct: direct, agent: &llm.ClaudeCodeCLI{Model: a.orch, Binary: bin},
					subModel: a.sub, perAgent: a.perAgent, expected: len(groups), workDir: work}
				model = fan
			}
			s := &Summarizer{LLM: model, Policy: DefaultPolicy()}
			start := time.Now()
			var wg sync.WaitGroup
			sem := make(chan struct{}, 8) // production -j
			if a.agent {
				sem = make(chan struct{}, len(groups)) // every first-wave call must reach the batcher
			}
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
			r := fanoutResult{PR: fmt.Sprintf("%s#%d", rs.pr.Repo, rs.pr.Number), Arm: a.name + "/" + reviewer,
				Units: len(rs.review), Groups: len(groups), WallSeconds: time.Since(start).Seconds(),
				BySeverity: map[string]int{}, Buckets: map[string]int{}, Totals: readTotals(logs)}
			if fan != nil {
				r.AgentSecs, r.AgentErr, r.Fallbacks = fan.agentSecs, fan.agentErr, fan.fallbacks
			}
			for _, u := range rs.review {
				if u.Reviewed {
					r.Reviewed++
				}
				if u.Decision.Failed {
					r.Failed++
				}
				r.Buckets[string(u.Decision.Bucket)]++
				for _, is := range u.Issues {
					r.Issues++
					r.BySeverity[is.Severity]++
					r.IssueTitles = append(r.IssueTitles, u.ID+": ["+is.Severity+"] "+is.Title)
				}
			}
			t.Logf("%-14s %-28s groups=%2d wall=%5.0fs agent=%5.0fs fallbacks=%d reviewed=%d/%d issues=%d cost=$%.2f runs=%d %s",
				a.name, r.PR, r.Groups, r.WallSeconds, r.AgentSecs, r.Fallbacks, r.Reviewed, r.Units, r.Issues, r.Totals.CostUSD, r.Totals.Runs, r.AgentErr)
			results = append(results, r)
			_ = os.MkdirAll(filepath.Dir(resultsPath), 0o755)
			b, _ := json.MarshalIndent(results, "", "  ")
			_ = os.WriteFile(resultsPath, append(b, '\n'), 0o644)
		}
	}
}

// reloadPR gives each arm fresh units: an arm writes its review onto them.
func reloadPR(t *testing.T, repo string) reviewSet {
	for _, rs := range loadTuningPRs(t) {
		if rs.pr.Repo == repo {
			return rs
		}
	}
	t.Fatalf("no PR %s", repo)
	return reviewSet{}
}

func slicesFilter[T any](in []T, keep func(string) bool, name func(T) string) []T {
	var out []T
	for _, v := range in {
		if keep(name(v)) {
			out = append(out, v)
		}
	}
	return out
}
