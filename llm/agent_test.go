package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// scripted answers with its responses in turn and keeps the requests.
type scripted struct {
	resps []*LLMResponse
	reqs  []LLMRequest
}

func (s *scripted) Call(_ context.Context, req LLMRequest) (*LLMResponse, error) {
	s.reqs = append(s.reqs, req)
	r := s.resps[0]
	s.resps = s.resps[1:]
	return r, nil
}
func (s *scripted) ModelID() string { return "m" }
func (s *scripted) Name() string    { return "test" }

var replyTool = ToolDefinition{Name: "reply", InputSchema: map[string]any{"type": "object"}}

func call(name string, args map[string]any) *LLMResponse {
	return &LLMResponse{ToolCalls: []ToolCall{{Name: name, Arguments: args}}, Usage: Usage{InputTokens: 10, OutputTokens: 1}}
}

// The loop runs the model's tool calls, sends their output back with the
// call's id, asks again after a text answer, and ends at the final tool.
func TestCallWithTools(t *testing.T) {
	echo := LocalTool{Def: ToolDefinition{Name: "echo"}, Run: func(_ context.Context, a map[string]any) (string, error) {
		return "said " + argStr(a, "s"), nil
	}}
	l := &scripted{resps: []*LLMResponse{
		call("echo", map[string]any{"s": "hi"}),
		{Text: "I think so."},
		call("nope", nil),
		call("reply", map[string]any{"answer": "done"}),
	}}
	args, usage, err := CallWithTools(context.Background(), l, []ChatMessage{{Role: "user", Content: "q"}}, replyTool, []LocalTool{echo}, 100)
	if err != nil || args["answer"] != "done" {
		t.Fatalf("args %v, err %v", args, err)
	}
	if usage.InputTokens != 30 {
		t.Errorf("usage %+v", usage)
	}
	if len(l.reqs) != 4 || l.reqs[0].ToolChoice != ToolChoiceAny || l.reqs[0].Tools[0].Name != "reply" {
		t.Fatalf("requests %+v", l.reqs)
	}
	m := l.reqs[1].Messages
	if a, r := m[1], m[2]; len(a.ToolCalls) != 1 || a.ToolCalls[0].CallID == "" || r.ToolResults[0].CallID != a.ToolCalls[0].CallID || r.ToolResults[0].Content != "said hi" {
		t.Errorf("round 1: %+v", m)
	}
	if last := l.reqs[2].Messages; last[len(last)-1].Role != "user" || !strings.Contains(last[len(last)-1].Content, "reply tool") {
		t.Errorf("no nudge after text: %+v", last)
	}
	if last := l.reqs[3].Messages; !last[len(last)-1].ToolResults[0].IsError {
		t.Errorf("an unknown tool is not an error: %+v", last)
	}
}

// Out of rounds, the model must answer.
func TestCallWithToolsLastRound(t *testing.T) {
	l := &scripted{}
	for range toolRounds - 1 {
		l.resps = append(l.resps, call("echo", nil))
	}
	l.resps = append(l.resps, call("reply", map[string]any{"answer": "ok"}))
	echo := LocalTool{Def: ToolDefinition{Name: "echo"}, Run: func(context.Context, map[string]any) (string, error) { return "x", nil }}
	if _, _, err := CallWithTools(context.Background(), l, []ChatMessage{{Role: "user", Content: "q"}}, replyTool, []LocalTool{echo}, 100); err != nil {
		t.Fatal(err)
	}
	if got := l.reqs[len(l.reqs)-1].ToolChoice; got != ToolChoiceRequired {
		t.Errorf("last round %s", got)
	}
}

// An API provider asked for an editing workspace says it can't.
func TestCallToolInEdit(t *testing.T) {
	_, _, err := CallToolReading(context.Background(), &AnthropicLLM{}, &Workspace{Dir: t.TempDir(), Edit: true}, nil, replyTool, 10)
	if err == nil || !strings.Contains(err.Error(), "can't edit") {
		t.Errorf("err %v", err)
	}
}

