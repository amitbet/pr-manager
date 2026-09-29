package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/internal/activity"
	"github.com/amitbet/pr-manager/internal/proc"
)

// Coding-agent subscriptions: instead of an API key, spawn the locally
// logged-in Codex CLI (ChatGPT plan) or Claude Code CLI (Claude plan) on this
// machine in one-shot structured-output mode, the same way t3code generates
// commit messages. The CLIs keep their own credentials; API keys are removed
// from their environment so a key in .env never overrides the subscription.

// Subscription model presets.
const (
	CodexSmall      = "gpt-6-luna"
	CodexLarge      = OpenAIGPT6Sol
	ClaudeCodeSmall = "claude-haiku-4-5"
	ClaudeCodeLarge = AnthropicClaudeOpus55
	// The fastest to translate in a benchmark of the models on each CLI.
	CodexTranslate      = "gpt-5.6-luna"
	ClaudeCodeTranslate = "claude-sonnet-5-5"
)

const cliTimeout = 5 * time.Minute

// CodexCLI runs `codex exec` with --output-schema.
type CodexCLI struct {
	Model  string
	Effort string // model_reasoning_effort: low|medium|high|xhigh ('' = model default)
	Binary string // default "codex"
}

func (c *CodexCLI) ModelID() string {
	if c.Model == "" {
		return CodexSmall
	}
	return c.Model
}

func (c *CodexCLI) Name() string { return "codex" }

func (c *CodexCLI) Call(ctx context.Context, req LLMRequest) (*LLMResponse, error) {
	tool, err := cliTool(req)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "pr-manager-codex-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	schema, err := json.Marshal(strictSchema(tool.InputSchema))
	if err != nil {
		return nil, err
	}
	schemaPath := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(schemaPath, schema, 0o600); err != nil {
		return nil, err
	}
	args := []string{
		"exec", "--json", "--ephemeral", "--skip-git-repo-check",
		"--ignore-user-config", "--ignore-rules", "-s", "read-only",
		"--model", c.ModelID(), "--output-schema", schemaPath,
	}
	// No AGENTS.md or AGENTS.override.md from the workspace, the untrusted
	// PR head, as project instructions.
	args = append(args, "--config", "project_doc_max_bytes=0")
	if effort := codexEffort(c.Effort); effort != "" {
		args = append(args, "--config", fmt.Sprintf("model_reasoning_effort=%q", effort))
	}
	// The read-only sandbox already lets it read anywhere; -C only sets
	// where it starts.
	cwd := dir
	if ws := req.Workspace; ws != nil {
		cwd = ws.Dir
		args = append(args, "-C", ws.Dir)
	}
	args = append(args, "-")
	system, prompt := cliPrompt(req.Messages, tool, req.Workspace != nil)
	if system != "" {
		prompt = system + "\n\n" + prompt
	}
	out, err := runCLI(ctx, orDefault(c.Binary, "codex"), args, cwd, prompt, codexEvent, "OPENAI_API_KEY", "CODEX_API_KEY")
	if err != nil {
		return nil, fmt.Errorf("codex/%s: %w", c.ModelID(), err)
	}
	// --json prints one event per line; the answer is the last agent message.
	var text string
	var usage Usage
	for _, line := range strings.Split(string(out), "\n") {
		var ev struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "item.completed":
			if ev.Item.Type == "agent_message" {
				text = ev.Item.Text
			}
		case "turn.completed":
			usage.InputTokens += ev.Usage.InputTokens
			usage.OutputTokens += ev.Usage.OutputTokens
		case "turn.failed", "error":
			return nil, fmt.Errorf("codex/%s: %s%s", c.ModelID(), ev.Message, ev.Error.Message)
		}
	}
	return structuredResponse(tool, text, nil, usage)
}

