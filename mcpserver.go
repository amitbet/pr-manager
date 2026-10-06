package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/internal/webfetch"
)

// pr-manager mcp is a stdio MCP server the chat agent's CLI starts, so the
// agent can reach GitHub and the web from its sandbox, which has no
// network, and still change nothing:
//
//	gh     the gh CLI with the reader's login, for commands that only read
//	       (ghReadOnly): PRs, issues, checks, runs, releases, search, and
//	       gh api GET or graphql queries
//	fetch  GET a public http(s) URL, as text
//
// It runs outside the sandbox, as the CLI's child, in the repository's
// clone (-dir), so gh finds the repo from its remote.

const (
	mcpOutputMax = 200 << 10
	mcpGHTimeout = 90 * time.Second
)

func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory gh runs in (the repository's clone)")
	tools := fs.String("tools", "gh,fetch", "tools to offer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s := &mcpServer{dir: *dir, tools: map[string]bool{}}
	for _, t := range strings.Split(*tools, ",") {
		s.tools[strings.TrimSpace(t)] = true
	}
	return s.serve(os.Stdin, os.Stdout)
}

type mcpServer struct {
	dir   string
	tools map[string]bool
}

type mcpRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// serve answers JSON-RPC requests, one per line, until in ends.
func (s *mcpServer) serve(in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		var req mcpRequest
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		if len(req.ID) == 0 { // a notification
			continue
		}
		result, err := s.handle(req)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if err != nil {
			resp["error"] = map[string]any{"code": -32601, "message": err.Error()}
		} else {
			resp["result"] = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *mcpServer) handle(req mcpRequest) (any, error) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		return map[string]any{
			"protocolVersion": orDefault(p.ProtocolVersion, "2025-06-18"),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "pr-manager", "version": version},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.list()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err
		}
		text, err := s.call(p.Name, p.Arguments)
		if err != nil {
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true}, nil
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}, nil
	}
	return nil, fmt.Errorf("no method %s", req.Method)
}

func (s *mcpServer) list() []any {
	readOnly := map[string]any{"readOnlyHint": true, "openWorldHint": true}
	var out []any
	if s.tools["gh"] {
		out = append(out, map[string]any{
			"name":        "gh",
			"description": "Run the GitHub CLI, logged in as the reader, for commands that only read: pr view/list/diff/checks/status, issue view/list, run view/list (--log for a failed job's log), workflow view/list, repo view, release view/list, label list, search, and gh api GET (or graphql queries, no mutations). Runs in the repository's clone, so `pr view 12` finds the repo; pass -R owner/repo for another. Examples: [\"pr\",\"checks\",\"12\"], [\"pr\",\"view\",\"12\",\"--comments\"], [\"api\",\"repos/o/r/pulls/12/comments\"].",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"args": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "gh's arguments, without gh"},
			}, "required": []string{"args"}},
			"annotations": readOnly,
		})
	}
	if s.tools["fetch"] {
		out = append(out, map[string]any{
			"name":        "fetch",
			"description": webfetch.Description,
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"url": map[string]any{"type": "string"},
			}, "required": []string{"url"}},
			"annotations": readOnly,
		})
	}
	return out
}

