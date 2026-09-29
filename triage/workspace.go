package triage

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/llm"
)

// reviewWorkspace gives the reviewer the repository at the PR head: the
// saved head directory when there is one, else a detached git worktree of
// src.Head (removed by the returned cleanup). Go repos also get the module
// cache so library behavior can be checked instead of guessed. nil when
// neither is available.
func reviewWorkspace(src *Source) (*llm.Workspace, func(), error) {
	noop := func() {}
	var ws *llm.Workspace
	cleanup := noop
	switch {
	case src.HeadDir != "":
		ws = &llm.Workspace{Dir: src.HeadDir}
	case src.Dir != "" && src.Head != "":
		dir, err := os.MkdirTemp("", "pr-manager-head-")
		if err != nil {
			return nil, noop, err
		}
		// Resolve the temp dir (a symlink on macOS) so the path tools see
		// from getcwd() is the one we relativize against.
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			dir = r
		}
		if _, err := Git(src.Dir, "worktree", "add", "--detach", dir, src.Head); err != nil {
			os.RemoveAll(dir)
			return nil, noop, err
		}
		ws = &llm.Workspace{Dir: dir}
		cleanup = func() {
			_, _ = Git(src.Dir, "worktree", "remove", "--force", dir)
			os.RemoveAll(dir)
		}
		// The PR head is untrusted: its agent files must not reach the
		// reviewer as the project's instructions. A saved head directory
		// (the user's own working tree) is left as it is.
		if err := StripAgentFiles(dir); err != nil {
			cleanup()
			return nil, noop, err
		}
	default:
		return nil, noop, nil
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "go.mod")); err == nil {
		if mc := goModCache(); mc != "" {
			ws.ReadDirs = append(ws.ReadDirs, mc)
		}
	}
	return ws, cleanup, nil
}

var (
	modCacheOnce sync.Once
	modCache     string
)

// goModCache is `go env GOMODCACHE`, or "" without Go or the directory.
func goModCache() string {
	modCacheOnce.Do(func() {
		out, err := proc.Command("go", "env", "GOMODCACHE").Output()
		if err != nil {
			return
		}
		dir := strings.TrimSpace(string(out))
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			modCache = dir
		}
	})
	return modCache
}
