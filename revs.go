package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/triage"
)

// revList is what the picker offers in a checkout: its branches, newest
// first, and its recent commits on any of them.
type revList struct {
	Dir     string `json:"dir"`
	Current string `json:"current,omitempty"` // checked-out branch
	BaseRef string `json:"base_ref,omitempty"`
	// Base is the ref branches are counted from: origin/main.
	Base     string      `json:"base,omitempty"`
	Branches []revBranch `json:"branches"`
	Commits  []revCommit `json:"commits"`
	// Fetched says the list follows a fetch from origin; FetchError is
	// why one asked for didn't happen. The list is the local history
	// either way.
	Fetched    bool   `json:"fetched,omitempty"`
	FetchError string `json:"fetch_error,omitempty"`
	// Repo is set when the box named a repository rather than a path.
	// Remote is set when it has no checkout here and the list is the
	// forge's (revremote.go): Dir is empty, and picks are named on Web,
	// the repository's page.
	Repo   string `json:"repo,omitempty"`
	Remote bool   `json:"remote,omitempty"`
	Web    string `json:"web,omitempty"`
}

type revBranch struct {
	Name   string `json:"name"` // as typed after #: feature, origin/feature
	Remote bool   `json:"remote,omitempty"`
	Oid    string `json:"oid"`
	Date   string `json:"date"`
	Author string `json:"author"`
	Title  string `json:"title"`
	// Ahead counts its commits the default branch doesn't have; -1 when
	// it wasn't counted.
	Ahead int `json:"ahead"`
	// Upstream is origin's branch of a local branch's name, when origin
	// has one. Unpushed and Unpulled count the commits each has that the
	// other doesn't; -1 when they weren't counted.
	Upstream string `json:"upstream,omitempty"`
	Unpushed int    `json:"unpushed"`
	Unpulled int    `json:"unpulled"`
	// PR is the branch's open PR, in a list from GitHub.
	PR       string `json:"pr,omitempty"`
	PRNumber int    `json:"pr_number,omitempty"`
}

type revCommit struct {
	Oid    string `json:"oid"`
	Date   string `json:"date"`
	Author string `json:"author"`
	Title  string `json:"title"`
	Merge  bool   `json:"merge,omitempty"`
	Files  int    `json:"files"`
	Adds   int    `json:"additions"`
	Dels   int    `json:"deletions"`
}

const (
	revBranchLimit = 100
	revAheadLimit  = 40 // branches whose distance from the base is counted
	revCommitLimit = 200
)

var shortstat = regexp.MustCompile(`(\d+) (file|insertion|deletion)`)

