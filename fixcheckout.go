package main

// PR fixes go into one checkout per repository: a worktree of the cached
// clone, switched to a branch of the PR being fixed. Each fix of a PR is a
// commit on that branch, on top of the ones before it, so they are pushed
// together and a fix that rewrites another's lines shows (see touches).
//
// Fixes of one PR run side by side when they change different files: each
// claims the files its issues are in, and a fix whose files another fix
// holds waits for it, as does a fix of another PR, which needs the
// checkout on its own branch. The fixer calls run in parallel; applying a
// patch, snapshotting the tree for a check and committing take turns
// (lockTree).
//
// A PR's fixes are done when it is completed, by hand or after
// autoCompleteAfter without a fix: its branch and fix results go, and the
// checkout too once no PR in it has any.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

// autoCompleteAfter is how long a PR's fixes wait for another fix, a push
// or a completion before they are completed on their own.
const autoCompleteAfter = 10 * 24 * time.Hour

// checkoutState is what a repository's fix checkout holds, saved beside it.
type checkoutState struct {
	Repo   triage.PRRef        `json:"repo"` // Number unset
	Branch string              `json:"branch,omitempty"`
	PRs    map[string]*prFixes `json:"prs"` // by PR number
}

// prFixes is one PR's branch in the checkout.
type prFixes struct {
	Number int    `json:"number"`
	Branch string `json:"branch"`
	// Base is the PR head the branch's fixes start from: every commit
	// after it is a fix (see fixEntry) not on the PR yet, or pushed as
	// PushedAs.
	Base     string     `json:"base"`
	HeadRef  string     `json:"head_ref"` // the PR's branch on GitHub
	PushedAs string     `json:"pushed_as,omitempty"`
	Entries  []fixEntry `json:"entries,omitempty"`
	// Touched is the last fix, push or switch to the branch.
	Touched time.Time `json:"touched"`
}

// fixEntry is one fix's commit on its PR's branch.
type fixEntry struct {
	Key       string       `json:"key"` // the fix's result
	Commit    string       `json:"commit"`
	Files     []string     `json:"files"`
	Fixed     []fixedIssue `json:"fixed,omitempty"`
	Rounds    int          `json:"rounds"`
	CreatedAt time.Time    `json:"created_at"`
}

// fixClaim is a fix running, or waiting to, in a checkout.
type fixClaim struct {
	Job     string   `json:"job"`
	PR      int      `json:"pr"`
	Files   []string `json:"files"`
	Targets []string `json:"targets"` // issue and thread scopes
	// Waiting is what it waits for, "" once it runs.
	Waiting string `json:"waiting,omitempty"`
	files   map[string]bool
}

type repoCheckout struct {
	repo      triage.PRRef
	clone     string // the cached clone it is a worktree of
	dir       string
	statePath string
	tree      sync.Mutex // see lockTree
	stMu      sync.Mutex // the state file
	// Guarded by fixCheckouts.mu.
	claims    map[string]*fixClaim
	preparing bool
}

// fixCheckouts are the repositories' fix checkouts.
type fixCheckouts struct {
	root    string
	fetcher *triage.PRFetcher
	mu      sync.Mutex
	by      map[string]*repoCheckout
	changed chan struct{} // closed, and replaced, when a claim goes
}

func newFixCheckouts(cache string, fetcher *triage.PRFetcher) *fixCheckouts {
	root, _ := filepath.Abs(filepath.Join(cache, "fixes", "repos"))
	return &fixCheckouts{root: root, fetcher: fetcher, by: map[string]*repoCheckout{}, changed: make(chan struct{})}
}

func repoOnly(ref triage.PRRef) triage.PRRef {
	ref.Number = 0
	return ref
}

