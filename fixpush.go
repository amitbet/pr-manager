package main

// A fix leaves its changes uncommitted in the worktree or clone it ran in
// (see runFix). This is what happens to them after: the Issues tab lists
// the PR's pending fixes and what each one fixed, shows their changes,
// and commits them, pushes them to the PR's branch, one or all together,
// or throws them away.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/triage"
)

// fixedIssue is an issue or review thread a fix's checks found fixed.
// Scope matches it to the issue on the PR's own review (see issueScope).
type fixedIssue struct {
	Scope    string `json:"scope"`
	UnitID   string `json:"unit_id"`
	File     string `json:"file"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Thread   string `json:"thread,omitempty"`
	URL      string `json:"url,omitempty"`
	// Swept: the fix was not asked to work on it, and the fixer said its
	// patch resolved it too, for Reason.
	Swept  bool   `json:"swept,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func threadScope(id string) string { return "thread:" + id }

// fixedIssues is carried with what this fix added: the issues a round
// tried that a check found fixed (found) and the re-triage did not find
// again, and the targeted threads it found addressed.
func fixedIssues(carried []fixedIssue, next *PRResult, tried map[string]targetedIssue, found, threads map[string]bool) []fixedIssue {
	out := append([]fixedIssue(nil), carried...)
	have := map[string]bool{}
	for _, f := range out {
		have[f.Scope] = true
	}
	live := map[string]bool{}
	for _, f := range next.Files {
		for _, u := range f.Units {
			for _, is := range u.Issues {
				live[issueScope(u.ID, is)] = true
			}
		}
	}
	var add []fixedIssue
	for s, x := range tried {
		if found[s] && !live[s] && !have[s] {
			add = append(add, fixedIssue{Scope: s, UnitID: x.UnitID, File: x.File, Severity: x.Issue.Severity, Title: x.Issue.Title})
		}
	}
	sort.Slice(add, func(i, j int) bool {
		a, b := add[i], add[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.UnitID != b.UnitID {
			return a.UnitID < b.UnitID
		}
		return a.Title < b.Title
	})
	out = append(out, add...)
	for _, f := range next.Files {
		for _, u := range f.Units {
			for i := range u.Threads {
				th := &u.Threads[i]
				if s := threadScope(th.ID); threads[th.ID] && th.Fixed && !have[s] {
					have[s] = true
					is := th.AsIssue()
					out = append(out, fixedIssue{Scope: s, UnitID: u.ID, File: f.Path, Severity: is.Severity, Title: is.Title, Thread: th.ID, URL: th.URL})
				}
			}
		}
	}
	return out
}

// fixFile is one file a fix changed, with its hunks when the diff was
// asked for.
type fixFile struct {
	Path    string            `json:"path"`
	OldPath string            `json:"old_path,omitempty"`
	Status  triage.FileStatus `json:"status"`
	Binary  bool              `json:"binary,omitempty"`
	Adds    int               `json:"adds"`
	Dels    int               `json:"dels"`
	Hunks   []triage.Hunk     `json:"hunks,omitempty"`
	// Units are the hunks cut the way a review cuts them, so an issue's
	// unit finds the changes a fix made in it.
	Units []fixUnit `json:"units,omitempty"`
}

// fixUnit is a unit of a fix's changes to a file.
type fixUnit struct {
	ID    string        `json:"id"`
	Hunks []triage.Hunk `json:"hunks"`
}

// pendingFix is a fix checkout as it is now.
type pendingFix struct {
	Key       string    `json:"key"` // the newest result of the checkout
	CreatedAt time.Time `json:"created_at"`
	Dir       string    `json:"dir"`
	Branch    string    `json:"branch"`
	Location  string    `json:"location"`
	Rounds    int       `json:"rounds"`
	Base      string    `json:"base"`
	Head      string    `json:"head,omitempty"`
	// State is uncommitted, committed (on top of base, not pushed),
	// pushed, superseded (dropped for the PR's own version of its lines),
	// empty (nothing changed), done (a fix in the user's checkout
	// that moved on, so whatever became of it is theirs) or missing.
	State    string       `json:"state"`
	Commits  int          `json:"commits,omitempty"`
	Files    []fixFile    `json:"files"`
	Fixed    []fixedIssue `json:"fixed,omitempty"`
	PushedAs string       `json:"pushed_as,omitempty"`
	// Inferred: the fix was made before fixes recorded what they fixed,
	// so Fixed is the issues of the review it is listed for that the fix's
	// result no longer has.
	Inferred bool `json:"inferred,omitempty"`
	// Stale: the fix started from another commit than the result it is
	// listed for reviewed.
	Stale bool `json:"stale,omitempty"`
	// NoPush is why it can't be pushed to the PR, "" when it can.
	NoPush string `json:"no_push,omitempty"`
	// SupersededBy is the PR head whose own version of the lines a
	// superseded fix lost to (see repoCheckout.rebase).
	SupersededBy string `json:"superseded_by,omitempty"`
	// Kind "branch" is a commit on the PR's branch in the repository's
	// fix checkout (see fixbranch.go); Commit is it, and Touches the
	// earlier fixes whose lines it changed. Other: a commit no fix made,
	// with its Subject.
	Kind    string     `json:"kind,omitempty"`
	Commit  string     `json:"commit,omitempty"`
	Touches []fixTouch `json:"touches,omitempty"`
	Other   bool       `json:"other,omitempty"`
	Subject string     `json:"subject,omitempty"`
}

// fixMark puts a pending fix on an issue or thread of the result.
type fixMark struct {
	Unit   string `json:"unit"`
	Issue  *int   `json:"issue,omitempty"`
	Thread string `json:"thread,omitempty"`
	Key    string `json:"key"`
	// Commit is the fix's on the PR's branch, "" for a fix in a checkout
	// of its own.
	Commit string `json:"commit,omitempty"`
	State  string `json:"state"`
	// Swept and Reason are the fixedIssue's: the fix wasn't asked to work
	// on it, and the fixer said its patch resolved it too.
	Swept  bool   `json:"swept,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func fixBaseOf(r *PRResult) string {
	if r.FixBase != "" {
		return r.FixBase
	}
	return r.PR.HeadOid // saved before fixes kept their base
}

// fixPatch is the checkout's changes from base as a unified diff: tracked
// files, committed or not, and the untracked ones git status shows.
func fixPatch(ctx context.Context, dir, base string) (string, error) {
	out, err := triage.GitCtx(ctx, dir, "diff", "--no-color", "--no-ext-diff", "-M", base)
	if err != nil {
		return "", err
	}
	raw, err := triage.GitCtx(ctx, dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	for _, p := range strings.Split(raw, "\x00") {
		if p == "" {
			continue
		}
		// --no-index exits 1 when the files differ, which they do.
		cmd := proc.CommandContext(ctx, "git", triage.GitArgs("diff", "--no-color", "--no-ext-diff", "--no-index", "--", os.DevNull, p)...)
		cmd.Dir = dir
		b, err := cmd.Output()
		if ee, ok := err.(*exec.ExitError); err != nil && (!ok || ee.ExitCode() != 1) {
			return "", fmt.Errorf("diff %s: %w", p, err)
		}
		out += string(b)
	}
	return out, nil
}

// fixDiff is the checkout's changes from base (see fixPatch), by file.
func fixDiff(ctx context.Context, dir, base string, hunks bool) ([]fixFile, error) {
	out, err := fixPatch(ctx, dir, base)
	if err != nil {
		return nil, err
	}
	return diffFiles(out, hunks, func(path string) ([]byte, error) {
		if !safeRepoPath(path) {
			return nil, fmt.Errorf("unsafe path %q", path)
		}
		return os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
	})
}

// diffFiles is a unified diff by file, with line counts, and its hunks and
// units when asked for; content is a file as the diff leaves it.
func diffFiles(out string, hunks bool, content triage.ContentFunc) ([]fixFile, error) {
	files, err := triage.ParseDiff(out)
	if err != nil {
		return nil, err
	}
	res := make([]fixFile, 0, len(files))
	for _, f := range files {
		ff := fixFile{Path: f.Path, Status: f.Status, Binary: f.Binary}
		if f.OldPath != f.Path {
			ff.OldPath = f.OldPath
		}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				switch {
				case strings.HasPrefix(l, "+"):
					ff.Adds++
				case strings.HasPrefix(l, "-"):
					ff.Dels++
				}
			}
		}
		if hunks {
			ff.Hunks = f.Hunks
			for _, u := range triage.BuildUnits([]triage.FileDiff{f}, content, 0) {
				ff.Units = append(ff.Units, fixUnit{ID: u.ID, Hunks: u.Hunks})
			}
		}
		res = append(res, ff)
	}
	return res, nil
}

// inspectFix reads r's checkout as it is now.
func inspectFix(ctx context.Context, r *PRResult, hunks bool) (*pendingFix, error) {
	p := &pendingFix{Key: r.Key, CreatedAt: r.CreatedAt, Dir: r.LocalFixDir, Branch: r.LocalFixBranch, Location: r.LocalFixLocation,
		Rounds: r.FixRounds, Base: fixBaseOf(r), Fixed: r.FixedIssues, PushedAs: r.FixPushedAs, Files: []fixFile{}}
	if p.Location == "" {
		p.Location = "worktree"
	}
	if _, err := os.Stat(filepath.Join(r.LocalFixDir, ".git")); err != nil {
		p.State = "missing"
		return p, nil
	}
	head, err := triage.GitCtx(ctx, r.LocalFixDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	p.Head = strings.TrimSpace(head)
	dirty, err := hasUncommitted(r.LocalFixDir)
	if err != nil {
		return nil, err
	}
	if n, err := triage.GitCtx(ctx, r.LocalFixDir, "rev-list", "--count", p.Base+"..HEAD"); err == nil {
		p.Commits, _ = strconv.Atoi(strings.TrimSpace(n))
	}
	switch {
	case p.Location == "branch" && p.Head != p.Base:
		p.State = "done"
		return p, nil
	case dirty:
		p.State = "uncommitted"
	case r.FixPushed != "" && p.Head == r.FixPushed:
		p.State = "pushed"
	case p.Head != p.Base:
		p.State = "committed"
	default:
		p.State = "empty"
	}
	if p.Files, err = fixDiff(ctx, r.LocalFixDir, p.Base, hunks); err != nil {
		return nil, err
	}
	return p, nil
}

// samePR is whether fix r was made for the PR or checkout of result of.
func samePR(of, r *PRResult) bool {
	if of.PR == nil || r.PR == nil {
		return false
	}
	if of.PR.LocalPath != "" || r.PR.LocalPath != "" {
		return of.PR.LocalPath == r.PR.LocalPath
	}
	return of.PR.PRRef == r.PR.PRRef
}

// fixResults is the newest fix result of each checkout made for of's PR.
// A fix that continued another is newer and has its changes too.
func (t *triager) fixResults(of *PRResult) []*PRResult {
	paths, _ := filepath.Glob(filepath.Join(t.results, "fix__*.json"))
	newest := map[string]*PRResult{}
	for _, p := range paths {
		r, err := t.Load(strings.TrimSuffix(filepath.Base(p), ".json"))
		if err != nil || r.LocalFixDir == "" || !samePR(of, r) || t.checkouts.isRepoCheckout(r.LocalFixDir) {
			continue
		}
		id := r.LocalFixDir + "\x00" + r.LocalFixBranch
		if n := newest[id]; n == nil || r.CreatedAt.After(n.CreatedAt) {
			newest[id] = r
		}
	}
	out := make([]*PRResult, 0, len(newest))
	for _, r := range newest {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// pendingFixes lists of's PR's fix checkouts that still hold something
// for the reviewer, and marks the issues and threads of of they fixed.
// A pushed fix stays listed until of is a review of what it pushed.
func (t *triager) pendingFixes(ctx context.Context, of *PRResult) ([]*pendingFix, []fixMark, []fixClaim, error) {
	var out []*pendingFix
	type fixedBy struct {
		p *pendingFix
		f fixedIssue
	}
	scopes := map[string]fixedBy{}
	for _, r := range t.fixResults(of) {
		p, err := inspectFix(ctx, r, false)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", r.LocalFixDir, err)
		}
		chain := of.LocalFixDir == r.LocalFixDir && of.LocalFixBranch == r.LocalFixBranch
		switch p.State {
		case "missing", "empty", "done":
			continue
		case "pushed":
			if !chain && (p.PushedAs == of.PR.HeadOid || t.contains(of, p.PushedAs)) {
				continue
			}
		}
		p.Stale = !chain && p.Base != of.PR.HeadOid
		if r.FixBase == "" && len(p.Fixed) == 0 && !chain && !p.Stale {
			p.Fixed, p.Inferred = vanished(of, r), true
		}
		switch {
		case r.PR.LocalPath != "":
			p.NoPush = "a local review: commit the fix, and its branch is in your repository"
		case p.State == "pushed":
			p.NoPush = "already pushed"
		case r.PR.State != "OPEN":
			p.NoPush = "the PR is " + strings.ToLower(r.PR.State)
		case p.Stale:
			p.NoPush = "the fix started from " + shortOid(p.Base) + ", not the head this review is of"
		}
		out = append(out, p)
		if !p.Stale {
			for _, f := range p.Fixed {
				scopes[f.Scope] = fixedBy{p, f}
			}
		}
	}
	branch, running, err := t.branchFixes(ctx, of)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, p := range branch {
		out = append(out, p)
		if !p.Stale && p.State != "superseded" {
			for _, f := range p.Fixed {
				scopes[f.Scope] = fixedBy{p, f}
			}
		}
	}
	// A running fix marks what it works on, unless a fix has it already.
	for _, c := range running {
		state := "running"
		if c.Waiting != "" {
			state = "waiting"
		}
		for _, s := range c.Targets {
			if _, ok := scopes[s]; !ok {
				scopes[s] = fixedBy{&pendingFix{Key: c.Job, State: state}, fixedIssue{Reason: c.Waiting}}
			}
		}
	}
	var marks []fixMark
	for _, f := range of.Files {
		for _, u := range f.Units {
			for i, is := range u.Issues {
				if b, ok := scopes[issueScope(u.ID, is)]; ok {
					marks = append(marks, fixMark{Unit: u.ID, Issue: &i, Key: b.p.Key, Commit: b.p.Commit, State: b.p.State, Swept: b.f.Swept, Reason: b.f.Reason})
				}
			}
			for _, th := range u.Threads {
				if b, ok := scopes[threadScope(th.ID)]; ok {
					marks = append(marks, fixMark{Unit: u.ID, Thread: th.ID, Key: b.p.Key, Commit: b.p.Commit, State: b.p.State, Swept: b.f.Swept, Reason: b.f.Reason})
				}
			}
		}
	}
	return out, marks, running, nil
}

// vanished is the live issues and confirmed threads of review that fix's
// result no longer has: what an older fix, which didn't record what it
// fixed, most likely fixed.
func vanished(review, fix *PRResult) []fixedIssue {
	left := map[string]bool{}
	for _, f := range fix.Files {
		for _, u := range f.Units {
			for _, is := range u.Issues {
				left[issueScope(u.ID, is)] = true
			}
			for _, th := range u.Threads {
				if !th.Fixed {
					left[threadScope(th.ID)] = true
				}
			}
		}
	}
	var out []fixedIssue
	for _, f := range review.Files {
		for _, u := range f.Units {
			for _, is := range u.Issues {
				if s := issueScope(u.ID, is); !is.Dismissed && !left[s] {
					out = append(out, fixedIssue{Scope: s, UnitID: u.ID, File: f.Path, Severity: is.Severity, Title: is.Title})
				}
			}
			for i := range u.Threads {
				th := &u.Threads[i]
				if s := threadScope(th.ID); th.Status == triage.ThreadValid && !th.Fixed && !left[s] {
					is := th.AsIssue()
					out = append(out, fixedIssue{Scope: s, UnitID: u.ID, File: f.Path, Severity: is.Severity, Title: is.Title, Thread: th.ID, URL: th.URL})
				}
			}
		}
	}
	return out
}

// contains is whether the head of's review is at or after commit, in the
// clone of's PR was fetched into.
func (t *triager) contains(of *PRResult, commit string) bool {
	dir := t.fetcher.RepoDir(of.PR.PRRef)
	if of.PR.LocalPath != "" {
		dir = of.PR.LocalPath
	}
	_, err := triage.Git(dir, "merge-base", "--is-ancestor", commit, of.PR.HeadOid)
	return err == nil
}

var errFixBusy = errors.New("a fix is running; wait for it to finish")

// loadFix loads a fix result whose checkout is still there.
func (t *triager) loadFix(key string) (*PRResult, error) {
	r, err := t.Load(key)
	if err != nil {
		return nil, err
	}
	if r.LocalFixDir == "" {
		return nil, errors.New("not a fix result")
	}
	if t.checkouts.isRepoCheckout(r.LocalFixDir) {
		return nil, errors.New("this fix is a commit on the PR's branch: use the PR's fixes")
	}
	if _, err := os.Stat(filepath.Join(r.LocalFixDir, ".git")); err != nil {
		return nil, fmt.Errorf("the fix checkout is gone: %s", r.LocalFixDir)
	}
	return r, nil
}

// commitFix commits a fix's changes in its checkout.
func (t *triager) commitFix(ctx context.Context, o options, key string) (*pendingFix, error) {
	if !t.fixMu.TryLock() {
		return nil, errFixBusy
	}
	defer t.fixMu.Unlock()
	r, err := t.loadFix(key)
	if err != nil {
		return nil, err
	}
	if o.summarizer == "off" {
		return nil, errors.New("enable a summarizer to write the commit message")
	}
	commit, err := commitFixes(ctx, o, r.LocalFixDir)
	if err != nil {
		return nil, err
	}
	if commit == "" {
		return nil, errors.New("the fix has nothing uncommitted")
	}
	return inspectFix(ctx, r, false)
}

// prTarget is where a PR's head branch lives, to push to.
type prTarget struct {
	State     string `json:"state"`
	HeadOid   string `json:"headRefOid"`
	HeadRef   string `json:"headRefName"`
	Cross     bool   `json:"isCrossRepository"`
	MaintEdit bool   `json:"maintainerCanModify"`
	Repo      struct {
		Name string `json:"name"`
	} `json:"headRepository"`
	Owner struct {
		Login string `json:"login"`
	} `json:"headRepositoryOwner"`
}

func prPushTarget(ctx context.Context, ref triage.PRRef) (*prTarget, error) {
	cmd := proc.CommandContext(ctx, "gh", "pr", "view", strconv.Itoa(ref.Number), "-R", ref.RepoArg(), "--json",
		"state,headRefOid,headRefName,isCrossRepository,maintainerCanModify,headRepository,headRepositoryOwner")
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = ee.Stderr
		}
		if ghErr := triage.GHError(err, stderr); ghErr != nil {
			return nil, ghErr
		}
		return nil, fmt.Errorf("gh pr view: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	var p prTarget
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// remote is what to push the PR's head branch to: the clone's origin, or
// the fork the branch is in.
func (p *prTarget) remote(ref triage.PRRef) string {
	if !p.Cross {
		return "origin"
	}
	return fmt.Sprintf("https://%s/%s/%s.git", ref.HostName(), p.Owner.Login, p.Repo.Name)
}

// pushFixes commits the fixes of keys, all made for the same open PR from
// its current head, and pushes them to its branch: one fix as it is,
// several as one cherry-picked series in a scratch worktree, in the order
// they were made. The push is a fast-forward of the head they started
// from, so it never overwrites a commit pushed meanwhile. It returns the
// PR's new head.
func (t *triager) pushFixes(ctx context.Context, o options, keys []string) (string, error) {
	if len(keys) == 0 {
		return "", errors.New("no fixes to push")
	}
	if !t.fixMu.TryLock() {
		return "", errFixBusy
	}
	defer t.fixMu.Unlock()
	var fixes []*PRResult
	for _, k := range keys {
		r, err := t.loadFix(k)
		if err != nil {
			return "", err
		}
		if r.PR.LocalPath != "" {
			return "", errors.New("a local review's fix is committed in your repository, not pushed from here")
		}
		if len(fixes) > 0 && !samePR(fixes[0], r) {
			return "", errors.New("the fixes are for different PRs")
		}
		fixes = append(fixes, r)
	}
	sort.Slice(fixes, func(i, j int) bool { return fixes[i].CreatedAt.Before(fixes[j].CreatedAt) })
	ref := fixes[0].PR.PRRef
	target, err := prPushTarget(ctx, ref)
	if err != nil {
		return "", err
	}
	if target.State != "OPEN" {
		return "", fmt.Errorf("PR #%d is %s", ref.Number, strings.ToLower(target.State))
	}
	for _, r := range fixes {
		if b := fixBaseOf(r); b != target.HeadOid {
			return "", fmt.Errorf("PR #%d's branch is at %s, and a fix started from %s: triage the PR again and fix its new head", ref.Number, shortOid(target.HeadOid), shortOid(b))
		}
	}
	if o.summarizer == "off" {
		return "", errors.New("enable a summarizer to write the commit messages")
	}
	heads := make([]string, len(fixes))
	var series []int // the fixes with commits to push
	for i, r := range fixes {
		if _, err := commitFixes(ctx, o, r.LocalFixDir); err != nil {
			return "", fmt.Errorf("commit %s: %w", r.LocalFixDir, err)
		}
		head, err := triage.GitCtx(ctx, r.LocalFixDir, "rev-parse", "HEAD")
		if err != nil {
			return "", err
		}
		heads[i] = strings.TrimSpace(head)
		if heads[i] != target.HeadOid {
			series = append(series, i)
		}
	}
	if len(series) == 0 {
		return "", errors.New("the fixes have no changes to push")
	}
	dir := fixes[series[0]].LocalFixDir
	if len(series) > 1 {
		scratch, cleanup, err := t.combineFixes(ctx, ref, target.HeadOid, fixes, heads, series)
		if err != nil {
			return "", err
		}
		defer cleanup()
		dir = scratch
	}
	cmd := proc.CommandContext(ctx, "git", triage.GitArgs("push", "--porcelain", target.remote(ref), "HEAD:refs/heads/"+target.HeadRef)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git push to %s: %w: %s", target.HeadRef, err, strings.TrimSpace(string(out)))
	}
	pushed, err := triage.GitCtx(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	pushed = strings.TrimSpace(pushed)
	for _, i := range series {
		if _, err := t.updateResult(fixes[i].Key, func(r *PRResult) {
			r.FixPushed, r.FixPushedAs = heads[i], pushed
		}); err != nil {
			return pushed, fmt.Errorf("pushed, but saving it failed: %w", err)
		}
	}
	return pushed, nil
}

// combineFixes cherry-picks the commits of each fix in series onto base,
// in a scratch worktree of the PR's clone, which every fix checkout of a
// PR shares its objects with. A fix that conflicts with the ones before it
// stops the push.
func (t *triager) combineFixes(ctx context.Context, ref triage.PRRef, base string, fixes []*PRResult, heads []string, series []int) (string, func(), error) {
	repo := t.fetcher.RepoDir(ref)
	var idb [6]byte
	_, _ = rand.Read(idb[:])
	dir, err := filepath.Abs(filepath.Join(t.opts.cache, "fixes", "push-"+hex.EncodeToString(idb[:])))
	if err != nil {
		return "", nil, err
	}
	if _, err := triage.GitCtx(ctx, repo, "worktree", "add", "--detach", dir, base); err != nil {
		return "", nil, err
	}
	cleanup := func() {
		if _, err := triage.Git(repo, "worktree", "remove", "--force", dir); err != nil {
			_ = os.RemoveAll(dir)
		}
		_, _ = triage.Git(repo, "worktree", "prune")
	}
	for _, i := range series {
		if _, err := triage.GitCtx(ctx, dir, "cherry-pick", base+".."+heads[i]); err != nil {
			_, _ = triage.Git(dir, "cherry-pick", "--abort")
			cleanup()
			return "", nil, fmt.Errorf("the fix in %s conflicts with the ones before it; push them one at a time, triaging the PR between: %w", fixes[i].LocalFixDir, err)
		}
	}
	return dir, cleanup, nil
}

// discardFix throws away a fix checkout this app made, its branch, and
// every result of it. A fix in the user's own checkout is theirs to undo.
func (t *triager) discardFix(key string) error {
	if !t.fixMu.TryLock() {
		return errFixBusy
	}
	defer t.fixMu.Unlock()
	r, err := t.Load(key)
	if err != nil {
		return err
	}
	if r.LocalFixDir == "" {
		return errors.New("not a fix result")
	}
	if t.checkouts.isRepoCheckout(r.LocalFixDir) {
		return errors.New("this fix is a commit on the PR's branch: drop it from the PR's fixes")
	}
	repo := t.fetcher.RepoDir(r.PR.PRRef)
	if r.PR.LocalPath != "" {
		repo = r.PR.LocalPath
	}
	// The branch is this app's in its own clone; in the user's repository
	// only one it named.
	ours := r.PR.LocalPath == "" || strings.HasPrefix(r.LocalFixBranch, "pr-manager/")
	if _, err := os.Stat(filepath.Join(r.LocalFixDir, ".git")); err == nil {
		switch r.LocalFixLocation {
		case "branch":
			return errors.New("this fix is in your own checkout: undo it there")
		case "clone":
			if r.PR.LocalPath != "" {
				return errors.New("this fix is in your own checkout: undo it there")
			}
			_, _ = triage.Git(repo, "reset", "-q", "--hard")
			_, _ = triage.Git(repo, "clean", "-q", "-fd")
			if _, err := triage.Git(repo, "switch", "-q", "--detach", fixBaseOf(r)); err != nil {
				return err
			}
			if ours && r.LocalFixBranch != "" {
				_, _ = triage.Git(repo, "branch", "-D", r.LocalFixBranch)
			}
		default:
			root, err := filepath.Abs(filepath.Join(t.opts.cache, "fixes"))
			if err != nil {
				return err
			}
			if path, err := filepath.Abs(r.LocalFixDir); err != nil || !strings.HasPrefix(path, root+string(os.PathSeparator)) {
				return errors.New("fix worktree is outside the cache")
			}
			removeFixWorktree(repo, r.LocalFixDir, r.LocalFixBranch, ours && r.LocalFixBranch != "")
		}
	}
	paths, _ := filepath.Glob(filepath.Join(t.results, "fix__*.json"))
	for _, p := range paths {
		k := strings.TrimSuffix(filepath.Base(p), ".json")
		o, err := t.Load(k)
		if err != nil || o.LocalFixDir != r.LocalFixDir || o.LocalFixBranch != r.LocalFixBranch {
			continue
		}
		mu := t.resultLock(k)
		mu.Lock()
		err = os.Remove(p)
		mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}