func testTools(t *testing.T) (*wsTools, string, string) {
	t.Helper()
	dir, other, outside := t.TempDir(), t.TempDir(), t.TempDir()
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var long strings.Builder
	for i := 1; i <= 1200; i++ {
		long.WriteString("line\n")
	}
	write(filepath.Join(dir, "main.go"), "package main\n\nfunc main() {\n\tHello()\n}\n")
	write(filepath.Join(dir, "pkg", "hello.go"), "package main\n\nfunc Hello() {}\n")
	write(filepath.Join(dir, "pkg", "long.txt"), long.String())
	write(filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(filepath.Join(other, "notes.md"), "Hello from the bundle\n")
	write(filepath.Join(outside, "secret"), "Hello secret\n")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	ts, err := newWorkspaceTools(&Workspace{Dir: dir, ReadDirs: []string{other}, Shell: true})
	if err != nil {
		t.Fatal(err)
	}
	return ts, other, outside
}

func run(t *testing.T, ts *wsTools, name string, args map[string]any) (string, error) {
	t.Helper()
	for _, tool := range ts.tools() {
		if tool.Def.Name == name {
			return tool.Run(context.Background(), args)
		}
	}
	t.Fatalf("no tool %s", name)
	return "", nil
}

// Paths resolve under the working directory or a read directory, and
// nowhere else, through .. or a symlink.
func TestWorkspaceToolsConfined(t *testing.T) {
	ts, other, outside := testTools(t)
	// An absolute path of the system's, outside every place: /etc/hosts is
	// relative on Windows, so there it is the drive's.
	system := "/etc/hosts"
	if v := filepath.VolumeName(outside); v != "" {
		system = v + `\Windows\System32\drivers\etc\hosts`
	}
	for _, p := range []string{"../" + filepath.Base(outside) + "/secret", filepath.Join(outside, "secret"), "link", system} {
		if _, err := run(t, ts, "read_file", map[string]any{"path": p}); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Errorf("read %s: %v", p, err)
		}
	}
	if out, err := run(t, ts, "read_file", map[string]any{"path": filepath.Join(other, "notes.md")}); err != nil || !strings.Contains(out, "bundle") {
		t.Errorf("read dir: %q %v", out, err)
	}
	if _, err := run(t, ts, "grep", map[string]any{"pattern": "Hello", "path": outside}); err == nil {
		t.Error("grep outside")
	}
	if out, _ := run(t, ts, "grep", map[string]any{"pattern": "secret"}); strings.Contains(out, "Hello secret") {
		t.Errorf("grep followed the symlink: %s", out)
	}
}

func TestReadFile(t *testing.T) {
	ts, _, _ := testTools(t)
	out, err := run(t, ts, "read_file", map[string]any{"path": "main.go"})
	if err != nil || !strings.Contains(out, "main.go, lines 1-5 of 5") || !strings.Contains(out, "     4\t\tHello()") {
		t.Errorf("%q %v", out, err)
	}
	out, _ = run(t, ts, "read_file", map[string]any{"path": "pkg/long.txt"})
	if !strings.Contains(out, "lines 1-500 of 1200") || !strings.Contains(out, "read on with offset 501") {
		t.Errorf("page 1: %s", out[:80])
	}
	out, _ = run(t, ts, "read_file", map[string]any{"path": "pkg/long.txt", "offset": float64(1150), "limit": float64(100)})
	if !strings.Contains(out, "lines 1150-1200 of 1200") {
		t.Errorf("last page: %s", out[:80])
	}
	if _, err := run(t, ts, "read_file", map[string]any{"path": "pkg"}); err == nil {
		t.Error("read a directory")
	}
}

func TestListGlobGrep(t *testing.T) {
	ts, _, _ := testTools(t)
	out, _ := run(t, ts, "list_dir", map[string]any{"depth": float64(2)})
	if !strings.Contains(out, "  pkg/\n    hello.go") || !strings.Contains(out, ".git/\n") || strings.Contains(out, "HEAD") {
		t.Errorf("list:\n%s", out)
	}
	for pattern, want := range map[string]string{"*.go": "main.go\npkg/hello.go\n", "pkg/*.go": "pkg/hello.go\n", "**/{hello,main}.go": "main.go\npkg/hello.go\n"} {
		if out, _ := run(t, ts, "glob", map[string]any{"pattern": pattern}); out != want {
			t.Errorf("glob %s = %q", pattern, out)
		}
	}
	check := func(how string) {
		out, err := run(t, ts, "grep", map[string]any{"pattern": "hello\\(", "ignore_case": true, "glob": "*.go"})
		if err != nil || !strings.Contains(out, "main.go:4:\tHello()") || !strings.Contains(out, "pkg/hello.go:3:func Hello()") {
			t.Errorf("%s grep: %q %v", how, out, err)
		}
		if out, _ := run(t, ts, "grep", map[string]any{"pattern": "Hello", "files_only": true, "path": "pkg"}); out != "pkg/hello.go\n" {
			t.Errorf("%s files: %q", how, out)
		}
	}
	if _, err := exec.LookPath("rg"); err == nil {
		check("rg")
	}
	t.Setenv("PATH", "")
	check("go")
}

