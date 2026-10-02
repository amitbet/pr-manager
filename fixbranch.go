package main

// What runs a PR fix in its repository's checkout (see fixcheckout.go),
// and what the Issues tab does with the fixes there: lists them, shows a
// fix's changes, pushes the branch, drops one fix, completes the PR, and
// moves the fixes made before this, each in a worktree of its own, onto
// their PR's branch.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/internal/activity"
	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/triage"
)

// targetScopes are the scopes of what a fix targets, to mark on the
// issues while it runs.
func targetScopes(issues []targetedIssue) []string {
	var out []string
	for _, x := range issues {
		if x.Comment != nil {
			out = append(out, threadScope(x.Comment.Thread))
		} else {
			out = append(out, issueScope(x.UnitID, x.Issue))
		}
	}
	return out
}

// runBranchFix fixes a PR in its repository's checkout, as a commit on
// the PR's branch there.
func (t *triager) runBranchFix(ctx context.Context, jobID string, old *PRResult, req fixRequest, progress func(string, int, int)) (*PRResult, error) {
	o := t.options(req.jobOptions)
	if o.summarizer == "off" {
		return nil, errors.New("enable a summarizer to fix and review issues")
	}
	issues := fixTargets(old, req)
	if len(issues) == 0 {
		return nil, errors.New("no matching review issues")
	}
	ref := old.PR.PRRef
	if _, err := triage.GitCtx(ctx, t.fetcher.RepoDir(ref), "cat-file", "-e", old.PR.HeadOid+"^{commit}"); err != nil {
		if _, _, err := t.fetcher.Fetch(ctx, ref); err != nil {
			return nil, err
		}
	}
	files := fixFiles(issues)
	co, release, err := t.checkouts.acquire(ctx, o, old.PR, jobID, files, targetScopes(issues), func(why string) {
		progress("wait: "+why, 0, 0)
	})
	if err != nil {
		return nil, err
	}
	defer release()
	p, err := co.pr(ref.Number)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("PR #%d has no branch in the fix checkout", ref.Number)
	}
	var paths []string
	for f := range files {
		paths = append(paths, f)
	}
	sort.Strings(paths)
	fr := fixRun{dir: co.dir, branch: p.Branch, location: "repo", base: p.Base, files: files}
	next, applied, err := t.fixRounds(withTreeLock(ctx, &co.tree), jobID, old, req, o, fr, issues, progress)
	if err != nil {
		co.restoreFiles(context.Background(), paths)
		return nil, err
	}
	commit, err := co.commitFix(ctx, ref.Number, paths, fixEntry{Key: next.Key, Files: paths, Fixed: next.FixedIssues, Rounds: applied, CreatedAt: time.Now()})
	if err != nil {
		co.restoreFiles(context.Background(), paths)
		return nil, fmt.Errorf("commit the fix: %w", err)
	}
	if commit == "" {
		t.warn(ctx, jobID, "the fix changed nothing")
	}
	next.FixCommit = commit
	if err := t.saveFixResult(next); err != nil {
		return nil, err
	}
	return next, nil
}

// commitFiles is a commit's changes by file.
func commitFiles(ctx context.Context, dir, commit string, hunks bool) ([]fixFile, error) {
	out, err := triage.GitCtx(ctx, dir, "diff", "--no-color", "--no-ext-diff", "-M", commit+"^", commit)
	if err != nil {
		return nil, err
	}
	return diffFiles(out, hunks, func(path string) ([]byte, error) {
		b, err := triage.GitCtx(ctx, dir, "show", commit+":"+path)
		return []byte(b), err
	})
}

