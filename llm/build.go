package llm

import (
	"encoding/json"
	"path/filepath"
	"sort"
)

// A reading agent may also build and test Go code, in its sandbox, with
// nothing written but its build output: Workspace.Builds is the directory
// it goes to (a Go build cache and temp directory). Claude Code may write
// there and nowhere else; codex's read-only sandbox writes nowhere, so it
// builds only when Workspace.Scratch says Dir is a throwaway copy, in its
// workspace-write sandbox (Dir, the temp directories and Builds; no
// network). Modules come from the module cache only: the caller downloads
// them first (the sandbox has no network).

// buildEnv is the environment the agent's commands get for Builds.
func buildEnv(ws *Workspace) map[string]string {
	if ws == nil || ws.Builds == "" || ws.Edit {
		return nil
	}
	return map[string]string{
		"GOCACHE":     filepath.Join(ws.Builds, "cache"),
		"GOTMPDIR":    filepath.Join(ws.Builds, "tmp"),
		"GOPROXY":     "off",
		"GOFLAGS":     "-mod=readonly",
		"GOTELEMETRY": "off",
	}
}

// codexBuilds reports whether codex builds in ws: only in a scratch copy.
func codexBuilds(ws *Workspace) bool {
	return ws != nil && !ws.Edit && ws.Builds != "" && ws.Scratch
}

// codexBuildArgs is codex's configuration for building in ws.
func codexBuildArgs(ws *Workspace) []string {
	if !codexBuilds(ws) {
		return nil
	}
	roots, _ := json.Marshal([]string{ws.Builds})
	args := []string{"--config", "sandbox_workspace_write.writable_roots=" + string(roots), "--config", "sandbox_workspace_write.network_access=false"}
	env := buildEnv(ws)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, _ := json.Marshal(env[k])
		args = append(args, "--config", "shell_environment_policy.set."+k+"="+string(v))
	}
	return args
}

// buildNote is what the prompt says about building.
func buildNote(ws *Workspace) string {
	if ws == nil || ws.Builds == "" || ws.Edit {
		return ""
	}
	return " You can also build and test Go code, in the sandbox: go build ./..., go vet ./..., go test ./some/pkg -run TestX (the build cache is set up for you; modules come from the local module cache only, so a missing one fails, and say so). Run only the packages you need; the PR's tests are its author's code, so read what a test does before you trust what it says."
}