// listRevs lists what path's checkout can review as path#rev. A #rev
// already on path is ignored.
func listRevs(path string) (*revList, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("enter a repository path")
	}
	path, _ = splitRev(path)
	dir, err := checkoutRoot(path)
	if err != nil {
		return nil, err
	}
	out := &revList{Dir: dir, Branches: []revBranch{}, Commits: []revCommit{}}
	if s, err := triage.Git(dir, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		out.Current = strings.TrimSpace(s)
	}
	baseRef, baseBranch, baseErr := localBase(dir)
	if baseErr == nil {
		out.BaseRef, out.Base = baseBranch, baseRef
	}
	refs, err := triage.Git(dir, "for-each-ref", "--sort=-committerdate", "--count="+strconv.Itoa(revBranchLimit),
		"--format=%(refname)%00%(objectname)%00%(committerdate:iso-strict)%00%(authorname)%00%(subject)", "refs/heads", "refs/remotes/origin")
	if err != nil {
		return nil, err
	}
	onOrigin := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(refs), "\n") {
		f := strings.Split(line, "\x00")
		if len(f) != 5 || f[0] == "refs/remotes/origin/HEAD" {
			continue
		}
		b := revBranch{Oid: f[1], Date: f[2], Author: f[3], Title: f[4], Ahead: -1, Unpushed: -1, Unpulled: -1}
		if name, ok := strings.CutPrefix(f[0], "refs/heads/"); ok {
			b.Name = name
		} else {
			b.Name, b.Remote = strings.TrimPrefix(f[0], "refs/remotes/"), true
			onOrigin[b.Name] = true
		}
		out.Branches = append(out.Branches, b)
	}
	// The list may hold only one side of a pair; origin's side is then
	// looked up on its own.
	for i := range out.Branches {
		b := &out.Branches[i]
		if baseErr == nil && i < revAheadLimit {
			if n, err := triage.Git(dir, "rev-list", "--count", baseRef+".."+b.Oid); err == nil {
				b.Ahead, _ = strconv.Atoi(strings.TrimSpace(n))
			}
		}
		if b.Remote {
			continue
		}
		up := "origin/" + b.Name
		if !onOrigin[up] {
			if _, err := triage.Git(dir, "show-ref", "--verify", "--quiet", "refs/remotes/"+up); err != nil {
				continue
			}
		}
		b.Upstream = up
		if i < revAheadLimit {
			if n, err := triage.Git(dir, "rev-list", "--left-right", "--count", "refs/remotes/"+up+"..."+b.Oid); err == nil {
				if f := strings.Fields(n); len(f) == 2 {
					b.Unpulled, _ = strconv.Atoi(f[0])
					b.Unpushed, _ = strconv.Atoi(f[1])
				}
			}
		}
	}
	// Records start with \x1e; --shortstat follows each one's header.
	log, err := triage.Git(dir, "log", "--branches", "--remotes=origin", "--date-order", "-n", strconv.Itoa(revCommitLimit),
		"--shortstat", "--format=%x1e%H%x00%P%x00%cI%x00%an%x00%s")
	if err != nil {
		return out, nil // a repository without commits
	}
	boundary := shallowCommits(dir)
	for _, rec := range strings.Split(log, "\x1e") {
		head, stat, _ := strings.Cut(rec, "\n")
		f := strings.Split(head, "\x00")
		if len(f) != 5 || boundary[f[0]] {
			continue // a shallow boundary's parent is missing, so it can't be reviewed
		}
		c := revCommit{Oid: f[0], Merge: len(strings.Fields(f[1])) > 1, Date: f[2], Author: f[3], Title: f[4]}
		for _, m := range shortstat.FindAllStringSubmatch(stat, -1) {
			n, _ := strconv.Atoi(m[1])
			switch m[2] {
			case "file":
				c.Files = n
			case "insertion":
				c.Adds = n
			case "deletion":
				c.Dels = n
			}
		}
		out.Commits = append(out.Commits, c)
	}
	return out, nil
}

// revFetchTimeout caps the picker's fetch: a remote that doesn't answer
// leaves the list as it was rather than holding the dialog.
const revFetchTimeout = 90 * time.Second

// fetchRevs fetches every branch of dir's origin, so the picker lists
// its new branches and commits, and drops branches deleted there. The
// refspec is spelled out so a single-branch clone gets them all too. It
// never prompts: a remote that needs a password fails and says so.
func fetchRevs(ctx context.Context, dir string) error {
	if _, err := triage.Git(dir, "remote", "get-url", "origin"); err != nil {
		return errors.New("the checkout has no origin remote to fetch from")
	}
	ctx, cancel := context.WithTimeout(ctx, revFetchTimeout)
	defer cancel()
	cmd := proc.CommandContext(ctx, "git", triage.GitArgs("fetch", "--prune", "--quiet", "origin", "+refs/heads/*:refs/remotes/origin/*")...)
	cmd.Dir = dir
	cmd.Env = noPromptEnv()
	out, err := cmd.CombinedOutput()
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return errors.New("git fetch origin timed out")
	case err != nil:
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return errors.New("git fetch origin: " + msg)
	}
	return nil
}

// shallowCommits returns the boundary commits of a shallow clone at dir,
// those whose parents weren't fetched; none for a full clone.
func shallowCommits(dir string) map[string]bool {
	path, err := triage.Git(dir, "rev-parse", "--git-path", "shallow")
	if err != nil {
		return nil
	}
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, oid := range strings.Fields(string(raw)) {
		out[oid] = true
	}
	return out
}
