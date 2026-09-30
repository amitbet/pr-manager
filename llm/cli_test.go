package llm

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStrictSchemaMakesOptionalNullable(t *testing.T) {
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"bucket": map[string]any{"type": "string", "enum": []string{"a", "b"}},
			"reason": map[string]any{"type": "string"},
			"issues": map[string]any{"type": "array", "items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title": map[string]any{"type": "string"},
					"line":  map[string]any{"type": "integer"},
				},
				"required": []string{"title"},
			}},
		},
		"required": []string{"bucket", "issues"},
	}
	out := strictSchema(in).(map[string]any)
	req := out["required"].([]string)
	sort.Strings(req)
	if !reflect.DeepEqual(req, []string{"bucket", "issues", "reason"}) || out["additionalProperties"] != false {
		t.Fatalf("top level: %v", out)
	}
	props := out["properties"].(map[string]any)
	if got := props["reason"].(map[string]any)["type"]; !reflect.DeepEqual(got, []any{"string", "null"}) {
		t.Fatalf("reason type = %v", got)
	}
	if got := props["bucket"].(map[string]any)["type"]; got != "string" {
		t.Fatalf("required bucket changed: %v", got)
	}
	item := props["issues"].(map[string]any)["items"].(map[string]any)
	if got := item["properties"].(map[string]any)["line"].(map[string]any)["type"]; !reflect.DeepEqual(got, []any{"integer", "null"}) {
		t.Fatalf("line type = %v", got)
	}
	// The input is left alone.
	if _, ok := in["additionalProperties"]; ok {
		t.Fatal("input mutated")
	}
}