// get is ref's repository's checkout; m.mu is held.
func (m *fixCheckouts) get(ref triage.PRRef) *repoCheckout {
	ref = repoOnly(ref)
	k := ref.RepoArg()
	if co := m.by[k]; co != nil {
		return co
	}
	parts := []string{m.root, ref.Owner, ref.Repo}
	if ref.Host != "" {
		parts = []string{m.root, ref.Host, ref.Owner, ref.Repo}
	}
	dir := filepath.Join(parts...)
	co := &repoCheckout{repo: ref, clone: m.fetcher.RepoDir(ref), dir: dir, statePath: dir + ".json", claims: map[string]*fixClaim{}}
	m.by[k] = co
	return co
}

// isRepoCheckout is whether dir is a repository's fix checkout, rather than
// the worktree of one fix that fixes made before this.
func (m *fixCheckouts) isRepoCheckout(dir string) bool {
	abs, err := filepath.Abs(dir)
	return err == nil && strings.HasPrefix(abs, m.root+string(os.PathSeparator))
}

func (m *fixCheckouts) broadcast() {
	close(m.changed)
	m.changed = make(chan struct{})
}

func (co *repoCheckout) load() (*checkoutState, error) {
	st := &checkoutState{Repo: co.repo, PRs: map[string]*prFixes{}}
	b, err := os.ReadFile(co.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, err
	}
	if st.PRs == nil {
		st.PRs = map[string]*prFixes{}
	}
	return st, nil
}

func (co *repoCheckout) save(st *checkoutState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(co.statePath), 0o755); err != nil {
		return err
	}
	tmp := co.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, co.statePath)
}

// update loads the state, applies set and saves it, under stMu.
func (co *repoCheckout) update(set func(*checkoutState) error) error {
	co.stMu.Lock()
	defer co.stMu.Unlock()
	st, err := co.load()
	if err != nil {
		return err
	}
	if err := set(st); err != nil {
		return err
	}
	return co.save(st)
}

// pr is the state of PR n, or nil.
func (co *repoCheckout) pr(n int) (*prFixes, error) {
	co.stMu.Lock()
	defer co.stMu.Unlock()
	st, err := co.load()
	if err != nil {
		return nil, err
	}
	return st.PRs[strconv.Itoa(n)], nil
}

// blocker is why a fix of pr changing files can't start in co now, or "".
// needPrep: the checkout has to switch to pr's branch or move it to pr's
// head first, which only a checkout nobody works in can.
func (co *repoCheckout) blocker(job string, pr int, files map[string]bool, needPrep bool) string {
	if co.preparing {
		return "the checkout is switching branches"
	}
	for _, c := range co.claims {
		if c.Job == job || c.Waiting != "" {
			continue
		}
		if c.PR != pr {
			return fmt.Sprintf("a fix of #%d is running in this repository's checkout", c.PR)
		}
		if needPrep {
			return "another fix of this PR, of an older head, is running"
		}
		for f := range files {
			if c.files[f] {
				return fmt.Sprintf("another fix is changing %s", f)
			}
		}
	}
	return ""
}

// needsPrep is whether co has to switch to pr's branch, or move it to pr's
// head, before a fix of it runs.
func (co *repoCheckout) needsPrep(pr *triage.PRInfo) bool {
	co.stMu.Lock()
	defer co.stMu.Unlock()
	st, err := co.load()
	if err != nil {
		return true
	}
	p := st.PRs[strconv.Itoa(pr.Number)]
	if _, err := os.Stat(filepath.Join(co.dir, ".git")); err != nil {
		return true
	}
	return p == nil || st.Branch != p.Branch || p.Base != pr.HeadOid
}

