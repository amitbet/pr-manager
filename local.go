package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/codemap"
	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/triage"
)

// localRepoRef names the checkout's repo from its origin remote. Owner is
// the whole namespace (GitLab group/subgroup, Azure DevOps org/project)
// and Repo the last segment; a checkout without a readable remote is
// local/<directory>.
func localRepoRef(dir string) triage.PRRef {
	remote, err := triage.Git(dir, "remote", "get-url", "origin")
	if err == nil {
		s := strings.TrimSpace(remote)
		if host, owner, repo, ok := codemap.ParseRemote(s); ok {
			return triage.PRRef{Host: triage.NormalizeHost(host), Owner: owner, Repo: repo}
		}
		if host, owner, repo, err := triage.ParseRepo(s); err == nil {
			return triage.PRRef{Host: host, Owner: owner, Repo: repo}
		}
	}
	return triage.PRRef{Owner: "local", Repo: filepath.Base(dir)}
}

// baseOverride, when set, is the ref local checkouts are diffed from
// instead of origin's default branch: the fix command's -base, for a CI
// job whose PR targets another branch. The server never sets it.
var baseOverride string

func localBase(dir string) (string, string, error) {
	if baseOverride != "" {
		if _, err := triage.Git(dir, "rev-parse", "--verify", "--quiet", baseOverride+"^{commit}"); err != nil {
			return "", "", fmt.Errorf("base %s is not in the checkout; fetch it first (in CI, check out with fetch-depth: 0)", baseOverride)
		}
		return baseOverride, strings.TrimPrefix(baseOverride, "origin/"), nil
	}
	if s, err := triage.Git(dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimSpace(s), strings.TrimPrefix(strings.TrimSpace(s), "origin/"), nil
	}
	for _, name := range []string{"main", "master"} {
		if _, err := triage.Git(dir, "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+name); err == nil {
			return "origin/" + name, name, nil
		}
	}
	return "", "", errors.New("cannot find origin's default branch; run git remote set-head origin -a or fetch origin/main")
}

