package llm

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A conversation with a CLI agent can carry on in the CLI's own session,
// so a follow-up question doesn't start from nothing: the agent keeps the
// files it read and the commands it ran. The caller keeps Session between
// calls; the first call starts a session and fills in its ID, the next
// ones resume it with only the new turns. Claude Code finds a session by
// its working directory, so Workspace.Dir must stay the same.
//
// The CLIs can also reach past their sandbox, without being able to
// write: Web turns on their own web search (and, on Claude Code, fetch),
// and MCP servers (the app's own, see mcpserver.go in package main) run
// outside the sandbox with tools that only read, such as gh.

// Session is a CLI conversation. ID is the CLI's session id ("" to start
// one); System is a hash of the system prompt the session last got, so
// codex, which keeps it in its transcript, is only sent a changed one.
type Session struct {
	ID     string `json:"id"`
	System string `json:"system,omitempty"`
}

// MCPServer is a stdio MCP server the agent may call Tools of.
type MCPServer struct {
	Name    string
	Command string
	Args    []string
	Tools   []string
}

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// reaches reports whether ws lets a reading agent past its sandbox.
func (ws *Workspace) reaches() bool {
	return ws != nil && !ws.Edit && (ws.Web || len(ws.MCP) > 0)
}

// claudeReach is what Claude Code adds for ws's web and MCP servers: the
// tools to list, the ones to allow and the arguments. The MCP config is
// written to dir.
func claudeReach(ws *Workspace, dir string) (tools, allowed string, args []string, err error) {
	if !ws.reaches() {
		return "", "", nil, nil
	}
	var list []string
	if ws.Web {
		list = append(list, "WebFetch", "WebSearch")
	}
	if len(ws.MCP) > 0 {
		servers := map[string]any{}
		var names []string
		for _, s := range ws.MCP {
			servers[s.Name] = map[string]any{"type": "stdio", "command": s.Command, "args": s.Args}
			for _, t := range s.Tools {
				names = append(names, "mcp__"+s.Name+"__"+t)
			}
		}
		b, err := json.Marshal(map[string]any{"mcpServers": servers})
		if err != nil {
			return "", "", nil, err
		}
		p := filepath.Join(dir, "mcp.json")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			return "", "", nil, err
		}
		args = append(args, "--mcp-config", p)
		// MCP tools aren't built in: --tools doesn't list them, and
		// --allowedTools lets them run.
		return strings.Join(list, ","), strings.Join(append(append([]string{}, list...), names...), ","), args, nil
	}
	return strings.Join(list, ","), strings.Join(list, ","), args, nil
}

// codexReach is the configuration codex gets for ws's web and MCP servers.
func codexReach(ws *Workspace) []string {
	if !ws.reaches() {
		return nil
	}
	var args []string
	if ws.Web {
		args = append(args, "--config", `web_search="live"`)
	}
	for _, s := range ws.MCP {
		cmd, _ := json.Marshal(s.Command)
		list, _ := json.Marshal(s.Args) // a JSON array of strings is a TOML one
		tools, _ := json.Marshal(s.Tools)
		pre := "mcp_servers." + s.Name + "."
		args = append(args, "--config", pre+"command="+string(cmd), "--config", pre+"args="+string(list),
			"--config", pre+"enabled_tools="+string(tools), "--config", pre+"default_tools_approval_mode=\"approve\"")
	}
	return args
}

// reachNote is what the prompt says about them.
func reachNote(ws *Workspace) string {
	if !ws.reaches() {
		return ""
	}
	var parts []string
	if ws.Web {
		parts = append(parts, "search and fetch the web")
	}
	for _, s := range ws.MCP {
		parts = append(parts, fmt.Sprintf("call the %s tool%s of the %s server", strings.Join(s.Tools, ", "), plural(len(s.Tools)), s.Name))
	}
	return " Outside the sandbox you can " + strings.Join(parts, " and ") + "."
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// mcpCall is a one-line summary of a call to an MCP server's tool: gh as
// its command line, fetch as its URL.
func mcpCall(server, tool string, input json.RawMessage) string {
	var in struct {
		Args []string `json:"args"`
		URL  string   `json:"url"`
	}
	_ = json.Unmarshal(input, &in)
	switch {
	case tool == "gh" && len(in.Args) > 0:
		return "gh " + strings.Join(in.Args, " ")
	case tool == "fetch" && in.URL != "":
		return "Fetch " + in.URL
	}
	return server + "." + tool + " " + truncate(string(input), 200)
}