// acquire waits until a fix of pr changing files can run in its
// repository's checkout, and gets the checkout ready for it. wait is told
// what it waits for. release ends the claim.
func (m *fixCheckouts) acquire(ctx context.Context, o options, pr *triage.PRInfo, job string, files map[string]bool, targets []string, wait func(string)) (*repoCheckout, func(), error) {
	claim := &fixClaim{Job: job, PR: pr.Number, Targets: targets, files: files, Waiting: "starting"}
	for f := range files {
		claim.Files = append(claim.Files, f)
	}
	sort.Strings(claim.Files)
	m.mu.Lock()
	co := m.get(pr.PRRef)
	co.claims[job] = claim
	m.mu.Unlock()
	release := func() {
		m.mu.Lock()
		delete(co.claims, job)
		m.broadcast()
		m.mu.Unlock()
	}
	said := ""
	for {
		needPrep := co.needsPrep(pr)
		m.mu.Lock()
		claim.Waiting = co.blocker(job, pr.Number, files, needPrep)
		why := claim.Waiting
		if why == "" {
			co.preparing = needPrep
		}
		ch := m.changed
		m.mu.Unlock()
		if why == "" {
			if !needPrep {
				return co, release, nil
			}
			err := co.prepare(ctx, o, pr)
			m.mu.Lock()
			co.preparing = false
			m.broadcast()
			m.mu.Unlock()
			if err != nil {
				release()
				return nil, nil, err
			}
			return co, release, nil
		}
		if why != said {
			wait(why)
			said = why
		}
		select {
		case <-ctx.Done():
			release()
			return nil, nil, ctx.Err()
		case <-ch:
		}
	}
}

// idle runs fn with no fix running or waiting in ref's repository's
// checkout, and none starting until it returns.
func (m *fixCheckouts) idle(ref triage.PRRef, fn func(co *repoCheckout) error) error {
	m.mu.Lock()
	co := m.get(ref)
	if len(co.claims) > 0 || co.preparing {
		m.mu.Unlock()
		return errFixBusy
	}
	co.preparing = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		co.preparing = false
		m.broadcast()
		m.mu.Unlock()
	}()
	return fn(co)
}

// claimsOf are the fixes of PR n running or waiting in its checkout.
func (m *fixCheckouts) claimsOf(ref triage.PRRef) []fixClaim {
	m.mu.Lock()
	defer m.mu.Unlock()
	co := m.get(ref)
	var out []fixClaim
	for _, c := range co.claims {
		if c.PR == ref.Number {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Job < out[j].Job })
	return out
}

func (co *repoCheckout) git(ctx context.Context, args ...string) (string, error) {
	out, err := triage.GitCtx(ctx, co.dir, args...)
	return strings.TrimSpace(out), err
}

func (co *repoCheckout) dirty() (bool, error) { return hasUncommitted(co.dir) }

var pmBranch = regexp.MustCompile(`^pr-manager/`)

// prepare puts the checkout on pr's branch, making both when they don't
// exist, with the branch's fixes on top of pr's head: the PR may have
// moved on since, by a push of them or anyone else's. A fix left
// uncommitted by a run that died is committed on its branch first, so
// switching never loses it.
func (co *repoCheckout) prepare(ctx context.Context, o options, pr *triage.PRInfo) error {
	co.tree.Lock()
	defer co.tree.Unlock()
	if _, err := os.Stat(filepath.Join(co.dir, ".git")); err != nil {
		if _, err := os.Stat(co.dir); err == nil {
			if err := os.RemoveAll(co.dir); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(co.dir), 0o755); err != nil {
			return err
		}
		_, _ = triage.GitCtx(ctx, co.clone, "worktree", "prune")
		if _, err := triage.GitCtx(ctx, co.clone, "worktree", "add", "--detach", co.dir, pr.HeadOid); err != nil {
			return fmt.Errorf("make the fix checkout: %w", err)
		}
	}
	return co.update(func(st *checkoutState) error {
		if dirty, err := co.dirty(); err != nil {
			return err
		} else if dirty {
			if st.Branch == "" {
				if _, err := co.git(ctx, "reset", "-q", "--hard"); err != nil {
					return err
				}
				if _, err := co.git(ctx, "clean", "-q", "-fd"); err != nil {
					return err
				}
			} else if err := co.saveLeftovers(ctx); err != nil {
				return err
			}
		}
		k := strconv.Itoa(pr.Number)
		p := st.PRs[k]
		if p == nil {
			p = &prFixes{Number: pr.Number, Branch: co.newBranch(ctx, pr), Base: pr.HeadOid, HeadRef: pr.HeadRef}
			if _, err := co.git(ctx, "checkout", "-q", "-B", p.Branch, pr.HeadOid); err != nil {
				return err
			}
			st.PRs[k] = p
		} else {
			if _, err := co.git(ctx, "checkout", "-q", p.Branch); err != nil {
				return err
			}
			if p.Base != pr.HeadOid {
				if err := co.rebase(ctx, p, pr.HeadOid); err != nil {
					return err
				}
			}
		}
		if pr.HeadRef != "" {
			p.HeadRef = pr.HeadRef
		}
		st.Branch, p.Touched = p.Branch, time.Now()
		return triage.StripAgentFiles(co.dir)
	})
}

