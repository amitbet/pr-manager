package main

// Stacked PRs: a branch built on another branch whose PR isn't merged
// yet, PR'd into that branch rather than into the default one, so that
// its review and its description cover only its own commits.
//
// The branch a local branch is stacked on is kept in git's
// branch.<name>.gh-merge-base, the base gh pr create defaults to. Every
// inspection of the checkout reads it from there, so a triage, a fix and
// a publish of the same branch agree on its diff. syncStack sets it from
// the base of the branch's PR when it has one; setStackBase sets it when
// the user stacks a branch that has none yet. A branch without it is
// diffed from the default branch, as every branch was before.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/triage"
)

func stackConfig(branch string) string { return "branch." + branch + ".gh-merge-base" }

// stackBase is the branch the checked-out branch is stacked on: the one
// its gh-merge-base names, when that isn't the default branch and origin
// has it. Empty otherwise, a stale setting included, and when the base
// is overridden (the fix command's -base).
func stackBase(dir, branch, defaultBranch string) string {
	if baseOverride != "" {
		return ""
	}
	out, err := triage.Git(dir, "config", "--get", stackConfig(branch))
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(out)
	if name == "" || name == defaultBranch || name == branch || strings.HasPrefix(name, "-") {
		return ""
	}
	if _, err := triage.Git(dir, "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+name); err != nil {
		return ""
	}
	return name
}

// setStackBase stacks branch on base, fetching base from origin first;
// an empty base unstacks it, back onto the default branch.
func setStackBase(dir, branch, base string) error {
	key := stackConfig(branch)
	if base == "" {
		_, err := triage.Git(dir, "config", "--unset-all", key)
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 5 {
			return nil // it wasn't set
		}
		return err
	}
	if strings.HasPrefix(base, "-") {
		return fmt.Errorf("bad branch %q", base)
	}
	if _, err := triage.Git(dir, "fetch", "--quiet", "--no-tags", "origin", fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", base, base)); err != nil {
		return err
	}
	_, err := triage.Git(dir, "config", key, base)
	return err
}

// openPR is an open PR on a checkout's origin, as gh pr list has it.
type openPR struct {
	URL               string `json:"url"`
	Number            int    `json:"number"`
	Title             string `json:"title"`
	HeadRefName       string `json:"headRefName"`
	HeadRefOid        string `json:"headRefOid"`
	BaseRefName       string `json:"baseRefName"`
	IsCrossRepository bool   `json:"isCrossRepository"`
}

var errNoPRList = errors.New("open PRs can't be listed for this checkout")

// listOpenPRs lists the open PRs on p's origin that match gh pr list
// args. It fails when origin isn't on GitHub or gh is missing. A var
// for tests.
var listOpenPRs = func(ctx context.Context, p *triage.PRInfo, args ...string) ([]openPR, error) {
	if p.Owner == "local" || strings.Contains(strings.ToLower(p.HostName()), "gitlab") {
		return nil, errNoPRList
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, errNoPRList
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := proc.CommandContext(ctx, "gh", append([]string{"pr", "list", "-R", p.RepoArg(), "--state", "open",
		"--json", "url,number,title,headRefName,headRefOid,baseRefName,isCrossRepository"}, args...)...)
	cmd.Dir = p.LocalPath
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var prs []openPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, err
	}
	// A fork's branch of the same name isn't this checkout's.
	return slices.DeleteFunc(prs, func(o openPR) bool { return o.IsCrossRepository }), nil
}

// syncStack looks up the checked-out branch's open PR, whatever its
// base, and what the branch is or could be stacked on, into s. When the
// PR's base puts the branch on a stack, or takes it off one, the base is
// recorded and the checkout inspected again, from it. ok is false when
// the PRs couldn't be listed; s is then as it was inspected.
func syncStack(ctx context.Context, s *localSnapshot) (_ *localSnapshot, ok bool, _ error) {
	p := s.info
	if baseOverride != "" {
		p.URL = openPRFor(ctx, p, p.HeadRef)
		return s, p.URL != "", nil
	}
	defaultBranch := p.BaseRef
	if p.StackedOn != "" {
		defaultBranch = p.DefaultBase
	}
	if p.HeadRef == defaultBranch {
		return s, false, nil
	}
	var own, all []openPR
	var ownErr, allErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); own, ownErr = listOpenPRs(ctx, p, "--head", p.HeadRef, "--limit", "5") }()
	go func() { defer wg.Done(); all, allErr = listOpenPRs(ctx, p, "--limit", "100") }()
	wg.Wait()
	if ownErr != nil {
		return s, false, nil
	}
	var mine *openPR
	if len(own) > 0 {
		mine = &own[0]
		p.URL = mine.URL
		want := mine.BaseRefName
		if want == defaultBranch {
			want = ""
		}
		if want != p.StackedOn {
			if err := setStackBase(p.LocalPath, p.HeadRef, want); err != nil {
				return nil, false, fmt.Errorf("diff from %s, the base of %s: %w", mine.BaseRefName, mine.URL, err)
			}
			next, err := inspectLocal(ctx, p.LocalPath)
			if err != nil {
				return nil, false, err
			}
			if next.info.HeadRef != p.HeadRef {
				return nil, false, errors.New("the checkout switched branches during triage; triage it again")
			}
			next.info.URL = mine.URL
			s, p = next, next.info
		}
	}
	switch {
	case p.StackedOn != "":
		p.StackPR = stackPR(ctx, p, all, allErr)
	case mine == nil && allErr == nil:
		p.StackCandidates = stackCandidates(p, all)
	}
	return s, true, nil
}

