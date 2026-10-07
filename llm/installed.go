package llm

import (
	"path/filepath"
	"strings"
)

// An Installed workspace runs the CLI as the reader has it set up: their
// own tools, MCP servers, skills, hooks and permission rules, editing the
// working directory and running commands, instead of the read-only agent
// the app confines. It still gets the app's material, MCP server and
// session, and answers with the same JSON object, so its actions go
// through the UI as before.
//
// What it must not do is post or push on its own: that goes through the
// actions, which the reader approves. Claude Code runs every other command
// without asking (bypassPermissions), but deny rules still hold: the gh
// and git push commands (gh still reads through the app's MCP server),
// compound ones included, and edits of the directories it reads. Codex has no rule for that, so its
// sandbox gets no network: gh and git push fail, and its web search and
// the app's MCP server, which run outside the sandbox, still work. These
// are guards for an agent that means well, not a sandbox against one
// that doesn't.

// installedDenied are the commands an Installed Claude Code may not run.
var installedDenied = []string{"Bash(gh:*)", "Bash(git push:*)"}

// claudeInstalledArgs are the permission and settings arguments of an
// Installed workspace. allowed are the app's own tools, which the
// reader's rules don't know (claudeReach).
func claudeInstalledArgs(ws *Workspace, allowed string) []string {
	sources := "user"
	if ws.Project {
		sources = "user,project,local"
	}
	// A -p call can't ask, so everything not denied below runs, as the
	// reader's own agent would with them approving.
	args := []string{"--permission-mode", "bypassPermissions", "--setting-sources", sources}
	if allowed != "" {
		args = append(args, "--allowedTools", allowed)
	}
	deny := append([]string(nil), installedDenied...)
	if !ws.Web {
		deny = append(deny, "WebFetch", "WebSearch")
	}
	// The other places are there to read. One that holds the working directory,
	// such as the cache the conversation's worktree is in, stays open.
	for _, d := range append(append([]string(nil), ws.ReadDirs...), ws.NoWrite...) {
		if !within(ws.Dir, d) {
			deny = append(deny, "Edit("+absRule(d)+"/**)")
		}
	}
	args = append(args, "--disallowedTools", strings.Join(deny, ","))
	for _, d := range ws.ReadDirs {
		args = append(args, "--add-dir", d)
	}
	return args
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// absRule is an absolute path as a permission rule writes it: //path.
func absRule(dir string) string {
	return "/" + strings.TrimPrefix(filepath.ToSlash(dir), "/")
}

// codexInstalledArgs are the configuration of an Installed workspace:
// the reader's own config and rules load, and its sandbox writes the
// working directory, without network.
func codexInstalledArgs(ws *Workspace) []string {
	args := []string{"--config", "sandbox_workspace_write.network_access=false"}
	if !ws.Project {
		// No AGENTS.md from an untrusted checkout.
		args = append(args, "--config", "project_doc_max_bytes=0")
	}
	return args
}

// installedNote is what an Installed agent is told about its workspace.
func installedNote(ws *Workspace) string {
	return "Use your tools as you need to: read, search, edit files in your working directory, build and run tests. The other directories you are given are there to read; don't change them. Don't push and don't post to GitHub yourself (gh and git push won't run): propose the actions for that, which the reader approves. Your shell may have no network." + strings.Replace(reachNote(ws), " Outside the sandbox you can", " You can also", 1)
}
