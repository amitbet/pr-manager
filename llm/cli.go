package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/internal/activity"
	"github.com/amitbet/pr-manager/internal/proc"
)

// Coding-agent subscriptions: instead of an API key, spawn the locally
// logged-in Codex CLI (ChatGPT plan) or Claude Code CLI (Claude plan) on this
// machine in one-shot structured-output mode, the same way t3code generates
// commit messages. The CLIs keep their own credentials; API keys (and, for
// Claude Code, the variables that point it at another endpoint, cloud
// provider or model) are removed from their environment so a key in .env
// never overrides the subscription.

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

// cliTimeout caps a one-shot answer; workspaceTimeout a call that reads the
// workspace, an agent session that can take many turns over a large PR.
const (
	cliTimeout       = 5 * time.Minute
	workspaceTimeout = 20 * time.Minute
)

// callTimeout is how long a CLI call with req may run.
func callTimeout(req LLMRequest) time.Duration {
	if req.Workspace != nil {
		return workspaceTimeout
	}
	return cliTimeout
}

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
	ws := req.Workspace
	var sess *Session
	if ws != nil && !ws.Edit {
		sess = ws.Session
		// Codex writes nothing in read-only, so it builds in a scratch
		// copy only (build.go).
		if ws.Builds != "" && !ws.Scratch {
			c := *ws
			c.Builds = ""
			ws = &c
		}
	}
	resume := sess != nil && sess.ID != ""
	// A resumed session keeps its sandbox and directory; exec resume takes
	// neither -s nor -C, so the sandbox is set again as configuration.
	args := []string{"exec", "--json", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules",
		"--model", c.ModelID(), "--output-schema", schemaPath}
	if resume {
		args = append([]string{"exec", "resume"}, args[1:]...)
		args = append(args, "--config", fmt.Sprintf("sandbox_mode=%q", codexSandbox(ws)))
	} else {
		args = append(args, "-s", codexSandbox(ws))
	}
	if sess == nil {
		args = append(args, "--ephemeral")
	}
	// No AGENTS.md or AGENTS.override.md from the workspace, the untrusted
	// PR head, as project instructions.
	args = append(args, "--config", "project_doc_max_bytes=0")
	if effort := codexEffort(c.Effort); effort != "" {
		args = append(args, "--config", fmt.Sprintf("model_reasoning_effort=%q", effort))
	}
	args = append(args, codexReach(ws)...)
	args = append(args, codexBuildArgs(ws)...)
	// The read-only sandbox already lets it read anywhere; -C only sets
	// where it starts.
	cwd := dir
	if ws != nil {
		cwd = ws.Dir
		if !resume {
			args = append(args, "-C", ws.Dir)
		}
	}
	if ws != nil && !ws.Edit {
		for _, p := range ws.Images {
			args = append(args, "-i", p)
		}
	}
	if resume {
		args = append(args, sess.ID)
	}
	args = append(args, "-")
	system, prompt := cliPrompt(req.Messages, tool, ws)
	// The system prompt is part of codex's transcript: a resumed session
	// gets it again only when it changed.
	if system != "" {
		switch h := hashText(system); {
		case !resume:
			prompt = system + "\n\n" + prompt
		case h != sess.System:
			prompt = "What you were told at the start has changed; this replaces it:\n\n" + system + "\n\n" + prompt
		}
		if sess != nil {
			sess.System = hashText(system)
		}
	}
	out, err := runCLI(ctx, orDefault(c.Binary, "codex"), args, cwd, prompt, callTimeout(req), codexEvent, "OPENAI_API_KEY", "CODEX_API_KEY")
	if err != nil {
		return nil, fmt.Errorf("codex/%s: %w", c.ModelID(), err)
	}
	// --json prints one event per line; the answer is the last agent message.
	var text string
	var usage Usage
	for _, line := range strings.Split(string(out), "\n") {
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Item     struct {
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
		case "thread.started":
			if sess != nil && ev.ThreadID != "" {
				sess.ID = ev.ThreadID
			}
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
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Command   string          `json:"command"`
			ExitCode  *int            `json:"exit_code"`
			Server    string          `json:"server"`
			Tool      string          `json:"tool"`
			Arguments json.RawMessage `json:"arguments"`
			Query     string          `json:"query"`
			Status    string          `json:"status"`
			Error     *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"item"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(line), &ev) != nil {
		return line
	}
	switch ev.Type {
	case "item.started":
		switch ev.Item.Type {
		case "command_execution":
			return "$ " + ev.Item.Command
		case "mcp_tool_call":
			return "→ " + mcpCall(ev.Item.Server, ev.Item.Tool, ev.Item.Arguments)
		}
	case "item.completed":
		switch ev.Item.Type {
		case "reasoning":
			return "thinking: " + ev.Item.Text
		case "command_execution":
			if ev.Item.ExitCode != nil && *ev.Item.ExitCode != 0 {
				return fmt.Sprintf("  exit %d: %s", *ev.Item.ExitCode, ev.Item.Command)
			}
		case "mcp_tool_call":
			if ev.Item.Error != nil || ev.Item.Status == "failed" {
				msg := ev.Item.Status
				if ev.Item.Error != nil {
					msg = ev.Item.Error.Message
				}
				return "  error: " + truncate(msg, 300)
			}
		case "web_search":
			return "→ WebSearch " + strconv.Quote(ev.Item.Query)
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
	system, prompt := cliPrompt(req.Messages, tool, req.Workspace)
	tools, cwd := "", dir
	reachTools, reachAllowed, reachArgs, err := claudeReach(req.Workspace, dir)
	if err != nil {
		return nil, err
	}
	var sess *Session
	if ws := req.Workspace; ws != nil {
		tools, cwd = readOnlyTools, ws.Dir
		if ws.Edit {
			tools = editTools
		} else {
			if ws.Shell {
				tools += ",Bash"
			}
			if reachTools != "" {
				tools += "," + reachTools
			}
			sess = ws.Session
		}
	}
	// Anything long or multi-line goes in a file, not on the command line:
	// an npm install is claude.cmd on Windows, run through cmd.exe, which
	// ends the command at the first newline and expands % and ^.
	settingsPath := filepath.Join(dir, "settings.json")
	settings, err := claudeSettings(req.Workspace)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(settingsPath, settings, 0o600); err != nil {
		return nil, err
	}
	// The workspace is the untrusted PR head: --setting-sources user keeps
	// its .claude/settings*.json and every CLAUDE.md, CLAUDE.local.md and
	// .claude/rules in it (nested ones included) out of the session.
	args := []string{
		"-p", "--output-format", "stream-json", "--verbose", "--json-schema", cmdSafeJSON(schema),
		"--include-partial-messages", // see claudeStream
		"--model", c.ModelID(), "--tools", tools, "--disable-slash-commands",
		"--strict-mcp-config", "--permission-mode", "dontAsk",
		"--setting-sources", "user", "--settings", settingsPath,
	}
	// A session is kept, under the working directory's project, only when
	// the caller carries the conversation on (see session.go).
	newSession := ""
	switch {
	case sess == nil:
		args = append(args, "--no-session-persistence")
	case sess.ID != "":
		args = append(args, "--resume", sess.ID)
	default:
		newSession = newSessionID()
		args = append(args, "--session-id", newSession)
	}
	args = append(args, reachArgs...)
	if ws := req.Workspace; ws != nil {
		// dontAsk denies anything not allowed up front: edits only in
		// the working directory.
		allowed := readOnlyTools
		if ws.Edit {
			allowed = editAllowed
		} else if ws.Shell {
			allowed += "," + shellAllowed()
		}
		if !ws.Edit && reachAllowed != "" {
			allowed += "," + reachAllowed
		}
		args = append(args, "--allowedTools", allowed)
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
	stream := newClaudeStream(ctx, cwd)
	out, err := runCLI(ctx, orDefault(c.Binary, "claude"), args, cwd, prompt, callTimeout(req), stream.line, claudeCodeUnset()...)
	stream.done(err)
	if err != nil {
		return nil, fmt.Errorf("claude-code/%s: %w", c.ModelID(), err)
	}
	// stream-json prints one event per line; the answer is the result event.
	type result struct {
		Type             string         `json:"type"`
		IsError          bool           `json:"is_error"`
		Result           string         `json:"result"`
		SessionID        string         `json:"session_id"`
		StructuredOutput map[string]any `json:"structured_output"`
		Usage            struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	var res *result
	for _, line := range strings.Split(string(out), "\n") {
		var ev result
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type == "result" {
			res = &ev
		}
	}
	if res == nil {
		return nil, fmt.Errorf("claude-code/%s: no result in output: %q", c.ModelID(), truncate(string(out), 200))
	}
	if res.IsError {
		return nil, fmt.Errorf("claude-code/%s: %s", c.ModelID(), res.Result)
	}
	if sess != nil {
		sess.ID = orDefault(res.SessionID, orDefault(newSession, sess.ID))
		sess.System = hashText(system)
	}
	usage := Usage{
		InputTokens:  res.Usage.InputTokens + res.Usage.CacheCreationInputTokens + res.Usage.CacheReadInputTokens,
		OutputTokens: res.Usage.OutputTokens,
	}
	return structuredResponse(tool, res.Result, res.StructuredOutput, usage)
}

// claudeEvent turns a `claude -p --output-format stream-json` event into
// activity lines: what the agent says and the tools it calls, with paths
// relative to dir. The answer (the StructuredOutput call and the result) and
// bookkeeping events are dropped. See claudeStream for what an agent run
// adds.
func claudeEvent(dir string) func(string) string {
	return newClaudeStream(context.Background(), dir).line
}

// claudeStream formats one claude run's events. A subagent's events go to
// a thread of its own in ctx's activity log, opened when it starts and
// closed when it ends (or at done). While the model thinks or writes a
// long answer, which shows nothing until the turn ends (thinking comes
// back empty), a line every progressEvery says how far it has got.
type claudeStream struct {
	ctx  context.Context
	rel  func(string) string
	now  func() time.Time
	mu   sync.Mutex
	subs map[string]*claudeSub // by the Agent call's tool_use id
	main claudeProgress
}

type claudeSub struct {
	t        *activity.Thread
	name     string
	progress claudeProgress
}

// claudeProgress is a turn's output so far, and when it was last said.
type claudeProgress struct {
	thinking, writing int // estimated thinking tokens, chars written
	said              time.Time
}

const progressEvery = 15 * time.Second

func newClaudeStream(ctx context.Context, dir string) *claudeStream {
	// Claude reports paths with the directory's symlinks resolved, as
	// /private/var for macOS's /var.
	dirs := []string{dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		dirs = append(dirs, real)
	}
	return &claudeStream{
		ctx: ctx,
		rel: func(p string) string {
			for _, d := range dirs {
				if r, ok := strings.CutPrefix(p, d+string(filepath.Separator)); ok {
					return r
				}
			}
			return p
		},
		now:  time.Now,
		subs: map[string]*claudeSub{},
	}
}

// sub is the thread of the subagent the Agent call id started, opened
// under name if it has none yet.
func (c *claudeStream) sub(id, name string) *claudeSub {
	if s := c.subs[id]; s != nil {
		return s
	}
	if name == "" {
		name = "subagent"
	}
	_, t := activity.Start(c.ctx, "agent", "subagent: %s", name)
	s := &claudeSub{t: t, name: name}
	c.subs[id] = s
	return s
}

// done closes the subagent threads still open, as when the run died.
func (c *claudeStream) done(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, s := range c.subs {
		if err == nil {
			err = errors.New("the run ended before the subagent did")
		}
		s.t.Finish(err)
		delete(c.subs, id)
	}
}

// tick says how far a turn has got, at most every progressEvery.
func (c *claudeStream) tick(p *claudeProgress) string {
	now := c.now()
	if p.said.IsZero() {
		p.said = now
		return ""
	}
	if now.Sub(p.said) < progressEvery {
		return ""
	}
	p.said = now
	var parts []string
	if p.thinking > 0 {
		parts = append(parts, fmt.Sprintf("thought ~%d tokens", p.thinking))
	}
	if p.writing > 0 {
		parts = append(parts, fmt.Sprintf("wrote %d chars", p.writing))
	}
	if len(parts) == 0 {
		return ""
	}
	return "… " + strings.Join(parts, ", ") + " so far"
}

func (c *claudeStream) line(line string) string {
	var ev struct {
		Type          string `json:"type"`
		Subtype       string `json:"subtype"`
		IsError       bool   `json:"is_error"`
		Result        string `json:"result"`
		Parent        string `json:"parent_tool_use_id"`
		TaskName      string `json:"task_description"`
		ToolUseID     string `json:"tool_use_id"`
		Description   string `json:"description"`
		Prompt        string `json:"prompt"`
		Status        string `json:"status"`
		Summary       string `json:"summary"`
		ThinkingDelta int    `json:"estimated_tokens_delta"`
		Event         struct {
			Type  string `json:"type"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		} `json:"event"`
		Usage struct {
			ToolUses   int `json:"tool_uses"`
			DurationMS int `json:"duration_ms"`
		} `json:"usage"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(line), &ev) != nil {
		return line
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Where the event's lines go: a subagent's thread, or the run's.
	var sub *claudeSub
	progress := &c.main
	if ev.Parent != "" {
		sub = c.sub(ev.Parent, ev.TaskName)
		progress = &sub.progress
	}
	out := func(lines []string) string {
		text := strings.Join(lines, "\n")
		if sub != nil {
			if text != "" {
				sub.t.Printf("%s", text)
			}
			return ""
		}
		return text
	}
	switch ev.Type {
	case "system":
		switch ev.Subtype {
		case "task_started":
			s := c.sub(ev.ToolUseID, ev.Description)
			if p := strings.TrimSpace(ev.Prompt); p != "" {
				s.t.Printf("task: %s", truncate(p, 1500))
			}
			return "→ subagent started: " + s.name
		case "task_notification":
			s := c.sub(ev.ToolUseID, ev.Description)
			delete(c.subs, ev.ToolUseID)
			if sum := strings.TrimSpace(ev.Summary); sum != "" {
				s.t.Printf("%s", sum)
			}
			var err error
			if ev.Status != "completed" {
				err = fmt.Errorf("subagent %s", orDefault(ev.Status, "failed"))
			}
			s.t.Finish(err)
			took := time.Duration(ev.Usage.DurationMS) * time.Millisecond
			return fmt.Sprintf("← subagent %s: %s, %d tool calls in %s", orDefault(ev.Status, "ended"), s.name, ev.Usage.ToolUses, took.Round(time.Second))
		case "thinking_tokens":
			progress.thinking += ev.ThinkingDelta
			return out([]string{c.tick(progress)})
		}
		return ""
	case "stream_event":
		switch ev.Event.Type {
		case "message_start":
			*progress = claudeProgress{said: c.now()}
		case "content_block_delta":
			progress.writing += len(ev.Event.Delta.Text) + len(ev.Event.Delta.PartialJSON)
			return out([]string{c.tick(progress)})
		}
		return ""
	}
	var blocks []struct {
		Type     string          `json:"type"`
		ID       string          `json:"id"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
		IsError  bool            `json:"is_error"`
		Content  json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(ev.Message.Content, &blocks) // a string for synthetic messages
	var lines []string
	switch ev.Type {
	case "assistant":
		for _, b := range blocks {
			switch b.Type {
			case "thinking":
				if t := strings.TrimSpace(b.Thinking); t != "" {
					lines = append(lines, "thinking: "+t)
				}
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					lines = append(lines, t)
				}
			case "tool_use":
				if b.Name != "StructuredOutput" {
					lines = append(lines, "→ "+claudeToolCall(b.Name, b.Input, c.rel))
				}
			}
		}
	case "user":
		for _, b := range blocks {
			if b.Type == "tool_result" && b.IsError {
				var msg string
				if json.Unmarshal(b.Content, &msg) != nil {
					msg = string(b.Content)
				}
				lines = append(lines, "  error: "+truncate(strings.TrimSpace(msg), 300))
			}
		}
	case "result":
		if ev.IsError {
			lines = append(lines, "error: "+ev.Result)
		}
	}
	return out(lines)
}

// claudeToolCall is a one-line summary of a Claude Code tool call.
func claudeToolCall(name string, input json.RawMessage, rel func(string) string) string {
	var in struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		Pattern  string `json:"pattern"`
		Glob     string `json:"glob"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	}
	_ = json.Unmarshal(input, &in)
	switch name {
	case "Read":
		s := "Read " + rel(in.FilePath)
		if in.Offset > 0 || in.Limit > 0 {
			s += fmt.Sprintf(" (from line %d, %d lines)", max(in.Offset, 1), in.Limit)
		}
		return s
	case "Edit", "Write":
		return name + " " + rel(in.FilePath)
	case "Agent":
		var a struct {
			Description string `json:"description"`
			Background  *bool  `json:"run_in_background"`
		}
		_ = json.Unmarshal(input, &a)
		s := "Agent: " + a.Description
		if a.Background != nil && !*a.Background {
			s += " (waits for it)"
		}
		return s
	case "WebFetch":
		var w struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(input, &w)
		return "Fetch " + w.URL
	case "WebSearch":
		var w struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(input, &w)
		return fmt.Sprintf("WebSearch %q", w.Query)
	case "Bash":
		var b struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(input, &b)
		return "$ " + truncate(b.Command, 300)
	case "Grep", "Glob":
		s := fmt.Sprintf("%s %q", name, in.Pattern)
		if in.Glob != "" {
			s += " --glob " + in.Glob
		}
		if in.Path != "" {
			s += " in " + rel(in.Path)
		}
		return s
	}
	if rest, ok := strings.CutPrefix(name, "mcp__"); ok {
		server, tool, _ := strings.Cut(rest, "__")
		return mcpCall(server, tool, input)
	}
	return name + " " + truncate(string(input), 200)
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

// readOnlyCommands are the shell commands a Shell workspace may run on
// Claude Code where its sandbox can't (Windows): prefix rules, so they
// are only as read-only as the commands are. Where the sandbox runs, it
// allows any command it wraps, and denies their writes and the network.
var readOnlyCommands = []string{
	"git log", "git show", "git blame", "git diff", "git grep", "git status", "git ls-files", "git ls-tree",
	"git rev-parse", "git rev-list", "git cat-file", "git branch", "git tag", "git describe", "git shortlog",
	"git merge-base", "git name-rev", "git for-each-ref", "git --version",
	"rg", "ls", "cat", "head", "tail", "wc", "sort", "uniq", "cut", "which", "command -v", "pwd",
}

// shellAllowed is the --allowedTools entry for readOnlyCommands. Bash on
// its own is never allowed: that would let a command run unsandboxed
// when the sandbox can't start.
func shellAllowed() string {
	rules := make([]string, len(readOnlyCommands))
	for i, c := range readOnlyCommands {
		rules[i] = "Bash(" + c + ":*)"
	}
	return strings.Join(rules, ",")
}

// claudeSettings is the --settings file. A Shell workspace turns on the
// sandbox: commands it wraps run without asking; they can't write to the
// workspace, the read directories or NoWrite (or anywhere outside the
// temp directory), can't reach the network, and can't read the usual
// credential stores.
func claudeSettings(ws *Workspace) ([]byte, error) {
	s := map[string]any{"disableAllHooks": true}
	if ws != nil && ws.Shell && !ws.Edit {
		deny := append(append([]string{ws.Dir}, ws.ReadDirs...), ws.NoWrite...)
		home, _ := os.UserHomeDir()
		var secrets []string
		if home != "" {
			for _, p := range []string{".ssh", ".aws", ".gnupg", ".netrc", ".config/gh", ".docker", ".kube", ".azure", ".config/gcloud", "Library/Keychains"} {
				secrets = append(secrets, filepath.Join(home, p))
			}
		}
		fsys := map[string]any{"denyWrite": deny, "denyRead": secrets}
		if ws.Builds != "" {
			fsys["allowWrite"] = []string{ws.Builds} // build output only (build.go)
		}
		s["sandbox"] = map[string]any{
			"enabled": true, "autoAllowBashIfSandboxed": true, "allowUnsandboxedCommands": false,
			"filesystem": fsys,
			"network":    map[string]any{"allowedDomains": []string{}},
		}
		// git must not take locks, ask for passwords or page.
		env := map[string]string{"GIT_OPTIONAL_LOCKS": "0", "GIT_TERMINAL_PROMPT": "0", "GIT_PAGER": "cat", "PAGER": "cat"}
		for k, v := range buildEnv(ws) {
			env[k] = v
		}
		s["env"] = env
	}
	return json.Marshal(s)
}

// editTools are what an editing workspace gets: the read-only tools, the
// file editors and subagents, which get the same tools. editAllowed lets
// the editors change files under the working directory only: an Edit
// rule covers every file-editing tool, Write included.
const (
	editTools   = readOnlyTools + ",Edit,Write,Agent"
	editAllowed = readOnlyTools + ",Edit(./**),Agent"
)

// codexSandbox is the codex sandbox for ws: writable in its directory
// when it edits, read-only otherwise.
func codexSandbox(ws *Workspace) string {
	if ws != nil && ws.Edit || codexBuilds(ws) {
		return "workspace-write"
	}
	return "read-only"
}

// cliPrompt flattens the chat into a system prompt and one user prompt.
// ws is the workspace the CLI was given to read or edit, if any.
func cliPrompt(msgs []ChatMessage, tool ToolDefinition, ws *Workspace) (string, string) {
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
	switch {
	case ws != nil && ws.Edit:
		fmt.Fprintf(&sb, "%s Make your changes by editing the files, then answer only with the JSON object.", tool.Description)
	case ws != nil && ws.Shell:
		fmt.Fprintf(&sb, "%s Read files and run read-only shell commands (rg, git log/show/blame/diff/grep, ls, cat, pipes) if you need to; you are in a sandbox where writes and the network fail, so don't try to change anything.%s%s Not every tool may be installed: check (rg --version, git --version) and use what there is. Answer only with the JSON object.", tool.Description, reachNote(ws), buildNote(ws))
	case ws != nil:
		fmt.Fprintf(&sb, "%s Read files if you need to, but do not change anything. Answer only with the JSON object.", tool.Description)
	default:
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
// for at most timeout, and returns stdout. Its output goes to ctx's activity
// log as it comes, stdout lines through format.
func runCLI(ctx context.Context, bin string, args []string, dir, prompt string, timeout time.Duration, format func(string) string, unset ...string) ([]byte, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
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
		if ctx.Err() == context.DeadlineExceeded && parent.Err() == nil {
			return nil, fmt.Errorf("timed out after %s", timeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			// The last event; the first is a stream's start-up noise.
			out := strings.TrimSpace(stdout.String())
			msg = out[strings.LastIndexByte(out, '\n')+1:]
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

// KeepClaudeCodeEnv is the env var that, set to 1, runs claude with the
// environment as it is, for a Claude Code deliberately routed through a
// gateway or a cloud provider.
const KeepClaudeCodeEnv = "PR_MANAGER_CLAUDE_CODE_KEEP_ENV"

// claudeCodeRerouting are the variables that take Claude Code off the
// claude.ai login: API credentials, another endpoint or cloud provider, and
// model overrides (the model is passed with --model). This app reads the
// same variables for its own claude-api, bedrock, vertex and foundry
// providers, so they are often set. CLAUDE_CODE_OAUTH_TOKEN is a
// subscription token and stays.
var claudeCodeRerouting = []string{
	// Credentials other than the claude.ai login.
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
	// Workload identity federation.
	"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_SERVICE_ACCOUNT_ID",
	"ANTHROPIC_IDENTITY_TOKEN", "ANTHROPIC_IDENTITY_TOKEN_FILE", "ANTHROPIC_SCOPE",
	// Another endpoint.
	"ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_UNIX_SOCKET",
	// Cloud providers and gateways.
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	"CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
	"CLAUDE_CODE_USE_GATEWAY", "AWS_BEARER_TOKEN_BEDROCK", "CLOUD_ML_REGION",
	// Model overrides.
	"ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION",
	"CLAUDE_CODE_SUBAGENT_MODEL",
}

// claudeCodeReroutingPrefixes are families of the same: per-provider
// endpoints and settings, per-model regions and the ANTHROPIC_DEFAULT_*_MODEL
// aliases.
var claudeCodeReroutingPrefixes = []string{
	"ANTHROPIC_BEDROCK_", "ANTHROPIC_VERTEX_", "ANTHROPIC_FOUNDRY_", "ANTHROPIC_AWS_",
	"ANTHROPIC_GOOGLE_CLOUD_", "VERTEX_REGION_", "ANTHROPIC_DEFAULT_",
}

// claudeCodeUnset is what to remove from claude's environment so it runs on
// the claude.ai subscription: the rerouting variables that are set, or none
// if KeepClaudeCodeEnv is 1.
func claudeCodeUnset() []string {
	if os.Getenv(KeepClaudeCodeEnv) == "1" {
		return nil
	}
	unset := slices.Clone(claudeCodeRerouting)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		for _, p := range claudeCodeReroutingPrefixes {
			if strings.HasPrefix(k, p) {
				unset = append(unset, k)
				break
			}
		}
	}
	return unset
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
		out, err := probe(ctx, []string{"OPENAI_API_KEY", "CODEX_API_KEY"}, "codex", "login", "status")
		return err == nil && strings.Contains(out, "ChatGPT")
	case "claude-code":
		// The same environment as a run, so the answer is what a run uses.
		out, err := probe(ctx, claudeCodeUnset(), "claude", "auth", "status")
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

func probe(ctx context.Context, unset []string, bin string, args ...string) (string, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return "", err
	}
	cmd := proc.CommandContext(ctx, bin, args...)
	ownGroup(cmd)
	cmd.Env = cliEnv(unset...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
