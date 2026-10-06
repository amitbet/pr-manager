package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// A Shell workspace runs Claude Code in its sandbox, with every directory
// it reads denied to writes, no network and no unsandboxed commands; the
// fallback list never allows Bash as a whole.
func TestClaudeShellSettings(t *testing.T) {
	b, err := claudeSettings(&Workspace{Dir: "/repo", ReadDirs: []string{"/cache/results"}, NoWrite: []string{"/cache"}, Shell: true})
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Sandbox struct {
			Enabled, AutoAllowBashIfSandboxed, AllowUnsandboxedCommands bool
			Filesystem                                                  struct{ DenyWrite []string }
			Network                                                     struct{ AllowedDomains []string }
		}
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	sb := s.Sandbox
	if !sb.Enabled || !sb.AutoAllowBashIfSandboxed || sb.AllowUnsandboxedCommands || len(sb.Network.AllowedDomains) != 0 {
		t.Errorf("sandbox %+v", sb)
	}
	if got := strings.Join(sb.Filesystem.DenyWrite, " "); got != "/repo /cache/results /cache" {
		t.Errorf("denyWrite %q", got)
	}
	for _, rule := range strings.Split(shellAllowed(), ",") {
		if rule == "Bash" || !strings.HasPrefix(rule, "Bash(") {
			t.Errorf("rule %q", rule)
		}
	}
	for _, ws := range []*Workspace{nil, {Dir: "/repo"}, {Dir: "/repo", Edit: true, Shell: true}} {
		if b, _ := claudeSettings(ws); string(b) != `{"disableAllHooks":true}` {
			t.Errorf("%+v: %s", ws, b)
		}
	}
}

// With Builds, Claude Code may write there and nowhere else, with Go's
// caches pointed at it and no module downloads; codex builds only in a
// scratch copy, in workspace-write.
func TestBuildSandbox(t *testing.T) {
	ws := &Workspace{Dir: "/repo", Shell: true, Builds: "/tmp/b"}
	b, err := claudeSettings(ws)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Sandbox struct {
			Filesystem struct{ AllowWrite, DenyWrite []string }
		}
		Env map[string]string
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.Sandbox.Filesystem.AllowWrite, " "); got != "/tmp/b" {
		t.Errorf("allowWrite %q", got)
	}
	if s.Env["GOCACHE"] != "/tmp/b/cache" || s.Env["GOPROXY"] != "off" || s.Env["GOFLAGS"] != "-mod=readonly" {
		t.Errorf("env %v", s.Env)
	}
	if codexSandbox(ws) != "read-only" || codexBuildArgs(ws) != nil {
		t.Error("codex builds in a directory that isn't a scratch copy")
	}
	ws.Scratch = true
	args := strings.Join(codexBuildArgs(ws), " ")
	if codexSandbox(ws) != "workspace-write" || !strings.Contains(args, `writable_roots=["/tmp/b"]`) || !strings.Contains(args, "network_access=false") || !strings.Contains(args, `shell_environment_policy.set.GOCACHE="/tmp/b/cache"`) {
		t.Errorf("codex: %s %s", codexSandbox(ws), args)
	}
	if buildNote(ws) == "" || buildNote(&Workspace{Dir: "/repo"}) != "" {
		t.Error("build note")
	}
}
