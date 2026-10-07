package llm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeCall runs a CLI through fakeCLI and returns its arguments.
func fakeCall(t *testing.T, l LLMTool, out string, ws *Workspace) []string {
	t.Helper()
	bin, argsFile := fakeCLI(t, out)
	switch c := l.(type) {
	case *ClaudeCodeCLI:
		c.Binary = bin
	case *CodexCLI:
		c.Binary = bin
	}
	tool := ToolDefinition{Name: "reply", Description: "Reply.", InputSchema: map[string]any{"type": "object"}}
	if _, err := l.Call(context.Background(), LLMRequest{
		Messages: []ChatMessage{{Role: "system", Content: "the material"}, {Role: "user", Content: "hi"}},
		Tools:    []ToolDefinition{tool}, ToolChoice: ToolChoiceRequired, Workspace: ws,
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argsFile)
	var args []string
	if err := json.Unmarshal(b, &args); err != nil {
		t.Fatal(err)
	}
	return args
}

func argOf(args []string, name string) (string, bool) {
	i := slices.Index(args, name)
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

func TestClaudeCodeInstalled(t *testing.T) {
	repo, lib, cache := t.TempDir(), t.TempDir(), t.TempDir()
	ws := &Workspace{Dir: repo, ReadDirs: []string{lib}, NoWrite: []string{cache}, Shell: true, Installed: true,
		Web: true, MCP: []MCPServer{{Name: "prm", Command: "pr-manager", Args: []string{"mcp"}, Tools: []string{"gh"}}}}
	args := fakeCall(t, &ClaudeCodeCLI{}, `{"type":"result","is_error":false,"result":"","structured_output":{"answer":"ok"}}`, ws)
	for _, a := range []string{"--tools", "--strict-mcp-config", "--disable-slash-commands", "--system-prompt-file"} {
		if slices.Contains(args, a) {
			t.Errorf("installed has %s: %q", a, args)
		}
	}
	if v, _ := argOf(args, "--permission-mode"); v != "acceptEdits" {
		t.Errorf("--permission-mode = %q", v)
	}
	if v, _ := argOf(args, "--setting-sources"); v != "user" {
		t.Errorf("--setting-sources = %q, want user for an untrusted checkout", v)
	}
	if _, ok := argOf(args, "--append-system-prompt-file"); !ok {
		t.Errorf("no --append-system-prompt-file: %q", args)
	}
	if _, ok := argOf(args, "--mcp-config"); !ok {
		t.Errorf("the app's MCP server is missing: %q", args)
	}
	if v, _ := argOf(args, "--allowedTools"); !strings.Contains(v, "mcp__prm__gh") || !strings.Contains(v, "WebFetch") {
		t.Errorf("--allowedTools = %q", v)
	}
	deny, _ := argOf(args, "--disallowedTools")
	for _, want := range []string{"Bash(gh:*)", "Bash(git push:*)", "Edit(" + absRule(lib) + "/**)", "Edit(" + absRule(cache) + "/**)"} {
		if !strings.Contains(deny, want) {
			t.Errorf("--disallowedTools = %q, missing %s", deny, want)
		}
	}
	if strings.Contains(deny, "WebFetch") {
		t.Errorf("web denied with Web on: %q", deny)
	}
	if v, _ := argOf(args, "--add-dir"); v != lib {
		t.Errorf("--add-dir = %q", v)
	}
	if v, _ := argOf(args, "--json-schema"); v == "" {
		t.Errorf("no --json-schema: the answer and its actions need it")
	}
	if err := cmdUnsafe("claude.cmd", args); err != nil {
		t.Errorf("args won't survive cmd.exe: %v", err)
	}

	// A trusted checkout loads its project settings; no web denies it.
	args = fakeCall(t, &ClaudeCodeCLI{}, `{"type":"result","is_error":false,"result":"","structured_output":{"answer":"ok"}}`, &Workspace{Dir: repo, Installed: true, Project: true})
	if v, _ := argOf(args, "--setting-sources"); v != "user,project,local" {
		t.Errorf("trusted --setting-sources = %q", v)
	}
	if deny, _ := argOf(args, "--disallowedTools"); !strings.Contains(deny, "WebFetch") || !strings.Contains(deny, "WebSearch") {
		t.Errorf("web not denied with Web off: %q", deny)
	}
}

func TestInstalledDeniesOnlyOtherDirs(t *testing.T) {
	// The cache holds the conversation's worktree: it stays editable.
	cache := t.TempDir()
	code := filepath.Join(cache, "chats", "x", "code")
	args := claudeInstalledArgs(&Workspace{Dir: code, NoWrite: []string{cache}, ReadDirs: []string{filepath.Join(cache, "chats", "x", "material")}}, "")
	deny, _ := argOf(args, "--disallowedTools")
	if strings.Contains(deny, "Edit("+absRule(cache)+"/**)") {
		t.Errorf("denies editing the working directory: %q", deny)
	}
	if !strings.Contains(deny, "material/**") {
		t.Errorf("material editable: %q", deny)
	}
}

func TestClaudeSettingsInstalled(t *testing.T) {
	b, err := claudeSettings(&Workspace{Dir: t.TempDir(), Installed: true, Shell: true})
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	_ = json.Unmarshal(b, &s)
	if _, ok := s["disableAllHooks"]; ok {
		t.Errorf("installed turns off the reader's hooks: %s", b)
	}
	if _, ok := s["sandbox"]; ok {
		t.Errorf("installed replaces the reader's sandbox: %s", b)
	}
}

func TestCodexInstalled(t *testing.T) {
	out := `{"type":"item.completed","item":{"type":"agent_message","text":"{\"answer\":\"ok\"}"}}`
	args := fakeCall(t, &CodexCLI{}, out, &Workspace{Dir: t.TempDir(), Installed: true, Shell: true})
	for _, a := range []string{"--ignore-user-config", "--ignore-rules"} {
		if slices.Contains(args, a) {
			t.Errorf("installed has %s: %q", a, args)
		}
	}
	if v, _ := argOf(args, "-s"); v != "workspace-write" {
		t.Errorf("-s = %q", v)
	}
	if !slices.Contains(args, "sandbox_workspace_write.network_access=false") || !slices.Contains(args, "project_doc_max_bytes=0") {
		t.Errorf("args = %q", args)
	}
	args = fakeCall(t, &CodexCLI{}, out, &Workspace{Dir: t.TempDir(), Installed: true, Project: true})
	if slices.Contains(args, "project_doc_max_bytes=0") {
		t.Errorf("trusted checkout skips AGENTS.md: %q", args)
	}
	// Read-only is as it was.
	args = fakeCall(t, &CodexCLI{}, out, &Workspace{Dir: t.TempDir(), Shell: true})
	if !slices.Contains(args, "--ignore-user-config") || !slices.Contains(args, "--ignore-rules") {
		t.Errorf("read-only args = %q", args)
	}
}