func TestStructuredResponseDropsNulls(t *testing.T) {
	r, err := structuredResponse(ToolDefinition{Name: "submit"}, "```json\n{\"a\":1,\"b\":null,\"c\":[{\"d\":null}]}\n```", nil, Usage{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(r.ToolCalls[0].Arguments)
	if string(got) != `{"a":1,"c":[{}]}` || r.ToolCalls[0].Name != "submit" {
		t.Fatalf("got %s", got)
	}
}

// fakeCLI builds a local executable that records its args and working
// directory, then prints the configured response.
func fakeCLI(t *testing.T, out string) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	bin = filepath.Join(dir, "cli")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	src := filepath.Join(dir, "cli.go")
	const program = `package main
import ("encoding/json"; "fmt"; "io"; "os")
func main() {
	args, _ := json.Marshal(os.Args[1:])
	_ = os.WriteFile(os.Getenv("PR_MANAGER_FAKE_CLI_ARGS"), args, 0600)
	cwd, _ := os.Getwd()
	_ = os.WriteFile(os.Getenv("PR_MANAGER_FAKE_CLI_ARGS")+".cwd", []byte(cwd), 0600)
	for i, a := range os.Args {
		if a == "--system-prompt-file" && i+1 < len(os.Args) {
			b, _ := os.ReadFile(os.Args[i+1])
			_ = os.WriteFile(os.Getenv("PR_MANAGER_FAKE_CLI_ARGS")+".system", b, 0600)
		}
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	fmt.Println(os.Getenv("PR_MANAGER_FAKE_CLI_OUT"))
}`
	if err := os.WriteFile(src, []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build fake CLI: %v: %s", err, output)
	}
	t.Setenv("PR_MANAGER_FAKE_CLI_ARGS", argsFile)
	t.Setenv("PR_MANAGER_FAKE_CLI_OUT", out)
	return bin, argsFile
}

func TestClaudeCodeWorkspaceEnablesReadOnlyTools(t *testing.T) {
	tool := ToolDefinition{Name: "submit", Description: "Submit.", InputSchema: map[string]any{"type": "object"}}
	call := func(ws *Workspace) ([]string, string, string) {
		bin, argsFile := fakeCLI(t, `{"is_error":false,"result":"","structured_output":{"a":1}}`)
		c := &ClaudeCodeCLI{Binary: bin}
		var prompt string
		if _, err := c.Call(context.Background(), LLMRequest{
			Messages: []ChatMessage{{Role: "user", Content: "hi"}}, Tools: []ToolDefinition{tool},
			ToolChoice: ToolChoiceRequired, Workspace: ws,
		}); err != nil {
			t.Fatal(err)
		}
		_, prompt = cliPrompt([]ChatMessage{{Role: "user", Content: "hi"}}, tool, ws != nil)
		b, _ := os.ReadFile(argsFile)
		cwd, _ := os.ReadFile(argsFile + ".cwd")
		var args []string
		if err := json.Unmarshal(b, &args); err != nil {
			t.Fatal(err)
		}
		return args, strings.TrimSpace(string(cwd)), prompt
	}
	flag := func(args []string, name string) []string {
		var vals []string
		for i, a := range args {
			if a == name && i+1 < len(args) {
				vals = append(vals, args[i+1])
			}
		}
		return vals
	}

	args, _, prompt := call(nil)
	if got := flag(args, "--tools"); len(got) != 1 || got[0] != "" || len(flag(args, "--add-dir")) != 0 || !strings.Contains(prompt, "do not run commands or read files") {
		t.Errorf("no workspace: args %q", args)
	}
	repo, lib := t.TempDir(), t.TempDir()
	args, cwd, prompt := call(&Workspace{Dir: repo, ReadDirs: []string{lib}})
	if got := flag(args, "--tools"); len(got) != 1 || got[0] != readOnlyTools {
		t.Errorf("--tools = %q", got)
	}
	if got := flag(args, "--allowedTools"); len(got) != 1 || got[0] != readOnlyTools {
		t.Errorf("--allowedTools = %q", got)
	}
	if got := flag(args, "--add-dir"); len(got) != 1 || got[0] != lib {
		t.Errorf("--add-dir = %q", got)
	}
	gotDir, gotErr := os.Stat(cwd)
	wantDir, wantErr := os.Stat(repo)
	if gotErr != nil || wantErr != nil || !os.SameFile(gotDir, wantDir) {
		t.Errorf("cwd = %q, want %q", cwd, repo)
	}
	if strings.Contains(prompt, "do not run commands") {
		t.Errorf("prompt still forbids reading: %q", prompt)
	}
}

// A claude.cmd shim runs through cmd.exe, which ends the command line at
// the first newline and expands % and ^: nothing multi-line or with cmd
// syntax may be an argument.
func TestClaudeCodeArgsSurviveCmdShim(t *testing.T) {
	system := "You review code.\nLine two: 100% sure ^ | & < > !\r\nLine three."
	tool := ToolDefinition{Name: "submit", Description: "Submit.", InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"why": map[string]any{"type": "string", "description": "Say why.\nUse 50% or less | <b> & ^ !"},
		},
	}}
	bin, argsFile := fakeCLI(t, `{"is_error":false,"result":"","structured_output":{"why":"x"}}`)
	c := &ClaudeCodeCLI{Binary: bin}
	if _, err := c.Call(context.Background(), LLMRequest{
		Messages: []ChatMessage{{Role: "system", Content: system}, {Role: "user", Content: "hi"}},
		Tools:    []ToolDefinition{tool}, ToolChoice: ToolChoiceRequired, Workspace: &Workspace{Dir: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argsFile)
	var args []string
	if err := json.Unmarshal(b, &args); err != nil {
		t.Fatal(err)
	}
	if err := cmdUnsafe("claude.cmd", args); err != nil {
		t.Errorf("args won't survive cmd.exe: %v", err)
	}
	for i, a := range args {
		if a == "--system-prompt" {
			t.Errorf("system prompt on the command line: %q", args[i+1])
		}
		if a == "--json-schema" {
			var got map[string]any
			if err := json.Unmarshal([]byte(args[i+1]), &got); err != nil {
				t.Fatalf("schema: %v", err)
			}
			want, _ := json.Marshal(tool.InputSchema)
			if g, _ := json.Marshal(got); string(g) != string(want) {
				t.Errorf("schema = %s, want %s", g, want)
			}
		}
	}
	if got, _ := os.ReadFile(argsFile + ".system"); string(got) != system {
		t.Errorf("--system-prompt-file held %q, want %q", got, system)
	}
	if !slices.Contains(args, "--setting-sources") || args[slices.Index(args, "--setting-sources")+1] != "user" {
		t.Errorf("project settings and CLAUDE.md not skipped: %q", args)
	}
}

func TestCmdUnsafe(t *testing.T) {
	if err := cmdUnsafe(`C:\npm\claude.CMD`, []string{"-p", "a\nb"}); err == nil {
		t.Error("newline passed to a .cmd shim")
	}
	if err := cmdUnsafe(`C:\npm\claude.bat`, []string{"100%"}); err == nil {
		t.Error("% passed to a .bat shim")
	}
	if err := cmdUnsafe(`C:\bin\claude.exe`, []string{"a\nb%"}); err != nil {
		t.Errorf("an .exe gets its args verbatim: %v", err)
	}
}

func TestCodexSkipsProjectDocs(t *testing.T) {
	bin, argsFile := fakeCLI(t, `{"type":"item.completed","item":{"type":"agent_message","text":"{\"a\":1}"}}`)
	c := &CodexCLI{Binary: bin}
	tool := ToolDefinition{Name: "submit", Description: "Submit.", InputSchema: map[string]any{"type": "object"}}
	if _, err := c.Call(context.Background(), LLMRequest{
		Messages: []ChatMessage{{Role: "system", Content: "a\nb"}, {Role: "user", Content: "hi"}},
		Tools:    []ToolDefinition{tool}, ToolChoice: ToolChoiceRequired, Workspace: &Workspace{Dir: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argsFile)
	var args []string
	if err := json.Unmarshal(b, &args); err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(args, "project_doc_max_bytes=0"); i < 1 || args[i-1] != "--config" {
		t.Errorf("AGENTS.md not skipped: %q", args)
	}
	if err := cmdUnsafe("codex.cmd", args); err != nil {
		t.Errorf("args won't survive cmd.exe: %v", err)
	}
}

func TestStructuredResponseNull(t *testing.T) {
	if _, err := structuredResponse(ToolDefinition{Name: "submit"}, "null", nil, Usage{}); err == nil {
		t.Fatal("want an error for a null answer")
	}
}

// A grandchild holding the CLI's output pipes open must not keep runCLI
// waiting past its context.
func TestRunCLIKillsGrandchildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := runCLI(ctx, "sh", []string{"-c", "sleep 30 & sleep 30"}, t.TempDir(), "", nil); err == nil {
		t.Fatal("want a timeout error")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("runCLI returned after %v", d)
	}
}

// Logging in after the first check shows up once the cache is forgotten
// (Catalogs with refresh) or the negative result is older than subsRetry.
func TestHasSubscriptionRechecks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	forgetSubscriptions()
	defer forgetSubscriptions()
	if HasSubscription("codex") {
		t.Fatal("codex found without a codex binary")
	}
	script := "#!/bin/sh\necho 'Logged in using ChatGPT'\n"
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if HasSubscription("codex") {
		t.Fatal("a negative result is kept for subsRetry")
	}
	forgetSubscriptions()
	if !HasSubscription("codex") {
		t.Fatal("codex login not seen after forgetSubscriptions")
	}
	forgetSubscriptions()
	if err := os.Remove(filepath.Join(dir, "codex")); err != nil {
		t.Fatal(err)
	}
	if HasSubscription("codex") {
		t.Fatal("codex found without a codex binary")
	}
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ageSubscription("codex")
	if !HasSubscription("codex") {
		t.Fatal("codex login not seen after subsRetry")
	}
}

// ageSubscription makes provider's negative result older than subsRetry.
func ageSubscription(provider string) {
	subsMu.Lock()
	if st := subs[provider]; st != nil {
		st.at = time.Now().Add(-subsRetry)
	}
	subsMu.Unlock()
}

// Each provider is cached on its own: a found subscription survives a
// later failing probe of either CLI, and checking one CLI never runs the
// other.
func TestHasSubscriptionPerProvider(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	log := filepath.Join(dir, "calls")
	write := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\necho "+name+" >> '"+log+"'\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	calls := func() string {
		b, _ := os.ReadFile(log)
		os.Remove(log)
		return strings.TrimSpace(string(b))
	}
	forgetSubscriptions()
	defer forgetSubscriptions()
	write("claude", `echo '{"loggedIn":true,"authMethod":"claude.ai"}'`+"\n")
	write("codex", "exit 1\n")
	if !HasSubscription("claude-code") {
		t.Fatal("claude subscription not found")
	}
	if c := calls(); c != "claude" {
		t.Errorf("probes = %q, want claude only", c)
	}
	if HasSubscription("codex") {
		t.Fatal("codex found though its probe fails")
	}
	calls()
	ageSubscription("codex")
	write("claude", "exit 1\n") // a transient failure
	if HasSubscription("codex") || !HasSubscription("claude-code") {
		t.Fatal("want codex missing and claude kept")
	}
	if c := calls(); c != "codex" {
		t.Errorf("probes = %q, want codex only", c)
	}
}

// A slow probe of one CLI runs once for all its callers and doesn't hold
// up the other.
func TestHasSubscriptionConcurrent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	log := filepath.Join(dir, "calls")
	claude := "#!/bin/sh\necho claude >> '" + log + "'\nsleep 1\necho '{\"loggedIn\":true,\"authMethod\":\"claude.ai\"}'\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(claude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\necho 'Logged in using ChatGPT'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	forgetSubscriptions()
	defer forgetSubscriptions()
	var wg sync.WaitGroup
	results := make([]bool, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = HasSubscription("claude-code")
		}()
	}
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if !HasSubscription("codex") {
		t.Error("codex subscription not found")
	}
	if d := time.Since(start); d > 700*time.Millisecond {
		t.Errorf("codex waited %v for the claude probe", d)
	}
	wg.Wait()
	for i, ok := range results {
		if !ok {
			t.Errorf("caller %d: claude subscription not found", i)
		}
	}
	if b, _ := os.ReadFile(log); strings.Count(string(b), "claude") != 1 {
		t.Errorf("claude probed %d times, want once", strings.Count(string(b), "claude"))
	}
}

