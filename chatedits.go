package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/triage"
)

// The Installed chat agent (Settings → Chat agent → Agent) is the reader's
// own codex or claude-code, which edits and runs commands. On a PR or a
// fix its working directory is the conversation's worktree, and its edits
// stay there between answers, on the commit the worktree is of, which
// <hash>/code.base names. commit_edits makes them a change: a fix job
// that applies them to the fix checkout and checks, re-triages and
// commits them like a change the agent was asked for. On a local review
// it edits the reader's own checkout, so its edits are theirs already.

const (
	editsBase     = "code.base"
	editsFilesMax = 200 // files commit_edits may change
)

// installedWorktree makes <dir>/code a worktree of repo at head, keeping
// the agent's edits: as it is while head is the same; when head moved,
// checked out again with the edits carried over, unless head has them
// already (they were committed as a change). Edits that don't apply to
// the new head are saved to a patch file, which note names.
func installedWorktree(ctx context.Context, repo, dir, head string) (note string, err error) {
	code := filepath.Join(dir, "code")
	base := readBase(dir)
	_, statErr := os.Stat(code)
	if statErr == nil && base == head {
		return "", nil
	}
	var patch string
	if statErr == nil && base != "" {
		patch, _ = editsPatch(ctx, code, base)
	}
	if err := convWorktree(ctx, repo, code, head); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, editsBase), []byte(head+"\n"), 0o600); err != nil {
		return "", err
	}
	if strings.TrimSpace(patch) == "" || gitApply(ctx, code, patch, "--check", "-R") == nil {
		return "", nil
	}
	if gitApply(ctx, code, patch) == nil {
		return " Your edits since " + short(base) + " are carried over to it.", nil
	}
	saved, err := saveEdits(dir, patch)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(" Your edits since %s don't apply to it; they are saved in %s.", short(base), saved), nil
}

// keepEdits, before the read-only agent checks the worktree out again,
// saves the edits an Installed agent left in it, and forgets their base.
func keepEdits(ctx context.Context, dir string) string {
	base := readBase(dir)
	if base == "" {
		return ""
	}
	defer os.Remove(filepath.Join(dir, editsBase))
	patch, err := editsPatch(ctx, filepath.Join(dir, "code"), base)
	if err != nil || strings.TrimSpace(patch) == "" {
		return ""
	}
	saved, err := saveEdits(dir, patch)
	if err != nil {
		return ""
	}
	return " Edits made here earlier are saved in " + saved + "."
}

func readBase(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, editsBase))
	return strings.TrimSpace(string(b))
}

func saveEdits(dir, patch string) (string, error) {
	p := filepath.Join(dir, fmt.Sprintf("edits-%s.patch", time.Now().Format("20060102-150405")))
	return p, os.WriteFile(p, []byte(patch), 0o600)
}

// editsPatch is what changed in code since base, commits and untracked
// files included, as a patch. It stages into a copy of the index, so the
// agent's own staging stays as it left it, and the files StripAgentFiles
// hid stay out.
func editsPatch(ctx context.Context, code, base string) (string, error) {
	index, err := triage.GitCtx(ctx, code, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(strings.TrimSpace(index))
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp("", "pr-manager-edits-index-")
	if err != nil {
		return "", err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if err := os.WriteFile(tmp.Name(), b, 0o600); err != nil {
		return "", err
	}
	git := func(args ...string) (string, error) {
		cmd := proc.CommandContext(ctx, "git", triage.GitArgs(args...)...)
		cmd.Dir = code
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+tmp.Name())
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %w", args[0], err)
		}
		return string(out), nil
	}
	if _, err := git("add", "-A"); err != nil {
		return "", err
	}
	return git("diff", "--cached", "--binary", "--no-color", "--no-ext-diff", "--no-renames", base)
}

func gitApply(ctx context.Context, dir, patch string, flags ...string) error {
	cmd := proc.CommandContext(ctx, "git", triage.GitArgs(append(append([]string{"apply"}, flags...), "-")...)...)
	cmd.Dir, cmd.Stdin = dir, strings.NewReader(patch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// chatEdits is the Installed agent's edits in the conversation about
// change, for commit_edits: the patch and the files it changes.
func (t *triager) chatEdits(ctx context.Context, change string) (string, []string, error) {
	if t.opts.cache == "" {
		return "", nil, errors.New("no cache to keep the chat agent's edits in")
	}
	h := convHash(change)
	mu, _ := chatLocks.LoadOrStore(h, &sync.Mutex{})
	if !mu.(*sync.Mutex).TryLock() {
		return "", nil, errors.New("the chat agent is still answering")
	}
	defer mu.(*sync.Mutex).Unlock()
	dir := filepath.Join(t.chatsDir(), h)
	base := readBase(dir)
	if base == "" {
		return "", nil, errors.New("the chat agent has no edits in a worktree of its own (on a local review it edits your checkout)")
	}
	patch, err := editsPatch(ctx, filepath.Join(dir, "code"), base)
	if err != nil {
		return "", nil, fmt.Errorf("read the chat agent's edits: %w", err)
	}
	if strings.TrimSpace(patch) == "" {
		return "", nil, errors.New("the chat agent has made no edits")
	}
	diffs, err := triage.ParseDiff(patch)
	if err != nil {
		return "", nil, err
	}
	seen := map[string]bool{}
	var files []string
	for _, f := range diffs {
		for _, p := range []string{f.OldPath, f.Path} {
			if p != "" && !seen[p] {
				seen[p] = true
				files = append(files, p)
			}
		}
	}
	return patch, files, nil
}