func isAncestor(dir, a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	_, err := triage.Git(dir, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// branchFixes lists the fixes on of's PR's branch that of's head doesn't
// have, oldest first, and what runs there now.
func (t *triager) branchFixes(ctx context.Context, of *PRResult) ([]*pendingFix, []fixClaim, error) {
	if of.PR == nil || of.PR.LocalPath != "" {
		return nil, nil, nil
	}
	ref := of.PR.PRRef
	running := t.checkouts.claimsOf(ref)
	t.checkouts.mu.Lock()
	co := t.checkouts.get(ref)
	t.checkouts.mu.Unlock()
	p, err := co.pr(ref.Number)
	if err != nil || p == nil {
		return nil, running, err
	}
	tip, err := triage.GitCtx(ctx, co.clone, "rev-parse", "--verify", "--quiet", "refs/heads/"+p.Branch)
	if err != nil {
		return nil, running, nil // completed meanwhile
	}
	tip = strings.TrimSpace(tip)
	// Every commit on the branch after its base, with the fixes' records.
	raw, err := triage.GitCtx(ctx, co.clone, "rev-list", "--reverse", p.Base+".."+tip)
	if err != nil {
		return nil, running, err
	}
	byCommit := map[string]fixEntry{}
	for _, e := range p.Entries {
		byCommit[e.Commit] = e
	}
	touches := co.touches(ctx, p)
	stale := of.PR.HeadOid != p.Base && isAncestor(co.clone, of.PR.HeadOid, p.Base)
	var out []*pendingFix
	for _, c := range strings.Fields(raw) {
		if isAncestor(co.clone, c, of.PR.HeadOid) {
			continue // on the PR as this review saw it
		}
		e, ok := byCommit[c]
		pf := &pendingFix{Kind: "branch", Key: e.Key, Commit: c, CreatedAt: e.CreatedAt, Dir: co.dir, Branch: p.Branch, Location: "repo",
			Rounds: e.Rounds, Base: p.Base, Head: c, State: "committed", Fixed: e.Fixed, Touches: touches[c], Stale: stale}
		if !ok {
			pf.Other = true
			if subj, err := triage.GitCtx(ctx, co.clone, "log", "-1", "--format=%s%x00%cI", c); err == nil {
				s, at, _ := strings.Cut(strings.TrimSpace(subj), "\x00")
				pf.Subject = s
				pf.CreatedAt, _ = time.Parse(time.RFC3339, at)
			}
		}
		if isAncestor(co.clone, c, p.PushedAs) {
			pf.State, pf.PushedAs = "pushed", p.PushedAs
		}
		if pf.Files, err = commitFiles(ctx, co.clone, c, false); err != nil {
			return nil, running, err
		}
		switch {
		case pf.State == "pushed":
			pf.NoPush = "already pushed"
		case of.PR.State != "OPEN":
			pf.NoPush = "the PR is " + strings.ToLower(of.PR.State)
		case stale:
			pf.NoPush = "the fixes are on " + shortOid(p.Base) + ", newer than the head this review is of"
		}
		out = append(out, pf)
	}
	for _, e := range p.Superseded {
		pf := &pendingFix{Kind: "branch", Key: e.Key, Commit: e.Commit, CreatedAt: e.CreatedAt, Dir: co.dir, Branch: p.Branch, Location: "repo",
			Rounds: e.Rounds, Base: p.Base, Head: e.Commit, State: "superseded", Fixed: e.Fixed, Other: e.Key == "", SupersededBy: e.By,
			NoPush: "the PR changed the same lines in " + shortOid(e.By)}
		if pf.Other {
			if subj, err := triage.GitCtx(ctx, co.clone, "log", "-1", "--format=%s", e.Commit); err == nil {
				pf.Subject = strings.TrimSpace(subj)
			}
		}
		// The commit is off the branch: once git prunes it, only the
		// files' names are left.
		if pf.Files, err = commitFiles(ctx, co.clone, e.Commit, false); err != nil {
			pf.Files = nil
			for _, f := range e.Files {
				pf.Files = append(pf.Files, fixFile{Path: f})
			}
		}
		out = append(out, pf)
	}
	return out, running, nil
}

// superseded is p's superseded fix commit, if it is one.
func (p *prFixes) superseded(commit string) bool {
	for _, e := range p.Superseded {
		if e.Commit == commit {
			return true
		}
	}
	return false
}

// fixCheckoutOf is of's PR's repository checkout and branch state.
func (t *triager) fixCheckoutOf(key string) (*PRResult, *repoCheckout, *prFixes, error) {
	of, err := t.Load(key)
	if err != nil {
		return nil, nil, nil, err
	}
	if of.PR == nil || of.PR.LocalPath != "" {
		return nil, nil, nil, errors.New("a local review's fixes are not on a PR branch")
	}
	t.checkouts.mu.Lock()
	co := t.checkouts.get(of.PR.PRRef)
	t.checkouts.mu.Unlock()
	p, err := co.pr(of.PR.Number)
	if err != nil {
		return nil, nil, nil, err
	}
	if p == nil {
		return nil, nil, nil, errors.New("the PR has no fixes")
	}
	return of, co, p, nil
}

// branchCommitDiff is one commit of key's PR's branch, with its hunks.
func (t *triager) branchCommitDiff(ctx context.Context, key, commit string) ([]fixFile, error) {
	_, co, p, err := t.fixCheckoutOf(key)
	if err != nil {
		return nil, err
	}
	if p.superseded(commit) {
		files, err := commitFiles(ctx, co.clone, commit, true)
		if err != nil {
			return nil, errors.New("git no longer has this superseded fix's changes")
		}
		return files, nil
	}
	if !isAncestor(co.clone, p.Base, commit) || !isAncestor(co.clone, commit, "refs/heads/"+p.Branch) {
		return nil, errors.New("not a fix on the PR's branch")
	}
	return commitFiles(ctx, co.clone, commit, true)
}

// pushBranch pushes key's PR's fixes, everything on its branch, to the
// PR's branch on GitHub. It is a fast-forward of the PR's head, so a
// commit pushed meanwhile is never overwritten. It returns the new head.
func (t *triager) pushBranch(ctx context.Context, key string) (string, error) {
	of, co, p, err := t.fixCheckoutOf(key)
	if err != nil {
		return "", err
	}
	ref := of.PR.PRRef
	target, err := prPushTarget(ctx, ref)
	if err != nil {
		return "", err
	}
	if target.State != "OPEN" {
		return "", fmt.Errorf("PR #%d is %s", ref.Number, strings.ToLower(target.State))
	}
	tip, err := triage.GitCtx(ctx, co.clone, "rev-parse", "refs/heads/"+p.Branch)
	if err != nil {
		return "", err
	}
	tip = strings.TrimSpace(tip)
	if tip == target.HeadOid || isAncestor(co.clone, tip, target.HeadOid) {
		return "", errors.New("the PR has every fix already")
	}
	if !isAncestor(co.clone, target.HeadOid, tip) {
		return "", fmt.Errorf("PR #%d's branch is at %s, which its fixes aren't on top of: triage the PR again, and the next fix moves them onto it", ref.Number, shortOid(target.HeadOid))
	}
	if target.HeadRef == "" {
		target.HeadRef = p.HeadRef
	}
	cmd := proc.CommandContext(ctx, "git", triage.GitArgs("push", "--porcelain", target.remote(ref), tip+":refs/heads/"+target.HeadRef)...)
	cmd.Dir = co.clone
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git push to %s: %w: %s", target.HeadRef, err, strings.TrimSpace(string(out)))
	}
	err = co.update(func(st *checkoutState) error {
		if q := st.PRs[strconv.Itoa(ref.Number)]; q != nil {
			q.PushedAs, q.Touched = tip, time.Now()
		}
		return nil
	})
	if err != nil {
		return tip, fmt.Errorf("pushed, but saving it failed: %w", err)
	}
	for _, e := range p.Entries {
		if isAncestor(co.clone, e.Commit, tip) {
			_, _ = t.updateResult(e.Key, func(r *PRResult) { r.FixPushed, r.FixPushedAs = e.Commit, tip })
		}
	}
	return tip, nil
}

// dropFix takes one fix's commit off key's PR's branch. A later fix that
// changed its lines makes that a conflict, which leaves the branch as it
// was and says to drop that fix first.
func (t *triager) dropFix(ctx context.Context, key, commit string) error {
	of, err := t.Load(key)
	if err != nil {
		return err
	}
	if of.PR == nil || of.PR.LocalPath != "" {
		return errors.New("a local review's fixes are not on a PR branch")
	}
	var gone string
	err = t.checkouts.idle(of.PR.PRRef, func(co *repoCheckout) error {
		co.tree.Lock()
		defer co.tree.Unlock()
		return co.update(func(st *checkoutState) error {
			p := st.PRs[strconv.Itoa(of.PR.Number)]
			if p == nil {
				return errors.New("the PR has no fixes")
			}
			// A superseded fix is off the branch already: it leaves the list.
			if p.superseded(commit) {
				var keep []fixEntry
				for _, e := range p.Superseded {
					if e.Commit != commit {
						keep = append(keep, e)
					} else {
						gone = e.Key
					}
				}
				p.Superseded, p.Touched = keep, time.Now()
				return nil
			}
			if isAncestor(co.dir, commit, p.PushedAs) {
				return errors.New("this fix is pushed: revert it on the PR instead")
			}
			if !isAncestor(co.dir, p.Base, commit) || commit == p.Base {
				return errors.New("not a fix on the PR's branch")
			}
			if st.Branch != p.Branch {
				if dirty, err := co.dirty(); err != nil {
					return err
				} else if dirty && st.Branch != "" {
					if err := co.saveLeftovers(ctx); err != nil {
						return err
					}
				}
				if _, err := co.git(ctx, "checkout", "-q", "-f", p.Branch); err != nil {
					return err
				}
				st.Branch = p.Branch
			}
			later, err := co.git(ctx, "rev-list", "--reverse", commit+"..HEAD")
			if err != nil {
				return err
			}
			if _, err := co.git(ctx, "rebase", "-q", "--empty=keep", "--onto", commit+"^", commit); err != nil {
				_, _ = co.git(ctx, "rebase", "--abort")
				return fmt.Errorf("a later fix changed this one's lines: drop that fix first (%v)", err)
			}
			now, err := co.git(ctx, "rev-list", "--reverse", commit+"^..HEAD")
			if err != nil {
				return err
			}
			old, moved := strings.Fields(later), strings.Fields(now)
			to := map[string]string{}
			if len(old) == len(moved) {
				for i := range old {
					to[old[i]] = moved[i]
				}
			}
			var entries []fixEntry
			for _, e := range p.Entries {
				switch {
				case e.Commit == commit:
					gone = e.Key
				case to[e.Commit] != "":
					e.Commit = to[e.Commit]
					entries = append(entries, e)
				case isAncestor(co.dir, e.Commit, "HEAD"):
					entries = append(entries, e)
				}
			}
			p.Entries, p.Touched = entries, time.Now()
			return triage.StripAgentFiles(co.dir)
		})
	})
	if err == nil && gone != "" {
		t.removeResult(gone)
	}
	return err
}

// migrateFixes moves each PR fix made before the repository checkouts,
// in a worktree of its own and not pushed, onto its PR's branch, as one
// commit, and removes the worktree.
func (t *triager) migrateFixes(ctx context.Context) {
	paths, _ := filepath.Glob(filepath.Join(t.results, "fix__*.json"))
	newest := map[string]*PRResult{}
	all := map[string][]string{}
	for _, p := range paths {
		k := strings.TrimSuffix(filepath.Base(p), ".json")
		r, err := t.Load(k)
		if err != nil || r.LocalFixDir == "" || r.PR == nil || r.PR.LocalPath != "" || t.checkouts.isRepoCheckout(r.LocalFixDir) {
			continue
		}
		if r.LocalFixLocation != "" && r.LocalFixLocation != "worktree" {
			continue
		}
		id := r.LocalFixDir + "\x00" + r.LocalFixBranch
		all[id] = append(all[id], k)
		if n := newest[id]; n == nil || r.CreatedAt.After(n.CreatedAt) {
			newest[id] = r
		}
	}
	for id, r := range newest {
		if err := t.migrateFix(ctx, r, all[id]); err != nil {
			activity.Printf(ctx, "move the fix in %s: %v", r.LocalFixDir, err)
			fmt.Fprintf(os.Stderr, "move the fix in %s to the repository checkout: %v\n", r.LocalFixDir, err)
		}
	}
}

// reviewAt is the newest review of ref at head, not a fix's.
func (t *triager) reviewAt(ref triage.PRRef, head string) *PRResult {
	paths, _ := filepath.Glob(filepath.Join(t.results, ref.FileKey()+"__*.json"))
	var best *PRResult
	for _, p := range paths {
		r, err := t.Load(strings.TrimSuffix(filepath.Base(p), ".json"))
		if err != nil || r.PR == nil || r.PR.HeadOid != head || r.LocalFixDir != "" || r.SummaryLang != "" {
			continue
		}
		if best == nil || r.CreatedAt.After(best.CreatedAt) {
			best = r
		}
	}
	return best
}

func (t *triager) migrateFix(ctx context.Context, r *PRResult, keys []string) error {
	pf, err := inspectFix(ctx, r, false)
	if err != nil {
		return err
	}
	if pf.State != "uncommitted" && pf.State != "committed" {
		return nil // nothing to move; discarding it is the user's call
	}
	base := fixBaseOf(r)
	out, err := triage.GitCtx(ctx, r.LocalFixDir, "diff", "--no-color", "--no-ext-diff", "--binary", "-M", base)
	if err != nil {
		return err
	}
	untracked, err := triage.GitCtx(ctx, r.LocalFixDir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	if strings.TrimSpace(untracked) != "" {
		// Into the diff with the rest: intent-to-add, then diffed.
		for _, p := range strings.Split(untracked, "\x00") {
			if p != "" {
				if _, err := triage.GitCtx(ctx, r.LocalFixDir, "add", "-N", "--", p); err != nil {
					return err
				}
			}
		}
		if out, err = triage.GitCtx(ctx, r.LocalFixDir, "diff", "--no-color", "--no-ext-diff", "--binary", "-M", base); err != nil {
			return err
		}
	}
	files, err := triage.ParseDiff(out)
	if err != nil {
		return err
	}
	set := map[string]bool{}
	var paths []string
	for _, f := range files {
		for _, p := range []string{f.Path, f.OldPath} {
			if p != "" && !set[p] {
				set[p] = true
				paths = append(paths, p)
			}
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil
	}
	pr := *r.PR
	pr.HeadOid = base
	o := t.options(jobOptions{})
	job := "move-" + filepath.Base(r.LocalFixDir)
	co, release, err := t.checkouts.acquire(ctx, o, &pr, job, set, nil, func(string) {})
	if err != nil {
		return err
	}
	defer release()
	patch := filepath.Join(os.TempDir(), "pr-manager-"+job+".patch")
	if err := os.WriteFile(patch, []byte(out), 0o600); err != nil {
		return err
	}
	defer os.Remove(patch)
	co.tree.Lock()
	_, err = co.git(ctx, "apply", "--whitespace=nowarn", patch)
	co.tree.Unlock()
	if err != nil {
		return fmt.Errorf("apply its changes: %w", err)
	}
	fixed := r.FixedIssues
	if len(fixed) == 0 {
		// Made before fixes recorded it: what the review of its base had
		// that the fix's result no longer does.
		if review := t.reviewAt(r.PR.PRRef, base); review != nil {
			fixed = vanished(review, r)
		}
	}
	commit, err := co.commitFix(ctx, pr.Number, paths, fixEntry{Key: r.Key, Files: paths, Fixed: fixed, Rounds: r.FixRounds, CreatedAt: r.CreatedAt})
	if err != nil {
		co.restoreFiles(context.Background(), paths)
		return err
	}
	p, err := co.pr(pr.Number)
	if err != nil || p == nil {
		return err
	}
	if _, err := t.updateResult(r.Key, func(x *PRResult) {
		x.LocalFixDir, x.LocalFixBranch, x.LocalFixLocation, x.FixCommit, x.FixBase = co.dir, p.Branch, "repo", commit, p.Base
	}); err != nil {
		return err
	}
	for _, k := range keys {
		if k != r.Key {
			t.removeResult(k) // the fixes it continued, all in its commit
		}
	}
	repo := t.fetcher.RepoDir(r.PR.PRRef)
	removeFixWorktree(repo, r.LocalFixDir, r.LocalFixBranch, r.LocalFixBranch != "" && r.LocalFixBranch != p.Branch)
	// The old worktree had the PR's branch name; it is free now.
	branch := p.Branch
	if name := co.newBranch(ctx, &pr); name != p.Branch && pmBranch.MatchString(p.Branch) {
		co.tree.Lock()
		err := co.update(func(st *checkoutState) error {
			if _, err := co.git(ctx, "branch", "-m", p.Branch, name); err != nil {
				return err
			}
			if q := st.PRs[strconv.Itoa(pr.Number)]; q != nil {
				q.Branch = name
			}
			if st.Branch == p.Branch {
				st.Branch = name
			}
			return nil
		})
		co.tree.Unlock()
		if err == nil {
			branch = name
			_, _ = t.updateResult(r.Key, func(x *PRResult) { x.LocalFixBranch = name })
		}
	}
	fmt.Fprintf(os.Stderr, "moved the fix in %s onto %s in %s\n", r.LocalFixDir, branch, co.dir)
	return nil
}