func gitWithIndex(ctx context.Context, dir, index string, args ...string) (string, error) {
	cmd := proc.CommandContext(ctx, "git", triage.GitArgs(args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
	out, err := cmd.Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(e.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

type localSnapshot struct {
	info *triage.PRInfo
	src  *triage.Source
	raw  string
}

// splitRev splits "path#rev" into the checkout and the commit or branch
// to review in it. A path that exists as typed has no rev, so a directory
// with # in its name still opens.
func splitRev(path string) (string, string) {
	i := strings.LastIndex(path, "#")
	if i < 0 {
		return path, ""
	}
	if _, err := os.Stat(path); err == nil {
		return path, ""
	}
	return path[:i], strings.TrimSpace(path[i+1:])
}

// inspectLocal reads a checkout's change for triage. A bare path is the
// checked-out branch from its merge base, working tree included;
// path#rev is a commit on its own (from its first parent) or a branch
// from its merge base, as committed, and path#base...branch is the
// branch from its merge base with base.
func inspectLocal(ctx context.Context, path string) (*localSnapshot, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("enter a repository path")
	}
	path, rev := splitRev(path)
	dir, err := checkoutRoot(path)
	if err != nil {
		return nil, err
	}
	if rev != "" {
		return inspectRev(ctx, dir, rev)
	}
	head, err := triage.Git(dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	head = strings.TrimSpace(head)
	branch, err := triage.Git(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return nil, errors.New("checkout is detached; switch to a branch before triaging")
	}
	branch = strings.TrimSpace(branch)
	return inspectBranch(ctx, dir, head, branch)
}

// checkoutRoot resolves path, ~ included, to the root of the Git checkout
// it names, and fails when it names a directory inside one.
func checkoutRoot(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~"+string(os.PathSeparator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, path[2:])
		}
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	root, err := triage.Git(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not a Git checkout: %w", path, err)
	}
	dir, err := filepath.EvalSymlinks(filepath.FromSlash(strings.TrimSpace(root)))
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(dir, path) {
		return "", fmt.Errorf("choose the repository root: %s", dir)
	}
	return dir, nil
}

func inspectBranch(ctx context.Context, dir, head, branch string) (*localSnapshot, error) {
	baseRef, baseBranch, err := localBase(dir)
	if err != nil {
		return nil, err
	}
	defaultBranch, stackedOn := baseBranch, ""
	if s := stackBase(dir, branch, baseBranch); s != "" {
		baseRef, baseBranch, stackedOn = "origin/"+s, s, s
	}
	base, err := triage.Git(dir, "merge-base", baseRef, "HEAD")
	if err != nil {
		return nil, err
	}
	base = strings.TrimSpace(base)
	counts, err := triage.Git(dir, "rev-list", "--left-right", "--count", baseRef+"...HEAD")
	if err != nil {
		return nil, err
	}
	parts := strings.Fields(counts)
	behind, ahead := 0, 0
	if len(parts) == 2 {
		behind, _ = strconv.Atoi(parts[0])
		ahead, _ = strconv.Atoi(parts[1])
	}
	status, err := triage.Git(dir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	// The working tree is frozen into a commit once, and everything after
	// reads that: a save or a fix while the review runs must not show the
	// prompt, the linters or the reviewer's tools other code than the diff.
	snapshot, err := snapshotWorkingTree(ctx, dir, head)
	if err != nil {
		return nil, err
	}
	diffArgs := []string{"diff", "--no-color", "--no-ext-diff", "--src-prefix=a/", "--dst-prefix=b/", "-M", "-U5", base, snapshot}
	raw, err := triage.GitCtx(ctx, dir, diffArgs...)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("no changes from " + baseRef)
	}
	files, err := triage.ParseDiff(raw)
	if err != nil {
		return nil, err
	}
	ws, err := triage.GitCtx(ctx, dir, "diff", "--no-color", "--no-ext-diff", "--src-prefix=a/", "--dst-prefix=b/", "-M", "-U5", "-w", "--ignore-blank-lines", base, snapshot)
	if err != nil {
		return nil, err
	}
	wsFiles, err := triage.ParseDiff(ws)
	if err != nil {
		return nil, err
	}
	real := map[string]bool{}
	for _, f := range wsFiles {
		if len(f.Hunks) > 0 || f.Binary {
			real[f.Path] = true
		}
	}
	src := &triage.Source{Files: files, RealChanges: real, Dir: dir, Base: base, Head: head, Snapshot: snapshot}
	src.Content = func(p string) ([]byte, error) { s, e := triage.Git(dir, "show", snapshot+":"+p); return []byte(s), e }
	src.BaseContent = func(p string) ([]byte, error) { s, e := triage.Git(dir, "show", base+":"+p); return []byte(s), e }
	ref := localRepoRef(dir)
	title, _ := triage.Git(dir, "log", "-1", "--format=%s", "HEAD")
	if ahead == 0 {
		title = branch + " working changes"
	}
	author, _ := triage.Git(dir, "log", "-1", "--format=%an", "HEAD")
	info := &triage.PRInfo{PRRef: ref, LocalPath: dir, Title: strings.TrimSpace(title), Author: strings.TrimSpace(author), State: "LOCAL", BaseRef: baseBranch, HeadRef: branch, BaseOid: base, HeadOid: head, Ahead: ahead, Behind: behind, Uncommitted: strings.TrimSpace(status) != ""}
	if stackedOn != "" {
		info.StackedOn, info.DefaultBase = stackedOn, defaultBranch
	}
	info.Commits = triage.CommitMessages(ctx, dir, base, head)
	return finishSnapshot(info, src, raw), nil
}

// snapshotWorkingTree writes the checkout's working tree, untracked files
// included and ignored ones not, as git add -A sees it, into a commit on
// top of head, and returns it; head itself when nothing is uncommitted.
// It stages in a copy of the checkout's index, which keeps its stat cache
// (so a large checkout isn't rehashed) and its skip-worktree bits; the
// checkout's own index, HEAD and refs are left alone. The commit is
// reachable from nothing and git gc drops it in time.
func snapshotWorkingTree(ctx context.Context, dir, head string) (string, error) {
	index, cleanup, err := scratchIndex(ctx, dir, true)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if _, err := gitWithIndex(ctx, dir, index, "add", "-A"); err != nil {
		return "", err
	}
	tree, err := gitWithIndex(ctx, dir, index, "write-tree")
	if err != nil {
		return "", err
	}
	tree = strings.TrimSpace(tree)
	if headTree, err := triage.GitCtx(ctx, dir, "rev-parse", head+"^{tree}"); err == nil && strings.TrimSpace(headTree) == tree {
		return head, nil
	}
	// A fixed identity and date: commit-tree needs one even where the
	// user has none set, and the same tree then makes the same commit.
	cmd := proc.CommandContext(ctx, "git", "commit-tree", tree, "-p", head, "-m", "pr-manager working tree snapshot")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=pr-manager", "GIT_AUTHOR_EMAIL=pr-manager@localhost", "GIT_AUTHOR_DATE=@0 +0000",
		"GIT_COMMITTER_NAME=pr-manager", "GIT_COMMITTER_EMAIL=pr-manager@localhost", "GIT_COMMITTER_DATE=@0 +0000")
	out, err := cmd.Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git commit-tree: %w: %s", err, strings.TrimSpace(string(e.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// emptyTree writes git's empty tree in dir and returns it, the base of a
// root commit. A repository doesn't always store it.
func emptyTree(ctx context.Context, dir string) (string, error) {
	cmd := proc.CommandContext(ctx, "git", "mktree") // stdin is empty
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git mktree: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// missingParent returns the first parent recorded in commit's object, for
// a commit whose parent git can't resolve: a shallow clone's boundary keeps
// its parent lines even though .git/shallow hides them from rev-parse. It
// returns "" for a true root commit.
func missingParent(dir, commit string) string {
	raw, err := triage.Git(dir, "cat-file", "commit", commit)
	if err != nil {
		return ""
	}
	header, _, _ := strings.Cut(raw, "\n\n")
	for _, line := range strings.Split(header, "\n") {
		if p, ok := strings.CutPrefix(line, "parent "); ok && len(p) >= 10 {
			return p
		}
	}
	return ""
}

// branchRef finds rev's branch in dir: the branch's name as the picker
// shows it (feature, origin/feature) and its full ref, or empty when rev
// is no branch. A branch is read from its own ref: git resolves a bare
// name to a tag before a branch, which would review the tag under the
// branch's name.
func branchRef(dir, rev string) (branch, ref string) {
	name := strings.TrimPrefix(strings.TrimPrefix(rev, "refs/heads/"), "refs/remotes/")
	for _, r := range []string{"refs/heads/" + name, "refs/remotes/origin/" + name, "refs/remotes/" + name} {
		if _, err := triage.Git(dir, "show-ref", "--verify", "--quiet", r); err == nil {
			return strings.TrimPrefix(strings.TrimPrefix(r, "refs/heads/"), "refs/remotes/"), r
		}
	}
	return "", ""
}

// revRange splits base...head; ok is false for a single rev.
func revRange(rev string) (base, head string, ok bool) {
	return strings.Cut(rev, "...")
}

// inspectRev reads rev in the checkout at dir, as committed: a local or
// origin branch from its merge base with origin's default branch, or any
// other commit from its first parent. base...branch is the branch from
// its merge base with base, any branch or commit: origin/feature for the
// commits not pushed yet. The working tree is not read.
func inspectRev(ctx context.Context, dir, rev string) (*localSnapshot, error) {
	headRev := rev
	baseRev, h, ranged := revRange(rev)
	if ranged {
		headRev = h
	}
	if strings.HasPrefix(headRev, "-") || strings.HasPrefix(baseRev, "-") || (ranged && (baseRev == "" || headRev == "")) {
		return nil, fmt.Errorf("bad revision %q", rev)
	}
	branch, ref := branchRef(dir, headRev)
	if ref == "" {
		if ranged {
			return nil, fmt.Errorf("%s is not a branch in %s; compare a branch with %s", headRev, dir, baseRev)
		}
		ref = headRev
	}
	head, err := triage.Git(dir, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("%s is not a commit or branch in %s", headRev, dir)
	}
	head = strings.TrimSpace(head)
	info := &triage.PRInfo{PRRef: localRepoRef(dir), LocalPath: dir, State: "LOCAL", Rev: rev, HeadOid: head}
	var base, empty string
	if branch != "" {
		var baseRef, baseBranch string
		if ranged {
			b, r := branchRef(dir, baseRev)
			if r == "" {
				r = baseRev
			}
			oid, err := triage.Git(dir, "rev-parse", "--verify", "--quiet", "--end-of-options", r+"^{commit}")
			if err != nil {
				return nil, fmt.Errorf("%s is not a commit or branch in %s", baseRev, dir)
			}
			// BaseRef is what a PR would go into: the branch's name on
			// origin, or the commit.
			baseRef, baseBranch = r, strings.TrimPrefix(b, "origin/")
			if b == "" {
				baseBranch = strings.TrimSpace(oid)[:10]
			}
		} else if baseRef, baseBranch, err = localBase(dir); err != nil {
			return nil, err
		}
		if base, err = triage.Git(dir, "merge-base", baseRef, head); err != nil {
			return nil, err
		}
		base = strings.TrimSpace(base)
		if counts, err := triage.Git(dir, "rev-list", "--left-right", "--count", baseRef+"..."+head); err == nil {
			if parts := strings.Fields(counts); len(parts) == 2 {
				info.Behind, _ = strconv.Atoi(parts[0])
				info.Ahead, _ = strconv.Atoi(parts[1])
			}
		}
		info.BaseRef, info.HeadRef = baseBranch, branch
	} else {
		info.SingleCommit = true
		if p, err := triage.Git(dir, "rev-parse", "--verify", "--quiet", head+"^"); err == nil {
			base = strings.TrimSpace(p)
			info.BaseRef = base[:10]
		} else {
			// A shallow clone's boundary commits have parents git doesn't
			// have; diffing those against the empty tree would review the
			// whole repository as new code.
			if parent := missingParent(dir, head); parent != "" {
				return nil, fmt.Errorf("the parent of commit %s (%s) isn't in this shallow clone; run git fetch --deepen=1 in %s", head[:10], parent[:10], dir)
			}
			if empty, err = emptyTree(ctx, dir); err != nil {
				return nil, err
			}
			base, info.BaseRef = empty, "empty tree"
		}
		info.HeadRef, info.Ahead = head[:10], 1
	}
	info.BaseOid = base
	raw, err := triage.Git(dir, "diff", "--no-color", "--no-ext-diff", "--src-prefix=a/", "--dst-prefix=b/", "-M", "-U5", base, head)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%s has no changes from %s", rev, info.BaseRef)
	}
	files, err := triage.ParseDiff(raw)
	if err != nil {
		return nil, err
	}
	ws, err := triage.Git(dir, "diff", "--no-color", "--no-ext-diff", "--src-prefix=a/", "--dst-prefix=b/", "-M", "-U5", "-w", "--ignore-blank-lines", base, head)
	if err != nil {
		return nil, err
	}
	wsFiles, err := triage.ParseDiff(ws)
	if err != nil {
		return nil, err
	}
	real := map[string]bool{}
	for _, f := range wsFiles {
		if len(f.Hunks) > 0 || f.Binary {
			real[f.Path] = true
		}
	}
	src := &triage.Source{Files: files, RealChanges: real, Dir: dir, Base: base, Head: head}
	src.Content = func(p string) ([]byte, error) { s, e := triage.Git(dir, "show", head+":"+p); return []byte(s), e }
	src.BaseContent = func(p string) ([]byte, error) { s, e := triage.Git(dir, "show", base+":"+p); return []byte(s), e }
	title, _ := triage.Git(dir, "log", "-1", "--format=%s", head)
	author, _ := triage.Git(dir, "log", "-1", "--format=%an", head)
	info.Title, info.Author = strings.TrimSpace(title), strings.TrimSpace(author)
	if info.SingleCommit {
		body, _ := triage.Git(dir, "log", "-1", "--format=%b", head)
		info.Body = strings.TrimSpace(body)
	}
	if empty == "" {
		info.Commits = triage.CommitMessages(ctx, dir, base, head)
	}
	return finishSnapshot(info, src, raw), nil
}

// finishSnapshot counts the diff's lines and hashes it into info.
func finishSnapshot(info *triage.PRInfo, src *triage.Source, raw string) *localSnapshot {
	files := src.Files
	digest := sha256.Sum256([]byte(raw))
	info.SnapshotHash = hex.EncodeToString(digest[:])
	for _, f := range files {
		for _, h := range f.Hunks {
			for _, line := range h.Lines {
				if strings.HasPrefix(line, "+") {
					info.Adds++
				}
				if strings.HasPrefix(line, "-") {
					info.Dels++
				}
			}
		}
	}
	src.Title = info.Title
	return &localSnapshot{info: info, src: src, raw: raw}
}

// localCacheKey names a run of a local checkout: which checkout, what the
// change was, and how it was triaged. The three are kept apart so runs of
// the same checkout under the same settings can be found for each other;
// the settings part used to be hashed from a string holding the head
// commit, which made every commit look like different settings.
func localCacheKey(s *localSnapshot, o options) string {
	pathHash := sha256.Sum256([]byte(localSource(s.info)))
	diffHash := sha256.Sum256([]byte(s.info.BaseOid + "|" + s.info.HeadOid + "|" + s.raw))
	return fmt.Sprintf("local__%x__%x__%s", pathHash[:5], diffHash[:6], settingsHash(o))
}

func readLocalFile(dir, path string) ([]byte, error) {
	if !safeRepoPath(path) {
		return nil, errors.New("unsafe path")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(dir, filepath.FromSlash(path)))
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(dir, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, errors.New("file is outside the repository")
	}
	return os.ReadFile(resolved)
}

func (t *triager) RunLocal(ctx context.Context, path string, jo jobOptions, progress func(string, int, int)) (*PRResult, error) {
	o := t.options(jo)
	progress("inspect", 0, 0)
	s, err := inspectLocal(ctx, path)
	if err != nil {
		return nil, err
	}
	if remoteRevDir(s.info.LocalPath) {
		// A rev fetched from its forge has no files on disk to map: the
		// repository is mapped as for a PR.
		err = ensureCodeMap(ctx, o, s.info.PRRef, progress)
	} else {
		err = ensureLocalCodeMap(ctx, o, s.info, progress)
	}
	if err != nil {
		return nil, err
	}
	// The branch's PR, opened from here or anywhere else, so the result
	// offers to update it rather than to open another, and what it is
	// stacked on (stack.go), which can move the diff's base.
	looked := false
	if s.info.Rev == "" {
		if s, looked, err = syncStack(ctx, s); err != nil {
			return nil, err
		}
	} else if head, _ := originBranch(s.info); head != "" {
		// A branch reviewed by name isn't stacked: it is diffed from the
		// default branch, and its PR is looked up there.
		s.info.URL = openPRFor(ctx, s.info, head)
		looked = s.info.URL != ""
	}
	key := localCacheKey(s, o)
	if !jo.rerun() {
		if r, err := t.Load(key); err == nil {
			if s.info.URL == "" && r.PR.URL != "" {
				// A link once found is kept, as it always was; a branch
				// with a PR isn't offered a stack.
				s.info.URL, s.info.StackCandidates = r.PR.URL, nil
			}
			if looked && !sameStack(r.PR, s.info) {
				if u, err := t.updateResult(key, func(r *PRResult) {
					r.PR.URL, r.PR.StackPR, r.PR.StackCandidates = s.info.URL, s.info.StackPR, s.info.StackCandidates
				}); err == nil {
					r = u
				}
			}
			return r, nil
		}
	}
	policy, attrs, err := sourceConfig(s.src, true)
	if err != nil {
		return nil, err
	}
	pipe, err := buildPipeline(o, policy, attrs)
	if err != nil {
		return nil, err
	}
	pipe.Progress = progress
	if m := loadCodeMap(o.codemapDir); m != nil {
		pipe.CodeMap = &triage.CodeMap{Map: m, Repo: codeMapRepo(m, s.info.PRRef)}
	}
	// A local branch moves the same way a PR does: a commit or a save
	// changes a few units and leaves the rest as they were.
	carry := t.planCarry(ctx, key, s.info.BaseOid, o, jo)
	pipe.CarryFrom = carry.carryFrom()
	t.withRelated(ctx, s.info, s.src)
	return t.runSource(ctx, key, s.info, s.src, pipe, o, carry)
}

func ensureLocalCodeMap(ctx context.Context, o options, info *triage.PRInfo, progress func(string, int, int)) error {
	if o.codemapDir == "" || o.codemapDir == "off" {
		return nil
	}
	ws, err := workspaceDir(o)
	if err != nil {
		return err
	}
	if codeMapHas(loadCodeMap(o.codemapDir), ws, info.PRRef) {
		return nil
	}
	codeMapBuildMu.Lock()
	defer codeMapBuildMu.Unlock()
	if codeMapHas(loadCodeMap(o.codemapDir), ws, info.PRRef) {
		return nil
	}
	progress("codemap", 0, 0)
	local := map[string]checkout{checkoutKey(info.PRRef): {Ref: info.PRRef, Dir: info.LocalPath}}
	name, err := addRepo(ctx, ws, info.PRRef, local)
	if err != nil {
		return skipUnlinked(err)
	}
	if err := buildCodeMap(ctx, o, ws, name); err != nil {
		return err
	}
	if !codeMapHas(reloadCodeMap(o.codemapDir), ws, info.PRRef) {
		return fmt.Errorf("code map did not include %s", name)
	}
	return nil
}

func (t *triager) startLocal(path string, jo jobOptions) (*job, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("enter a repository path")
	}
	return t.startLocalJob(path, func(context.Context, func(string, int, int)) (string, error) { return path, nil }, jo), nil
}

// startRemoteRev triages a rev picked from a forge's list, src being
// https://host/owner/repo#rev: it is fetched into the PR clone first
// (revremote.go), and triaged there as a local rev.
func (t *triager) startRemoteRev(src string, r revRepo, rev string, jo jobOptions) *job {
	return t.startLocalJob(src, func(ctx context.Context, progress func(string, int, int)) (string, error) {
		dir, local, err := fetchRemoteRev(ctx, t.fetcher.Dir, r, rev, progress)
		if err != nil {
			return "", err
		}
		return dir + "#" + local, nil
	}, jo)
}

// startLocalJob runs a local triage job named label on the path resolve
// returns.
func (t *triager) startLocalJob(label string, resolve func(context.Context, func(string, int, int)) (string, error), jo jobOptions) *job {
	j, ctx, progress := t.newJob("triage", label)
	go func() {
		defer t.saveJobLog(j) // after the job's outcome is set below
		path, err := resolve(ctx, progress)
		var r *PRResult
		if err == nil {
			r, err = t.RunLocal(ctx, path, jo, progress)
		}
		j.finish(err)
		t.mu.Lock()
		defer t.mu.Unlock()
		j.Cancelable = false
		if j.cancelled {
			j.Status = "cancelled"
			return
		}
		if err != nil {
			j.Status, j.Error = "error", err.Error()
		} else {
			j.Status, j.Key = "done", r.Key
			j.markCached(r)
		}
	}()
	return j
}

// publishOutcome is what publishLocal did: opened the PR at URL, or, when
// the branch had one already, pushed to it and, with Updated, replaced
// its description, which was Previous.
type publishOutcome struct {
	URL      string
	Updated  bool
	Previous string
	// Pushed is false when origin had the branch at its head already.
	Pushed bool
}

// publishLocal pushes a local result's branch and opens its PR, or
// updates the description of the one the branch already has. describe
// writes the PR body once the checks pass; an empty one leaves a new PR
// to gh's --fill, from the commit messages, and an existing one as it is.
func publishLocal(r *PRResult, describe func(*triage.PRInfo) string) (publishOutcome, error) {
	var none publishOutcome
	if r.PR.LocalPath == "" {
		return none, errors.New("this is not a local result")
	}
	if r.PR.SingleCommit {
		return none, errors.New("create PRs from a branch, not a single commit")
	}
	// A branch compared with base...branch opens its PR into base, so base
	// has to be a branch origin has, or a local one of origin's name.
	if base, _, ok := revRange(r.PR.Rev); ok {
		if _, ref := branchRef(r.PR.LocalPath, base); !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/remotes/origin/") {
			return none, fmt.Errorf("%s is not a branch; compare with a branch to create a PR", base)
		}
	}
	if _, err := triage.Git(r.PR.LocalPath, "fetch", "--quiet", "--no-tags", "origin", fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", r.PR.BaseRef, r.PR.BaseRef)); err != nil {
		return none, err
	}
	s, err := inspectLocal(context.Background(), localSource(r.PR))
	if err != nil {
		return none, err
	}
	p := s.info
	if p.HeadOid != r.PR.HeadOid || p.BaseOid != r.PR.BaseOid || p.SnapshotHash != r.PR.SnapshotHash || p.HeadRef != r.PR.HeadRef || p.BaseRef != r.PR.BaseRef {
		return none, errors.New("repository changed since triage; triage it again")
	}
	if p.Uncommitted {
		return none, errors.New("commit the working tree changes, then triage again before creating a PR")
	}
	if p.Ahead == 0 {
		return none, errors.New("branch has no commits ahead of origin/" + p.BaseRef)
	}
	if p.Owner == "local" {
		return none, errors.New("origin must be a GitHub repository to create a PR")
	}
	head, onOrigin := originBranch(p)
	if head == "" {
		return none, fmt.Errorf("%s is not a local or origin branch; check it out to create a PR", p.HeadRef)
	}
	// A branch only on origin is published as origin has it: there is no
	// local branch to push.
	if onOrigin && remoteTip(context.Background(), p.LocalPath, head) != p.HeadOid {
		return none, fmt.Errorf("origin has %s at a different commit now; fetch and triage it again", head)
	}
	if head == p.BaseRef {
		name := publishBranch(p)
		if tip, err := triage.Git(p.LocalPath, "rev-parse", "--verify", "refs/heads/"+name); err == nil {
			if strings.TrimSpace(tip) != p.HeadOid {
				return none, errors.New("generated PR branch already exists at a different commit")
			}
			if p.Rev == "" {
				if _, err = triage.Git(p.LocalPath, "switch", name); err != nil {
					return none, err
				}
			}
		} else if p.Rev != "" {
			// Not checked out: the branch is made without switching to it.
			if _, err = triage.Git(p.LocalPath, "branch", name, p.HeadOid); err != nil {
				return none, err
			}
		} else if _, err = triage.Git(p.LocalPath, "switch", "-c", name); err != nil {
			return none, err
		}
		head = name
	}
	p.HeadRef = head
	// Looked up again rather than taken from the result: the PR may have
	// been opened, or closed, since the triage.
	existing := openPRFor(context.Background(), p, p.HeadRef)
	body := ""
	if describe != nil {
		body = describe(p)
	}
	if body != "" && p.StackedOn != "" && r.PR.StackPR != "" {
		body = "Stacked on " + r.PR.StackPR + "\n\n" + body
	}
	pushed := remoteTip(context.Background(), p.LocalPath, p.HeadRef) != p.HeadOid
	if pushed {
		if _, err := triage.Git(p.LocalPath, "push", "-u", "origin", p.HeadRef); err != nil {
			return none, err
		}
	}
	if existing == "" {
		url, err := ghPR(p.LocalPath, body, "pr", "create", "--base", p.BaseRef, "--head", p.HeadRef, "--fill")
		if err == nil {
			return publishOutcome{URL: url, Pushed: pushed}, nil
		}
		// Opened where the lookup didn't see it: update that one instead.
		m := prExists.FindStringSubmatch(err.Error())
		if m == nil {
			return none, err
		}
		existing = m[1]
	}
	if body == "" {
		return publishOutcome{URL: existing, Pushed: pushed}, nil
	}
	prev, err := ghPR(p.LocalPath, "", "pr", "view", existing, "--json", "body", "--jq", ".body")
	if err != nil {
		return none, err
	}
	if _, err := ghPR(p.LocalPath, body, "pr", "edit", existing); err != nil {
		return none, err
	}
	return publishOutcome{URL: existing, Updated: true, Previous: prev, Pushed: pushed}, nil
}

// publishBranch is the branch a local result's PR is opened from: its own,
// or a generated one when it is on the base branch.
func publishBranch(p *triage.PRInfo) string {
	head, _ := originBranch(p)
	if head == p.BaseRef {
		return "pr-manager/" + p.HeadOid[:10]
	}
	return head
}

// originBranch is the branch on origin a local result's PR is opened from,
// before a generated one replaces the base branch: the checked-out
// branch, a local branch reviewed by name, or origin/name's name, with
// onOrigin. It is empty for a commit or another remote's branch.
func originBranch(p *triage.PRInfo) (name string, onOrigin bool) {
	if p.Rev == "" {
		return p.HeadRef, false
	}
	if p.SingleCommit {
		return "", false
	}
	// inspectRev prefers a local branch to a remote one of the same name.
	if _, err := triage.Git(p.LocalPath, "show-ref", "--verify", "--quiet", "refs/heads/"+p.HeadRef); err == nil {
		return p.HeadRef, false
	}
	if name, ok := strings.CutPrefix(p.HeadRef, "origin/"); ok {
		return name, true
	}
	return "", false
}

// remoteTip is the commit branch is at on dir's origin; empty when origin
// doesn't have it or can't be reached.
func remoteTip(ctx context.Context, dir, branch string) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := triage.GitCtx(ctx, dir, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	if f := strings.Fields(out); len(f) > 0 {
		return f[0]
	}
	return ""
}

// prExists finds the PR gh pr create names when the branch has one.
var prExists = regexp.MustCompile(`already exists:\s*(https://\S+/pull/\d+)`)

// setPRBody replaces the description of the PR at url with body.
func setPRBody(dir, url, body string) error {
	_, err := ghPR(dir, body, "pr", "edit", url)
	return err
}

// ghPR runs a gh pr command in dir and returns its output, trimmed. A
// body goes to gh on stdin as --body-file -; with create, it overrides
// --fill's body and the title still comes from it.
func ghPR(dir, body string, args ...string) (string, error) {
	if body != "" {
		args = append(args, "--body-file", "-")
	}
	cmd := proc.Command("gh", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			if ghErr := triage.GHError(err, e.Stderr); ghErr != nil {
				return "", ghErr
			}
			return "", fmt.Errorf("gh %s %s: %w: %s", args[0], args[1], err, strings.TrimSpace(string(e.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// openPRFor is the URL of the open PR from branch head into p's base on
// the checkout's origin; empty when there is none, origin isn't on
// GitHub, or gh can't tell.
func openPRFor(ctx context.Context, p *triage.PRInfo, head string) string {
	if p.Owner == "local" || head == "" || head == p.BaseRef || strings.Contains(strings.ToLower(p.HostName()), "gitlab") {
		return ""
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := proc.CommandContext(ctx, "gh", "pr", "list", "-R", p.RepoArg(), "--head", head, "--base", p.BaseRef, "--state", "open", "--limit", "1", "--json", "url", "--jq", ".[0].url // empty")
	cmd.Dir = p.LocalPath
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// localSource is what a local result was triaged from: the checkout's
// path, with #rev when it was a commit or branch in it.
func localSource(p *triage.PRInfo) string {
	if p.Rev != "" {
		return p.LocalPath + "#" + p.Rev
	}
	return p.LocalPath
}

func localPathID(path string) string {
	h := sha256.Sum256([]byte(path))
	return hex.EncodeToString(h[:10])
}