// codexEvent turns a `codex exec --json` event into an activity line: what
// the agent thinks and runs. The answer and bookkeeping events are dropped.
func codexEvent(line string) string {
	var ev struct {
		Type string `json:"type"`
		Item struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Command  string `json:"command"`
			ExitCode *int   `json:"exit_code"`
		} `json:"item"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(line), &ev) != nil {
		return line
	}
	switch ev.Type {
	case "item.started":
		if ev.Item.Type == "command_execution" {
			return "$ " + ev.Item.Command
		}
	case "item.completed":
		switch ev.Item.Type {
		case "reasoning":
			return "thinking: " + ev.Item.Text
		case "command_execution":
			if ev.Item.ExitCode != nil && *ev.Item.ExitCode != 0 {
				return fmt.Sprintf("  exit %d: %s", *ev.Item.ExitCode, ev.Item.Command)
			}
		case "agent_message":
		default:
			return ev.Item.Type + ": " + ev.Item.Text
		}
	case "error", "turn.failed":
		return ev.Type + ": " + ev.Message
	}
	return ""
}

func codexEffort(e string) string {
	switch e {
	case "", "none":
		return ""
	case "minimal":
		return "low" // subscription models start at low
	}
	return e
}

// ClaudeCodeCLI runs `claude -p` with --json-schema.
type ClaudeCodeCLI struct {
	Model  string
	Effort string // --effort: low|medium|high|xhigh|max ('' = model default)
	Binary string // default "claude"
}

func (c *ClaudeCodeCLI) ModelID() string {
	if c.Model == "" {
		return ClaudeCodeSmall
	}
	return c.Model
}

func (c *ClaudeCodeCLI) Name() string { return "claude-code" }

func (c *ClaudeCodeCLI) Call(ctx context.Context, req LLMRequest) (*LLMResponse, error) {
	tool, err := cliTool(req)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "pr-manager-claude-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	schema, err := json.Marshal(tool.InputSchema)
	if err != nil {
		return nil, err
	}
	system, prompt := cliPrompt(req.Messages, tool, req.Workspace != nil)
	tools, cwd := "", dir
	if ws := req.Workspace; ws != nil {
		tools, cwd = readOnlyTools, ws.Dir
	}
	// Anything long or multi-line goes in a file, not on the command line:
	// an npm install is claude.cmd on Windows, run through cmd.exe, which
	// ends the command at the first newline and expands % and ^.
	settingsPath := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"disableAllHooks":true}`), 0o600); err != nil {
		return nil, err
	}
	// The workspace is the untrusted PR head: --setting-sources user keeps
	// its .claude/settings*.json and every CLAUDE.md, CLAUDE.local.md and
	// .claude/rules in it (nested ones included) out of the session.
	args := []string{
		"-p", "--output-format", "json", "--json-schema", cmdSafeJSON(schema),
		"--model", c.ModelID(), "--tools", tools, "--disable-slash-commands",
		"--strict-mcp-config", "--permission-mode", "dontAsk",
		"--no-session-persistence", "--setting-sources", "user", "--settings", settingsPath,
	}
	if ws := req.Workspace; ws != nil {
		// dontAsk denies anything not allowed up front.
		args = append(args, "--allowedTools", readOnlyTools)
		for _, d := range ws.ReadDirs {
			args = append(args, "--add-dir", d)
		}
	}
	if system != "" {
		systemPath := filepath.Join(dir, "system.txt")
		if err := os.WriteFile(systemPath, []byte(system), 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--system-prompt-file", systemPath)
	}
	// Haiku has no effort setting.
	if e := c.Effort; e != "" && e != "none" && !strings.Contains(c.ModelID(), "haiku") {
		if e == "minimal" {
			e = "low"
		}
		args = append(args, "--effort", e)
	}
	// Its one JSON line is the answer, which CallToolIn logs.
	dropAll := func(string) string { return "" }
	out, err := runCLI(ctx, orDefault(c.Binary, "claude"), args, cwd, prompt, dropAll, "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN")
	if err != nil {
		return nil, fmt.Errorf("claude-code/%s: %w", c.ModelID(), err)
	}
	var res struct {
		IsError          bool           `json:"is_error"`
		Result           string         `json:"result"`
		StructuredOutput map[string]any `json:"structured_output"`
		Usage            struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("claude-code/%s: decode output: %w", c.ModelID(), err)
	}
	if res.IsError {
		return nil, fmt.Errorf("claude-code/%s: %s", c.ModelID(), res.Result)
	}
	usage := Usage{
		InputTokens:  res.Usage.InputTokens + res.Usage.CacheCreationInputTokens + res.Usage.CacheReadInputTokens,
		OutputTokens: res.Usage.OutputTokens,
	}
	return structuredResponse(tool, res.Result, res.StructuredOutput, usage)
}

// cliTool returns the tool whose arguments the CLI's structured output
// stands in for. Only forced single-tool calls (CallTool) are supported.
func cliTool(req LLMRequest) (ToolDefinition, error) {
	if len(req.Tools) == 0 || req.ToolChoice != ToolChoiceRequired {
		return ToolDefinition{}, fmt.Errorf("subscription CLIs only support forced tool calls")
	}
	return req.Tools[0], nil
}

// readOnlyTools are the Claude Code tools a workspace call gets.
const readOnlyTools = "Read,Grep,Glob"

