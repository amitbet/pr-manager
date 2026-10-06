package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The server answers initialize, lists only the tools it was given, and
// reports a refused command as a tool error.
func TestMCPServer(t *testing.T) {
	s := &mcpServer{tools: map[string]bool{"gh": true}}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"gh","arguments":{"args":["pr","merge","1"]}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fetch","arguments":{"url":"https://example.com"}}}`,
	}, "\n")
	var out bytes.Buffer
	if err := s.serve(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("%d answers to 4 requests:\n%s", len(lines), out.String())
	}
	var list struct {
		Result struct{ Tools []struct{ Name string } }
	}
	_ = json.Unmarshal([]byte(lines[1]), &list)
	if len(list.Result.Tools) != 1 || list.Result.Tools[0].Name != "gh" {
		t.Errorf("tools %+v", list.Result.Tools)
	}
	for _, l := range lines[2:] {
		if !strings.Contains(l, `"isError":true`) {
			t.Errorf("not refused: %s", l)
		}
	}
}
