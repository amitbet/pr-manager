package triage

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// agentFiles are the files the reviewer CLIs load from their working
// directory as instructions or configuration: Claude Code's CLAUDE.md and
// CLAUDE.local.md (at any depth, nested ones as it reads there), .claude/
// (settings, rules, skills, agents, commands, hooks) and .mcp.json; Codex's
// AGENTS.md and AGENTS.override.md, .codex/ (config.toml, agents, hooks) and
// .agents/skills and .agents/plugins. Names match without regard to case,
// as a case-insensitive file system finds them.
var (
	agentFileNames = []string{"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md", "AGENTS.override.md", ".mcp.json"}
	agentDirNames  = []string{".claude", ".codex"}
	agentSubdirs   = map[string][]string{".agents": {"skills", "plugins"}}
)

// StripAgentFiles removes agentFiles from every directory of dir, a
// checkout of an untrusted revision, so a PR can't hand the reviewer or
// fixer instructions posing as the project's. The CLIs are also told not
// to load them; this is the second line.
//
// In a git checkout the removed files that are tracked get the
// skip-worktree bit first, so git status, `git add -A` and diffs against
// the checkout's index don't see them deleted. The directory must be one
// the caller created: the files are gone for good.
func StripAgentFiles(dir string) error {
	var found []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		name := d.Name()
		if strings.EqualFold(name, ".git") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if isAgentFile(path, name) {
			found = append(found, path)
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	if err != nil || len(found) == 0 {
		return err
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		if err := skipWorktree(dir, found); err != nil {
			return err
		}
	}
	var errs []error
	for _, p := range found {
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func isAgentFile(path, name string) bool {
	for _, n := range agentFileNames {
		if strings.EqualFold(name, n) {
			return true
		}
	}
	for _, n := range agentDirNames {
		if strings.EqualFold(name, n) {
			return true
		}
	}
	parent := filepath.Base(filepath.Dir(path))
	for p, subs := range agentSubdirs {
		if !strings.EqualFold(parent, p) {
			continue
		}
		for _, s := range subs {
			if strings.EqualFold(name, s) {
				return true
			}
		}
	}
	return false
}

// skipWorktree sets the skip-worktree bit on the tracked files at or under
// paths, which are in the checkout dir.
func skipWorktree(dir string, paths []string) error {
	args := []string{"ls-files", "-z", "--"}
	for _, p := range paths {
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		args = append(args, ":(literal)"+filepath.ToSlash(rel))
	}
	out, err := Git(dir, args...)
	if err != nil {
		return err
	}
	var tracked []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			tracked = append(tracked, f)
		}
	}
	if len(tracked) == 0 {
		return nil
	}
	_, err = Git(dir, append([]string{"update-index", "--skip-worktree", "--"}, tracked...)...)
	return err
}