// Claude Code runs on the claude.ai login: the variables this app reads for
// its own API and cloud providers, which would send claude to another
// endpoint, provider or model, are removed unless KeepClaudeCodeEnv is 1.
func TestClaudeCodeEnvDropsRerouting(t *testing.T) {
	rerouting := map[string]string{
		"ANTHROPIC_API_KEY":              "sk-ant",
		"ANTHROPIC_AUTH_TOKEN":           "tok",
		"ANTHROPIC_BASE_URL":             "https://gateway.example.com",
		"ANTHROPIC_CUSTOM_HEADERS":       "X-A: b",
		"CLAUDE_CODE_USE_BEDROCK":        "1",
		"CLAUDE_CODE_USE_VERTEX":         "1",
		"CLAUDE_CODE_USE_FOUNDRY":        "1",
		"AWS_BEARER_TOKEN_BEDROCK":       "bedrock",
		"ANTHROPIC_BEDROCK_BASE_URL":     "https://bedrock.example.com",
		"ANTHROPIC_VERTEX_PROJECT_ID":    "proj",
		"CLOUD_ML_REGION":                "us-east5",
		"VERTEX_REGION_CLAUDE_HAIKU_4_5": "us-east5",
		"ANTHROPIC_FOUNDRY_RESOURCE":     "res",
		"ANTHROPIC_MODEL":                "us.anthropic.claude-sonnet-5",
		"ANTHROPIC_SMALL_FAST_MODEL":     "us.anthropic.claude-haiku-4-5",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  "us.anthropic.claude-haiku-4-5",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   "us.anthropic.claude-opus-5-5",
		"CLAUDE_CODE_SUBAGENT_MODEL":     "claude-haiku-4-5",
	}
	kept := map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat",
		"AWS_PROFILE":             "dev",
		"PR_MANAGER_TEST_KEEP":    "x",
	}
	for k, v := range rerouting {
		t.Setenv(k, v)
	}
	for k, v := range kept {
		t.Setenv(k, v)
	}
	envMap := func() map[string]string {
		m := map[string]string{}
		for _, kv := range cliEnv(claudeCodeUnset()...) {
			k, v, _ := strings.Cut(kv, "=")
			m[k] = v
		}
		return m
	}

	t.Setenv(KeepClaudeCodeEnv, "")
	env := envMap()
	for k := range rerouting {
		if _, ok := env[k]; ok {
			t.Errorf("%s passed to claude", k)
		}
	}
	for k, v := range kept {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}

	t.Setenv(KeepClaudeCodeEnv, "1")
	env = envMap()
	for k, v := range rerouting {
		if env[k] != v {
			t.Errorf("with %s=1: %s = %q, want %q", KeepClaudeCodeEnv, k, env[k], v)
		}
	}
}

// The subscription probe sees the environment a run gets: a Bedrock setup
// in the app's environment doesn't hide the claude.ai login.
func TestHasSubscriptionClaudeCodeEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	script := `#!/bin/sh
if [ -n "$CLAUDE_CODE_USE_BEDROCK" ]; then
  echo '{"loggedIn":true,"authMethod":"third_party"}'
else
  echo '{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}'
fi
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	forgetSubscriptions()
	defer forgetSubscriptions()
	t.Setenv(KeepClaudeCodeEnv, "")
	if !HasSubscription("claude-code") {
		t.Fatal("claude.ai login hidden by CLAUDE_CODE_USE_BEDROCK")
	}
	forgetSubscriptions()
	t.Setenv(KeepClaudeCodeEnv, "1")
	if HasSubscription("claude-code") {
		t.Fatalf("with %s=1 the probe should see Bedrock", KeepClaudeCodeEnv)
	}
}