// cliPrompt flattens the chat into a system prompt and one user prompt.
// canRead says whether the CLI was given a workspace to read.
func cliPrompt(msgs []ChatMessage, tool ToolDefinition, canRead bool) (string, string) {
	var system []string
	var sb strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "system":
			system = append(system, m.Content)
		case "assistant":
			fmt.Fprintf(&sb, "<assistant>\n%s\n</assistant>\n\n", m.Content)
		default:
			sb.WriteString(m.Content + "\n\n")
		}
	}
	if canRead {
		fmt.Fprintf(&sb, "%s Read files if you need to, but do not change anything. Answer only with the JSON object.", tool.Description)
	} else {
		fmt.Fprintf(&sb, "%s Answer only with the JSON object; do not run commands or read files.", tool.Description)
	}
	return strings.Join(system, "\n\n"), sb.String()
}

// cmdSafeJSON is JSON with the characters cmd.exe treats specially outside
// quotes (%, ^, |, !, and the <, >, & encoding/json already escapes)
// written as \u escapes. They can only occur inside JSON strings, where the
// escape means the same, so the value is unchanged and survives a .cmd shim.
func cmdSafeJSON(b []byte) string {
	return cmdJSONEscaper.Replace(string(b))
}

var cmdJSONEscaper = strings.NewReplacer("%", `\u0025`, "^", `\u005e`, "|", `\u007c`, "!", `\u0021`, "<", `\u003c`, ">", `\u003e`, "&", `\u0026`)

// cmdUnsafe reports an argument cmd.exe would cut or rewrite when bin is a
// .cmd or .bat shim, which Go runs through cmd.exe: a line break ends the
// command there, and %, ^, &, |, <, > and ! are cmd syntax whenever the
// quoting cmd.exe tracks gets out of step with the C runtime's.
func cmdUnsafe(bin string, args []string) error {
	if ext := strings.ToLower(filepath.Ext(bin)); ext != ".cmd" && ext != ".bat" {
		return nil
	}
	for _, a := range args {
		if strings.ContainsAny(a, "\r\n%^&|<>!") {
			return fmt.Errorf("argument %q can't pass through %s; install the native binary", truncate(a, 60), filepath.Base(bin))
		}
	}
	return nil
}

// structuredResponse turns the CLI's JSON answer into a call to tool. Nulls
// are dropped so fields strictSchema made nullable look omitted, as they do
// from the API providers.
func structuredResponse(tool ToolDefinition, text string, args map[string]any, usage Usage) (*LLMResponse, error) {
	if args == nil {
		if err := json.Unmarshal([]byte(extractJSON(text)), &args); err != nil {
			return nil, fmt.Errorf("no JSON answer: %q", truncate(text, 200))
		}
	}
	if args == nil { // the answer was null
		return nil, fmt.Errorf("no JSON answer: %q", truncate(text, 200))
	}
	return &LLMResponse{
		ToolCalls:  []ToolCall{{Name: tool.Name, Arguments: dropNulls(args).(map[string]any)}},
		StopReason: StopToolUse,
		Usage:      usage,
	}, nil
}

func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}

func dropNulls(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if x == nil {
				delete(t, k)
			} else {
				t[k] = dropNulls(x)
			}
		}
	case []any:
		for i, x := range t {
			t[i] = dropNulls(x)
		}
	}
	return v
}

// strictSchema rewrites a schema for OpenAI strict structured output, which
// Codex uses: every object closes with additionalProperties=false and lists
// all its properties as required, so optional ones become nullable.
func strictSchema(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t)+2)
		for k, x := range t {
			out[k] = strictSchema(x)
		}
		props, ok := out["properties"].(map[string]any)
		if !ok {
			return out
		}
		required := map[string]bool{}
		switch r := t["required"].(type) {
		case []string:
			for _, k := range r {
				required[k] = true
			}
		case []any:
			for _, k := range r {
				required[fmt.Sprint(k)] = true
			}
		}
		all := make([]string, 0, len(props))
		for k, p := range props {
			all = append(all, k)
			if pm, ok := p.(map[string]any); ok && !required[k] {
				props[k] = nullable(pm)
			}
		}
		out["required"] = all
		out["additionalProperties"] = false
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = strictSchema(x)
		}
		return out
	}
	return v
}

func nullable(p map[string]any) map[string]any {
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = v
	}
	if typ, ok := p["type"].(string); ok {
		out["type"] = []any{typ, "null"}
		if enum, ok := p["enum"].([]string); ok {
			e := make([]any, 0, len(enum)+1)
			for _, s := range enum {
				e = append(e, s)
			}
			out["enum"] = append(e, nil)
		}
	}
	return out
}