// git runs commands that read, and none of the options that write a file,
// run a program or read outside the roots.
func TestGitTool(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ts, other, outside := testTools(t)
	for _, args := range [][]string{
		{"push"}, {"commit", "-m", "x"}, {"branch", "x"}, {"config", "x", "y"},
		{"diff", "--output=/tmp/x"}, {"log", "--out", "/tmp/x"}, {"diff", "--no-index", "a", "b"}, {"grep", "-O", "x"},
		{"grep", "-nOvim", "x"}, {"grep", "-f", "/etc/passwd"}, {"blame", "--contents", "/etc/passwd", "x"}, {"diff", "-O/etc/passwd"},
	} {
		if err := gitArgsOK(args); err == nil {
			t.Errorf("git %v allowed", args)
		}
	}
	for _, args := range [][]string{{"log", "-S", "Hello", "--oneline"}, {"blame", "-L", "1,3", "--ignore-rev", "abc", "x"}, {"diff", "--output-indicator-new=+", "--", "--output"}} {
		if err := gitArgsOK(args); err != nil {
			t.Errorf("git %v: %v", args, err)
		}
	}
	// Outside a repository, git diff compares any two files.
	if _, err := run(t, ts, "git", map[string]any{"args": []any{"diff", filepath.Join(outside, "secret"), "notes.md"}, "dir": other}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("diff outside: %v", err)
	}
	repo := ts.dir
	for _, c := range [][]string{{"init", "-q"}, {"add", "main.go"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "first"}} {
		cmd := exec.Command("git", c...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", c, err, out)
		}
	}
	out, err := run(t, ts, "git", map[string]any{"args": []any{"log", "--format=%s", "--", "main.go"}})
	if err != nil || strings.TrimSpace(out) != "first" {
		t.Errorf("log: %q %v", out, err)
	}
}

