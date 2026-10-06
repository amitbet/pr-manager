package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

// The chat agent gets a shell to dig with: on codex and claude-code rg,
// git log, show, blame and diff, ls, pipes, all read-only (see
// llm.Workspace.Shell); on the API providers the workspace tools, with git
// for the history (see llm/fstools.go). It is told where the app keeps things, so it can
// look past the material it was given: the repository's clone with its
// whole history, the fix checkout, the saved results and the logs of
// every analysis, the review drafts, the dismissals and the code map.

// chatPlace is a directory the agent may read, and what is in it.
type chatPlace struct{ dir, what string }

// chatPlaces are the places of result r that exist on disk.
func (t *triager) chatPlaces(r *PRResult, workDir string) []chatPlace {
	var out []chatPlace
	add := func(dir, what string) {
		if dir == "" {
			return
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return
		}
		out = append(out, chatPlace{dir, what})
	}
	add(workDir, "your working directory: the code as reviewed (see above)")
	if r.PR.LocalPath != "" {
		add(r.PR.LocalPath, "the local checkout that was triaged, with its git history")
	} else if t.fetcher != nil {
		add(t.fetcher.RepoDir(r.PR.PRRef), fmt.Sprintf("the app's clone of the repository, with its history: the PR head is %s, the merge base %s, the base branch origin/%s (a blobless clone: old file contents of files the PR doesn't touch may be missing)", r.PR.HeadOid, r.PR.BaseOid, r.PR.BaseRef))
	}
	if r.LocalFixDir != "" && r.LocalFixDir != workDir {
		add(r.LocalFixDir, "the fix checkout, on branch "+orDefault(r.LocalFixBranch, "(detached)"))
	}
	add(t.results, fmt.Sprintf("saved results, one JSON file per run: this one is %s.json; every earlier run and fix of the same change starts with the same name up to its last two __ parts", r.Key))
	add(t.logDir(r.Key), "the saved logs of the jobs that made this result (triage, fixes): one JSON file per job, every model call, its reasoning, the files it read and its answer")
	if t.opts.cache != "" {
		add(filepath.Join(t.opts.cache, "drafts"), "pending review comments, per PR")
		add(filepath.Join(t.opts.cache, "dismissed"), "dismissed issues, per repository, with the reasons")
	}
	if d := t.opts.codemapDir; d != "" && d != "off" {
		add(d, "the code map: the indexed repositories' symbols, callers, ranks and file history")
	}
	// Other repositories and dependency sources (chatrepos.go).
	for _, p := range t.repoPlaces(r) {
		if p.dir != workDir {
			out = append(out, p)
		}
	}
	return dedupePlaces(out)
}

// chatPlacesText tells the agent about the places and how to dig in them:
// with a shell (cli), or with the workspace tools.
func chatPlacesText(places []chatPlace, cli bool) string {
	var b strings.Builder
	if cli {
		b.WriteString("You can run read-only shell commands: rg, git (log, show, blame, diff, grep), ls, cat, head, pipes. Writes and the network are blocked. Not every tool may be installed (check with rg --version or git --version, and use grep -r or git grep without rg). Prefer a command over guessing: who changed a line and why (git log -L, git blame), what a file looked like before, where else a function is called, what an earlier run or a fix's log said. Keep output small: filter with rg, -n and head.\n\nWhere things are:\n")
	} else {
		b.WriteString("You have tools to read with: read_file, list_dir, glob, grep, and git for commands that read (log, show, blame, diff, grep). Paths are relative to your working directory, or absolute in the places below; nothing can be changed. Call several at once when they don't depend on each other. Prefer a tool over guessing: who changed a line and why (git log -L, git blame), what a file looked like before (git show <rev>:<path>), where else a function is called (grep), what an earlier run or a fix's log said. Keep output small: grep with a glob or a path, read files in pages, git with -n or a path. When you know enough, answer with the reply tool.\n\nWhere things are:\n")
	}
	for _, p := range places {
		fmt.Fprintf(&b, "- %s: %s\n", p.dir, p.what)
	}
	return b.String()
}

// shellWorkspace gives ws the shell and the places to read.
func shellWorkspace(ws *llm.Workspace, places []chatPlace, cache string) {
	ws.Shell = true
	for _, p := range places {
		if p.dir != ws.Dir {
			ws.ReadDirs = append(ws.ReadDirs, p.dir)
		}
	}
	if cache != "" {
		ws.NoWrite = append(ws.NoWrite, cache)
	}
}

var warmed sync.Map // repo dir + head -> struct{}

// warmHistory fetches, in a blobless clone, the old contents of the
// files the PR changes, so blame and old versions work in the sandbox,
// which has no network. Best effort, bounded, once per head.
func warmHistory(ctx context.Context, repo, head string, paths []string) {
	if repo == "" || head == "" || len(paths) == 0 {
		return
	}
	if _, done := warmed.LoadOrStore(repo+"\x00"+head, struct{}{}); done {
		return
	}
	if v, _ := triage.Git(repo, "config", "--get", "remote.origin.promisor"); strings.TrimSpace(v) != "true" {
		return // a full clone has everything
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if len(paths) > 200 {
		paths = paths[:200]
	}
	// git log -p reads every version it diffs, fetching what is missing.
	args := triage.GitArgs(append([]string{"log", "-p", "--format=", "-n", "40", head, "--"}, paths...)...)
	cmd := proc.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	_ = cmd.Run()
}

func resultPaths(r *PRResult) []string {
	var paths []string
	for _, f := range r.Files {
		paths = append(paths, f.Path)
	}
	return paths
}
