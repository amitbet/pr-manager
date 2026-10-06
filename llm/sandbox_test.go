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