func TestNoGitWithoutShell(t *testing.T) {
	ts, err := newWorkspaceTools(&Workspace{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range ts.tools() {
		if tool.Def.Name == "git" {
			t.Error("git without Shell")
		}
	}
}

// loopMsgs is a tool round as each provider is sent it.
var loopMsgs = []ChatMessage{
	{Role: "system", Content: "sys"},
	{Role: "user", Content: "q"},
	{Role: "assistant", Content: "looking", ToolCalls: []ToolCall{{Name: "grep", CallID: "c1", Arguments: map[string]any{"pattern": "x"}}}},
	{Role: "user", ToolResults: []ToolResult{{CallID: "c1", Name: "grep", Content: "a.go:1:x"}}},
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestToolTurnsOnTheWire(t *testing.T) {
	req := LLMRequest{Messages: loopMsgs, Tools: []ToolDefinition{replyTool, {Name: "grep"}}, ToolChoice: ToolChoiceAny}

	a := jsonOf(t, (&AnthropicLLM{Model: "claude-sonnet-5"}).buildPayload(req))
	for _, want := range []string{`{"text":"looking","type":"text"},{"id":"c1","input":{"pattern":"x"},"name":"grep","type":"tool_use"}`,
		`{"cache_control":{"type":"ephemeral"},"content":"a.go:1:x","is_error":false,"tool_use_id":"c1","type":"tool_result"}`, `"tool_choice":{"type":"any"}`} {
		if !strings.Contains(a, want) {
			t.Errorf("anthropic lacks %s:\n%s", want, a)
		}
	}
	if a := jsonOf(t, (&AnthropicLLM{Model: "claude-opus-5-5"}).buildPayload(req)); !strings.Contains(a, `"tool_choice":{"type":"auto"}`) || !strings.Contains(a, "calling the reply tool") {
		t.Errorf("anthropic without forcing:\n%s", a)
	}

	o := jsonOf(t, (&OpenAILLM{Model: "gpt-5.4"}).buildPayload(req))
	for _, want := range []string{`{"content":"looking","role":"assistant","tool_calls":[{"function":{"arguments":"{\"pattern\":\"x\"}","name":"grep"},"id":"c1","type":"function"}]}`,
		`{"content":"a.go:1:x","role":"tool","tool_call_id":"c1"}`, `"tool_choice":"required"`} {
		if !strings.Contains(o, want) {
			t.Errorf("openai lacks %s:\n%s", want, o)
		}
	}
	r := jsonOf(t, (&OpenAILLM{Model: "gpt-5.4", Effort: "low"}).buildResponsesPayload(req))
	for _, want := range []string{`{"arguments":"{\"pattern\":\"x\"}","call_id":"c1","name":"grep","type":"function_call"}`, `{"call_id":"c1","output":"a.go:1:x","type":"function_call_output"}`} {
		if !strings.Contains(r, want) {
			t.Errorf("responses lack %s:\n%s", want, r)
		}
	}

	in := converseInput("amazon.nova-pro-v1:0", req, true)
	if len(in.Messages) != 3 {
		t.Fatalf("converse messages %+v", in.Messages)
	}
	use, ok := in.Messages[1].Content[1].(*brtypes.ContentBlockMemberToolUse)
	res, ok2 := in.Messages[2].Content[0].(*brtypes.ContentBlockMemberToolResult)
	if !ok || !ok2 || *use.Value.ToolUseId != "c1" || *res.Value.ToolUseId != "c1" || len(in.Messages[2].Content) != 1 {
		t.Errorf("converse %+v", in.Messages)
	}
	if _, ok := in.ToolConfig.ToolChoice.(*brtypes.ToolChoiceMemberAny); !ok {
		t.Errorf("converse choice %T", in.ToolConfig.ToolChoice)
	}
}

// A conversation larger than the window loses its oldest tool outputs,
// never the question.
func TestFitWindow(t *testing.T) {
	big := strings.Repeat("x", 5000)
	msgs := []ChatMessage{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []ToolCall{{Name: "grep", CallID: "1"}}},
		{Role: "user", ToolResults: []ToolResult{{CallID: "1", Content: big}}},
		{Role: "assistant", ToolCalls: []ToolCall{{Name: "grep", CallID: "2"}}},
		{Role: "user", ToolResults: []ToolResult{{CallID: "2", Content: big}}},
	}
	fitWindow(msgs, nil, 7000)
	if msgs[0].Content != "q" || !strings.Contains(msgs[2].ToolResults[0].Content, "dropped") || msgs[4].ToolResults[0].Content != big {
		t.Errorf("%+v", msgs)
	}
}

// Ollama's window is the loaded model's; a cloud model's or a large one
// doesn't count, and the other providers have none to fit.
func TestContextTokens(t *testing.T) {
	ctxLen, loads := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			if ctxLen == 0 {
				w.Write([]byte(`{"models":[]}`))
				return
			}
			fmt.Fprintf(w, `{"models":[{"name":"m:latest","context_length":%d}]}`, ctxLen)
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"m:latest"},{"name":"big:cloud","remote_host":"https://ollama.com:443"}]}`))
		case "/api/show":
			w.Write([]byte(`{"model_info":{"x.context_length":262144}}`))
		case "/api/generate":
			loads++
			ctxLen = 32768
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	if n := ContextTokens(context.Background(), &OllamaLLM{Model: "m", BaseURL: srv.URL}); n != 32768 || loads != 1 {
		t.Errorf("local: %d after %d loads", n, loads)
	}
	if n := ContextTokens(context.Background(), &OllamaLLM{Model: "big:cloud", BaseURL: srv.URL}); n != 0 {
		t.Errorf("cloud: %d", n)
	}
	if n := ContextTokens(context.Background(), &AnthropicLLM{}); n != 0 {
		t.Errorf("anthropic: %d", n)
	}
}

// A workspace with Web gives an API provider the fetch tool.
func TestWorkspaceFetchTool(t *testing.T) {
	ft := fetchTool()
	if ft.Def.Name != "fetch" {
		t.Fatalf("tool %q", ft.Def.Name)
	}
	if _, err := ft.Run(context.Background(), map[string]any{"url": "http://127.0.0.1:1/"}); err == nil {
		t.Error("fetched a local address")
	}
}