// runCLI runs bin in dir with prompt on stdin and the given env vars unset,
// and returns stdout. Its output goes to ctx's activity log as it comes,
// stdout lines through format.
func runCLI(ctx context.Context, bin string, args []string, dir, prompt string, format func(string) string, unset ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	if path, err := exec.LookPath(bin); err == nil {
		if err := cmdUnsafe(path, args); err != nil {
			return nil, err
		}
	}
	cmd := proc.CommandContext(ctx, bin, args...)
	ownGroup(cmd)
	cmd.Dir = dir
	cmd.Env = cliEnv(unset...)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	logOut, logErr := activity.Writer(ctx, ""), activity.Writer(ctx, "stderr: ")
	logOut.Format = format
	defer logOut.Flush()
	defer logErr.Flush()
	cmd.Stdout, cmd.Stderr = io.MultiWriter(&stdout, logOut), io.MultiWriter(&stderr, logErr)
	shown := make([]string, len(args))
	for i, a := range args {
		shown[i] = truncate(a, 120) // schemas and system prompts
	}
	activity.Printf(ctx, "$ %s %s", bin, strings.Join(shown, " "))
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("%v: %s", err, truncate(msg, 500))
	}
	return stdout.Bytes(), nil
}

func cliEnv(unset ...string) []string {
	drop := map[string]bool{}
	for _, k := range unset {
		drop[k] = true
	}
	env := []string{"CLAUDE_CODE_AUTO_CONNECT_IDE=0", "ENABLE_CLAUDEAI_MCP_SERVERS=false"}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			env = append(env, kv)
		}
	}
	return env
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// subState is what is known of one provider's subscription.
type subState struct {
	have bool
	at   time.Time // of the last probe that found none
	// probing is open while a probe of the provider runs; callers wait
	// on it rather than probe too.
	probing chan struct{}
}

var (
	subsMu sync.Mutex
	subs   = map[string]*subState{}
)

// subsRetry is how long a missing subscription is believed: the user may log
// in while the app runs.
const subsRetry = 30 * time.Second

// HasSubscription reports whether provider ("codex" or "claude-code") is
// installed on this machine and logged in with a subscription rather than an
// API key. Each provider is cached on its own: a subscription found is
// remembered until forgetSubscriptions; a missing one is probed again after
// subsRetry. Only one probe of a provider runs at a time, without holding
// up callers asking about the other.
func HasSubscription(provider string) bool {
	subsMu.Lock()
	var st *subState
	for {
		st = subs[provider]
		if st == nil {
			st = &subState{}
			subs[provider] = st
		}
		if st.have {
			subsMu.Unlock()
			return true
		}
		if st.probing == nil {
			if !st.at.IsZero() && time.Since(st.at) < subsRetry {
				subsMu.Unlock()
				return false
			}
			break
		}
		wait := st.probing
		subsMu.Unlock()
		<-wait
		subsMu.Lock()
	}
	done := make(chan struct{})
	st.probing = done
	subsMu.Unlock()

	have := probeSubscription(provider)

	subsMu.Lock()
	defer subsMu.Unlock()
	st.probing = nil
	close(done)
	if have {
		st.have = true
	} else {
		st.at = time.Now()
	}
	return have
}

// forgetSubscriptions makes the next HasSubscription probe the CLIs again.
// A probe already running reports to its callers but isn't kept.
func forgetSubscriptions() {
	subsMu.Lock()
	subs = map[string]*subState{}
	subsMu.Unlock()
}

func probeSubscription(provider string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch provider {
	case "codex":
		// "Logged in using ChatGPT" vs "Logged in using an API key".
		out, err := probe(ctx, "codex", "login", "status")
		return err == nil && strings.Contains(out, "ChatGPT")
	case "claude-code":
		out, err := probe(ctx, "claude", "auth", "status")
		var st struct {
			LoggedIn         bool   `json:"loggedIn"`
			AuthMethod       string `json:"authMethod"`
			SubscriptionType string `json:"subscriptionType"`
		}
		return err == nil && json.Unmarshal([]byte(extractJSON(out)), &st) == nil &&
			st.LoggedIn && (st.AuthMethod == "claude.ai" || st.SubscriptionType != "")
	}
	return false
}

func probe(ctx context.Context, bin string, args ...string) (string, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return "", err
	}
	cmd := proc.CommandContext(ctx, bin, args...)
	ownGroup(cmd)
	cmd.Env = cliEnv("OPENAI_API_KEY", "CODEX_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN")
	out, err := cmd.CombinedOutput()
	return string(out), err
}