func (s *mcpServer) call(name string, raw json.RawMessage) (string, error) {
	if !s.tools[name] {
		return "", fmt.Errorf("no tool %s", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpGHTimeout)
	defer cancel()
	switch name {
	case "gh":
		var in struct {
			Args []string `json:"args"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", err
		}
		if err := ghReadOnly(in.Args); err != nil {
			return "", err
		}
		cmd := proc.CommandContext(ctx, "gh", in.Args...)
		cmd.Dir = s.dir
		cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_PAGER=cat", "PAGER=cat", "NO_COLOR=1", "GH_NO_UPDATE_NOTIFIER=1")
		var b bytes.Buffer
		cmd.Stdout, cmd.Stderr = &b, &b
		err := cmd.Run()
		text := clipText(b.String(), mcpOutputMax)
		if err != nil {
			return "", fmt.Errorf("gh %s: %v\n%s", strings.Join(in.Args, " "), err, text)
		}
		return text, nil
	case "fetch":
		var in struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", err
		}
		return webfetch.Fetch(ctx, in.URL)
	}
	return "", fmt.Errorf("no tool %s", name)
}

// ghRead are the gh commands that only read, by subcommand.
var ghRead = map[string][]string{
	"pr":       {"view", "list", "diff", "checks", "status"},
	"issue":    {"view", "list", "status"},
	"run":      {"view", "list"},
	"workflow": {"view", "list"},
	"repo":     {"view"},
	"release":  {"view", "list"},
	"label":    {"list"},
	"search":   {"repos", "issues", "prs", "commits", "code"},
}

// ghReadOnly reports why args aren't a gh command that only reads, or nil.
func ghReadOnly(args []string) error {
	if len(args) == 0 {
		return errors.New("no gh command")
	}
	for _, a := range args {
		if a == "-w" || a == "--web" || strings.HasPrefix(a, "--web=") || a == "--watch" {
			return fmt.Errorf("%s is not allowed", a)
		}
	}
	cmd := args[0]
	if cmd == "api" {
		return ghAPIReadOnly(args[1:])
	}
	subs, ok := ghRead[cmd]
	if !ok {
		return fmt.Errorf("gh %s is not one of the read-only commands (%s, api)", cmd, strings.Join(sortedKeys(ghRead), ", "))
	}
	if len(args) < 2 {
		return fmt.Errorf("gh %s needs a subcommand: %s", cmd, strings.Join(subs, ", "))
	}
	for _, s := range subs {
		if args[1] == s {
			return nil
		}
	}
	return fmt.Errorf("gh %s %s is not read-only (allowed: %s)", cmd, args[1], strings.Join(subs, ", "))
}

var mutationWord = regexp.MustCompile(`(?i)\bmutation\b`)

// ghAPIReadOnly allows gh api GET requests and graphql queries. Fields
// make gh POST unless the method is GET; for graphql that is a query,
// unless it is a mutation.
func ghAPIReadOnly(args []string) error {
	method, endpoint := "", ""
	var fields []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := a, "", false
		if strings.HasPrefix(a, "--") {
			name, val, hasVal = strings.Cut(a, "=")
		} else if len(a) > 2 && a[0] == '-' && strings.ContainsRune("XfFHqtp", rune(a[1])) {
			name, val, hasVal = a[:2], a[2:], true
		}
		next := func() string {
			if hasVal {
				return val
			}
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch name {
		case "-X", "--method":
			method = strings.ToUpper(next())
		case "-f", "--raw-field", "-F", "--field":
			v := next()
			if (name == "-F" || name == "--field") && strings.Contains(v, "=@") {
				return errors.New("fields from files are not allowed")
			}
			fields = append(fields, v)
		case "-H", "--header":
			if v := next(); strings.Contains(strings.ToLower(v), "override") {
				return errors.New("method override headers are not allowed")
			}
		case "--input":
			return errors.New("--input is not allowed")
		case "-q", "--jq", "-t", "--template", "--hostname", "-p", "--preview", "--cache":
			next()
		default:
			if !strings.HasPrefix(a, "-") && endpoint == "" {
				endpoint = a
			}
		}
	}
	if endpoint == "" {
		return errors.New("gh api needs an endpoint")
	}
	if method != "" && method != "GET" {
		if !(method == "POST" && endpoint == "graphql") {
			return fmt.Errorf("gh api -X %s is not allowed: only GET", method)
		}
	}
	if len(fields) > 0 && method != "GET" {
		if endpoint != "graphql" {
			return errors.New("gh api with fields POSTs: add -X GET to send them as the query string")
		}
		for _, f := range fields {
			if mutationWord.MatchString(f) {
				return errors.New("graphql mutations are not allowed")
			}
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
