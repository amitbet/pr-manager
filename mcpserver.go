package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/internal/ghread"
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
			"description": ghread.Description,
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
		return ghread.Run(ctx, s.dir, in.Args)
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

func sortedKeys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