// saveLeftovers commits whatever a fix left uncommitted, on the branch
// it is on.
func (co *repoCheckout) saveLeftovers(ctx context.Context) error {
	if _, err := co.git(ctx, "add", "-A"); err != nil {
		return err
	}
	_, err := co.git(ctx, "commit", "-q", "--no-verify", "-m", "Save changes a fix left uncommitted")
	return err
}

// newBranch names pr's branch: the PR's own branch name when the clone
// has no branch by it, else pr-manager/pr-N.
func (co *repoCheckout) newBranch(ctx context.Context, pr *triage.PRInfo) string {
	name := strings.TrimSpace(pr.HeadRef)
	if name != "" && !pmBranch.MatchString(name) {
		if _, err := co.git(ctx, "check-ref-format", "--branch", name); err == nil {
			if _, err := co.git(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+name); err != nil {
				return name
			}
		}
	}
	return fmt.Sprintf("pr-manager/pr-%d", pr.Number)
}

// rebase moves p's branch, checked out, onto head: the fixes the PR
// doesn't have yet go on top of it, the others are dropped. A review of a
// head older than the fixes' is refused: fixing it would undo them.
func (co *repoCheckout) rebase(ctx context.Context, p *prFixes, head string) error {
	if _, err := co.git(ctx, "merge-base", "--is-ancestor", head, p.Base); err == nil {
		return fmt.Errorf("this review is of an older head of the PR than its fixes (%s): triage the PR again", shortOid(p.Base))
	}
	tip, err := co.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	// The branch's commits the PR has no copy of, oldest first.
	raw, err := co.git(ctx, "rev-list", "--reverse", "--cherry-pick", "--right-only", "--no-merges", head+"..."+tip)
	if err != nil {
		return err
	}
	var keep []string
	for _, c := range strings.Fields(raw) {
		keep = append(keep, c)
	}
	if _, err := co.git(ctx, "reset", "-q", "--hard", head); err != nil {
		return err
	}
	moved := map[string]string{}
	for _, c := range keep {
		if _, err := co.git(ctx, "cherry-pick", "--allow-empty", c); err != nil {
			_, _ = co.git(ctx, "cherry-pick", "--abort")
			_, _ = co.git(ctx, "reset", "-q", "--hard", tip)
			return fmt.Errorf("the PR moved on to %s, and the fix in %s conflicts with it: push or discard the PR's fixes, then fix again: %w", shortOid(head), shortOid(c), err)
		}
		n, err := co.git(ctx, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		moved[c] = n
	}
	var entries []fixEntry
	for _, e := range p.Entries {
		if n, ok := moved[e.Commit]; ok {
			e.Commit = n
			entries = append(entries, e)
		}
	}
	p.Entries, p.Base, p.PushedAs = entries, head, ""
	return nil
}

// lockTree, in a fix running in a shared checkout, takes the checkout's
// tree lock and returns its unlock; elsewhere it does nothing.
type treeLockKey struct{}

func withTreeLock(ctx context.Context, l *sync.Mutex) context.Context {
	return context.WithValue(ctx, treeLockKey{}, l)
}

func lockTree(ctx context.Context) func() {
	l, _ := ctx.Value(treeLockKey{}).(*sync.Mutex)
	if l == nil {
		return func() {}
	}
	l.Lock()
	return l.Unlock
}

// commitFix commits files, as a fix of p left them, with a message from
// what it fixed, and records the fix. It returns the commit, "" when the
// fix changed none of them.
func (co *repoCheckout) commitFix(ctx context.Context, n int, files []string, e fixEntry) (string, error) {
	co.tree.Lock()
	defer co.tree.Unlock()
	paths := append([]string{"--"}, files...)
	if _, err := co.git(ctx, append([]string{"add", "-A"}, paths...)...); err != nil {
		return "", err
	}
	staged, err := co.git(ctx, append([]string{"diff", "--cached", "--name-only"}, paths...)...)
	if err != nil || staged == "" {
		return "", err
	}
	if _, err := co.git(ctx, append([]string{"commit", "-q", "--no-verify", "-m", fixCommitMessage(e)}, paths...)...); err != nil {
		return "", err
	}
	head, err := co.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	e.Commit = head
	return head, co.update(func(st *checkoutState) error {
		p := st.PRs[strconv.Itoa(n)]
		if p == nil {
			return fmt.Errorf("PR #%d has no branch in the checkout", n)
		}
		p.Entries = append(p.Entries, e)
		p.Touched = time.Now()
		return nil
	})
}

// fixCommitMessage says what a fix fixed.
func fixCommitMessage(e fixEntry) string {
	var subject string
	switch len(e.Fixed) {
	case 0:
		subject = "Fix review issues"
	case 1:
		subject = "Fix: " + e.Fixed[0].Title
	default:
		subject = fmt.Sprintf("Fix %d review issues", len(e.Fixed))
	}
	if len(e.Fixed) != 1 {
		// The files, when they fit; base names, when those do.
		names := make([]string, len(e.Files))
		for i, f := range e.Files {
			names[i] = filepath.Base(f)
		}
		for _, list := range [][]string{e.Files, names} {
			if s := subject + " in " + strings.Join(list, ", "); len([]rune(s)) <= 72 {
				subject = s
				break
			}
		}
	}
	if r := []rune(subject); len(r) > 72 {
		subject = string(r[:71]) + "…"
	}
	var b strings.Builder
	b.WriteString(subject)
	if len(e.Fixed) > 1 || (len(e.Fixed) == 1 && e.Fixed[0].URL != "") {
		b.WriteString("\n\n")
		for _, f := range e.Fixed {
			fmt.Fprintf(&b, "- %s (%s)", f.Title, f.File)
			if f.URL != "" {
				b.WriteString(" " + f.URL)
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// restoreFiles puts files back as the branch has them, after a fix that
// failed: what it left would otherwise go into the next fix's commit.
func (co *repoCheckout) restoreFiles(ctx context.Context, files []string) {
	co.tree.Lock()
	defer co.tree.Unlock()
	for _, f := range files {
		_, _ = co.git(ctx, "reset", "-q", "--", f)
		if _, err := co.git(ctx, "cat-file", "-e", "HEAD:"+f); err == nil {
			_, _ = co.git(ctx, "checkout", "-q", "HEAD", "--", f)
		} else if safeRepoPath(f) {
			_ = os.Remove(filepath.Join(co.dir, filepath.FromSlash(f)))
		}
	}
}

// fixTouch is an earlier fix whose lines a fix changed.
type fixTouch struct {
	Key    string `json:"key"`
	Commit string `json:"commit"`
	File   string `json:"file"`
	Lines  []int  `json:"lines"`
}

// touches is, for each of p's commits, the earlier fixes on the branch
// whose lines it changed or wrote next to: the old lines of its hunks, and
// the line an insertion follows, blamed in its parent.
func (co *repoCheckout) touches(ctx context.Context, p *prFixes) map[string][]fixTouch {
	keyOf := map[string]string{}
	for _, e := range p.Entries {
		keyOf[e.Commit] = e.Key
	}
	out := map[string][]fixTouch{}
	for _, e := range p.Entries {
		raw, err := co.git(ctx, "diff", "--no-color", "--no-ext-diff", "-U0", e.Commit+"^", e.Commit)
		if err != nil {
			continue
		}
		files, err := triage.ParseDiff(raw)
		if err != nil {
			continue
		}
		byKey := map[string]*fixTouch{}
		for _, f := range files {
			path := f.OldPath
			if path == "" {
				path = f.Path
			}
			for _, h := range f.Hunks {
				from, n := h.OldStart, h.OldLines
				if n == 0 {
					from, n = h.OldStart, 1 // an insertion follows its OldStart line
				}
				if from < 1 {
					continue
				}
				blame, err := co.git(ctx, "blame", "--porcelain", "-L", fmt.Sprintf("%d,+%d", from, n), e.Commit+"^", "--", path)
				if err != nil {
					continue
				}
				for _, line := range strings.Split(blame, "\n") {
					fs := strings.Fields(line)
					if len(fs) < 3 || len(fs[0]) != 40 {
						continue
					}
					k, ok := keyOf[fs[0]]
					if !ok || fs[0] == e.Commit {
						continue
					}
					t := byKey[fs[0]+path]
					if t == nil {
						t = &fixTouch{Key: k, Commit: fs[0], File: f.Path}
						byKey[fs[0]+path] = t
					}
					if ln, err := strconv.Atoi(fs[2]); err == nil {
						t.Lines = append(t.Lines, ln)
					}
				}
			}
		}
		for _, t := range byKey {
			out[e.Commit] = append(out[e.Commit], *t)
		}
		sort.Slice(out[e.Commit], func(i, j int) bool { return out[e.Commit][i].Commit < out[e.Commit][j].Commit })
	}
	return out
}

// complete ends PR n's fixes: its branch, and the results of its fixes,
// go. The checkout goes too when no other PR has a branch in it.
func (t *triager) complete(ref triage.PRRef) error {
	return t.checkouts.idle(ref, func(co *repoCheckout) error {
		co.tree.Lock()
		defer co.tree.Unlock()
		var keys []string
		empty := false
		err := co.update(func(st *checkoutState) error {
			k := strconv.Itoa(ref.Number)
			p := st.PRs[k]
			if p == nil {
				return nil
			}
			if _, err := os.Stat(filepath.Join(co.dir, ".git")); err == nil {
				if st.Branch == p.Branch {
					if _, err := co.git(context.Background(), "checkout", "-q", "--detach"); err != nil {
						return err
					}
					st.Branch = ""
				}
				_, _ = co.git(context.Background(), "branch", "-D", p.Branch)
			}
			for _, e := range p.Entries {
				keys = append(keys, e.Key)
			}
			delete(st.PRs, k)
			empty = len(st.PRs) == 0
			return nil
		})
		if err != nil {
			return err
		}
		for _, k := range keys {
			t.removeResult(k)
		}
		if empty {
			if _, err := triage.Git(co.clone, "worktree", "remove", "--force", co.dir); err != nil {
				_ = os.RemoveAll(co.dir)
			}
			_, _ = triage.Git(co.clone, "worktree", "prune")
			co.stMu.Lock()
			_ = os.Remove(co.statePath)
			co.stMu.Unlock()
		}
		return nil
	})
}

// removeResult deletes a saved result, under its lock.
func (t *triager) removeResult(key string) {
	mu := t.resultLock(key)
	mu.Lock()
	_ = os.Remove(filepath.Join(t.results, key+".json"))
	mu.Unlock()
}

// autoComplete completes every PR whose fixes saw nothing for
// autoCompleteAfter. A checkout with a fix running is left for next time.
func (t *triager) autoComplete() {
	var states []string
	_ = filepath.WalkDir(t.checkouts.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".json") {
			states = append(states, path)
		}
		return nil
	})
	for _, path := range states {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var st checkoutState
		if json.Unmarshal(b, &st) != nil {
			continue
		}
		for _, p := range st.PRs {
			if time.Since(p.Touched) < autoCompleteAfter {
				continue
			}
			ref := st.Repo
			ref.Number = p.Number
			if err := t.complete(ref); err != nil {
				fmt.Fprintf(os.Stderr, "auto-complete %s: %v\n", ref.URL(), err)
			}
		}
	}
}
