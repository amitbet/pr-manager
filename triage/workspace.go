package triage

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/llm"
)

// reviewWorkspace gives the reviewer the repository at the PR head: the
// saved head directory when there is one, else a detached git worktree of
// src.Snapshot or src.Head (removed by the returned cleanup). Go repos
// also get the module cache so library behavior can be checked instead of
// guessed. nil when neither is available. warn, if set, gets problems
// that leave the workspace usable.
func reviewWorkspace(src *Source, warn func(string)) (*llm.Workspace, func(), error) {
	noop := func() {}
	var ws *llm.Workspace
	cleanup := noop
	switch {
	case src.HeadDir != "":
		ws = &llm.Workspace{Dir: src.HeadDir}
	case src.Dir != "" && (src.Snapshot != "" || src.Head != ""):
		rev := src.Head
		if src.Snapshot != "" {
			rev = src.Snapshot
		}
		dir, err := os.MkdirTemp("", "pr-manager-head-")
		if err != nil {
			return nil, noop, err
		}
		// Resolve the temp dir (a symlink on macOS) so the path tools see
		// from getcwd() is the one we relativize against.
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			dir = r
		}
		if _, err := Git(src.Dir, "worktree", "add", "--detach", dir, rev); err != nil {
			os.RemoveAll(dir)
			return nil, noop, err
		}
		ws = &llm.Workspace{Dir: dir}
		// A snapshot is the user's working tree, whose linters and tools
		// load plugins from its ignored node_modules; the worktree gets
		// links to them. They are unlinked before the worktree goes, so
		// removal cannot reach into the user's checkout.
		var links []string
		if src.Snapshot != "" {
			links = linkIgnoredDeps(src.Dir, dir, warn)
		}
		cleanup = func() {
			for _, l := range links {
				_ = os.Remove(l)
			}
			_, _ = Git(src.Dir, "worktree", "remove", "--force", dir)
			os.RemoveAll(dir)
		}
		// The PR head is untrusted: its agent files must not reach the
		// reviewer as the project's instructions. A snapshot of the user's
		// own working tree, like a saved head directory, is left as it is.
		if src.Snapshot == "" {
			if err := StripAgentFiles(dir); err != nil {
				cleanup()
				return nil, noop, err
			}
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

// depDirs are the ignored dependency directories a checkout's tools load
// from, linked into a snapshot's worktree.
var depDirs = map[string]bool{"node_modules": true}

// linkIgnoredDeps links each ignored dependency directory of the checkout
// live (at the root and in workspace packages) to the same path in the
// worktree wt, and returns the links made. It writes nothing under live; a
// directory it cannot link is left out with a warning.
func linkIgnoredDeps(live, wt string, warn func(string)) []string {
	out, err := Git(live, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory", "--no-empty-directory")
	if err != nil {
		if warn != nil {
			warn("node_modules not linked into the review workspace: " + err.Error())
		}
		return nil
	}
	var links []string
	for _, rel := range strings.Split(out, "\x00") {
		rel = strings.TrimSuffix(rel, "/")
		if rel == "" || !depDirs[pathBase(rel)] {
			continue
		}
		target := filepath.Join(live, filepath.FromSlash(rel))
		if st, err := os.Stat(target); err != nil || !st.IsDir() {
			continue
		}
		link := filepath.Join(wt, filepath.FromSlash(rel))
		if _, err := os.Lstat(link); err == nil {
			continue // the snapshot has something there already
		}
		// A package the snapshot no longer has gets no link.
		if st, err := os.Stat(filepath.Dir(link)); err != nil || !st.IsDir() {
			continue
		}
		if err := linkDir(target, link); err != nil {
			if warn != nil {
				warn(rel + " not linked into the review workspace: " + err.Error())
			}
			continue
		}
		links = append(links, link)
	}
	return links
}

// pathBase is the last element of the slash-separated path p.
func pathBase(p string) string {
	return p[strings.LastIndex(p, "/")+1:]
}

// linkDir links link to the directory target: a symlink, else on Windows
// (where a symlink takes Developer Mode or elevation) a directory junction.
func linkDir(target, link string) error {
	serr := os.Symlink(target, link)
	if serr == nil || runtime.GOOS != "windows" {
		return serr
	}
	if out, err := proc.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		return fmt.Errorf("%v; junction: %v: %s", serr, err, strings.TrimSpace(string(out)))
	}
	return nil
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