// stackPR is the open PR of the branch p is stacked on: found in all, or
// looked up when all couldn't be listed or doesn't reach it.
func stackPR(ctx context.Context, p *triage.PRInfo, all []openPR, allErr error) string {
	if allErr == nil {
		for _, o := range all {
			if o.HeadRefName == p.StackedOn {
				return o.URL
			}
		}
		if len(all) < 100 {
			return ""
		}
	}
	if prs, err := listOpenPRs(ctx, p, "--head", p.StackedOn, "--limit", "1"); err == nil && len(prs) > 0 {
		return prs[0].URL
	}
	return ""
}

// stackCandidates are the PRs among prs that p's branch can be stacked
// on: those whose head commit is one of the branch's own commits, not on
// the default branch and not its head, so their PR hasn't merged. The
// nearest, whose head is the branch's newest commit among them, comes
// first.
func stackCandidates(p *triage.PRInfo, prs []openPR) []triage.StackCandidate {
	if p.Ahead == 0 || len(prs) == 0 {
		return nil
	}
	out, err := triage.Git(p.LocalPath, "rev-list", "origin/"+p.BaseRef+".."+p.HeadOid)
	if err != nil {
		return nil
	}
	pos := map[string]int{} // newest first
	for i, c := range strings.Fields(out) {
		pos[c] = i
	}
	type cand struct {
		c   triage.StackCandidate
		pos int
	}
	var cs []cand
	for _, o := range prs {
		i, ok := pos[o.HeadRefOid]
		if !ok || o.HeadRefName == p.HeadRef || o.HeadRefName == p.BaseRef || o.HeadRefOid == p.HeadOid {
			continue
		}
		cs = append(cs, cand{triage.StackCandidate{Branch: o.HeadRefName, URL: o.URL, Number: o.Number, Title: o.Title}, i})
	}
	sort.SliceStable(cs, func(a, b int) bool { return cs[a].pos < cs[b].pos })
	var res []triage.StackCandidate
	for _, c := range cs {
		if len(res) == 3 {
			break
		}
		res = append(res, c.c)
	}
	return res
}

// sameStack says whether a and b agree on the PR lookups syncStack makes.
func sameStack(a, b *triage.PRInfo) bool {
	return a.URL == b.URL && a.StackPR == b.StackPR && slices.Equal(a.StackCandidates, b.StackCandidates)
}

// stackRoutes serves POST /api/local/{key}/stack {base}, which stacks a
// local branch without a PR on one of the PRs it is built on, or with an
// empty base puts it back onto the default branch. The caller triages the
// returned path again, from the new base.
func (t *triager) stackRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/local/{key}/stack", func(w http.ResponseWriter, r *http.Request) {
		res, err := t.Load(r.PathValue("key"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		var req struct {
			Base string `json:"base"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		p := res.PR
		if p.LocalPath == "" || p.Rev != "" {
			writeErr(w, 400, errors.New("this is not a local branch result"))
			return
		}
		if p.URL != "" {
			writeErr(w, 400, errors.New("the branch has a PR already; its base decides what the branch is diffed from"))
			return
		}
		if req.Base != "" && !slices.ContainsFunc(p.StackCandidates, func(c triage.StackCandidate) bool { return c.Branch == req.Base }) {
			writeErr(w, 400, fmt.Errorf("%s is not a branch this one can be stacked on", req.Base))
			return
		}
		if cur, err := triage.Git(p.LocalPath, "symbolic-ref", "--quiet", "--short", "HEAD"); err != nil || strings.TrimSpace(cur) != p.HeadRef {
			writeErr(w, 400, errors.New("the checkout is on another branch now; triage it again"))
			return
		}
		if err := setStackBase(p.LocalPath, p.HeadRef, req.Base); err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]string{"path": localSrcOf(p)})
	})
}
