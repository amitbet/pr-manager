package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/amitbet/pr-manager/triage"
)

// revList is what the picker offers in a checkout: its branches, newest
// first, and its recent commits on any of them.
type revList struct {
	Dir      string      `json:"dir"`
	Current  string      `json:"current,omitempty"` // checked-out branch
	BaseRef  string      `json:"base_ref,omitempty"`
	Branches []revBranch `json:"branches"`
	Commits  []revCommit `json:"commits"`
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
		out.BaseRef = baseBranch
	}
	refs, err := triage.Git(dir, "for-each-ref", "--sort=-committerdate", "--count="+strconv.Itoa(revBranchLimit),
		"--format=%(refname)%00%(objectname)%00%(committerdate:iso-strict)%00%(authorname)%00%(subject)", "refs/heads", "refs/remotes/origin")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(refs), "\n") {
		f := strings.Split(line, "\x00")
		if len(f) != 5 || f[0] == "refs/remotes/origin/HEAD" {
			continue
		}
		b := revBranch{Oid: f[1], Date: f[2], Author: f[3], Title: f[4], Ahead: -1}
		if name, ok := strings.CutPrefix(f[0], "refs/heads/"); ok {
			b.Name = name
		} else {
			b.Name, b.Remote = strings.TrimPrefix(f[0], "refs/remotes/"), true
		}
		if baseErr == nil && len(out.Branches) < revAheadLimit {
			if n, err := triage.Git(dir, "rev-list", "--count", baseRef+".."+b.Oid); err == nil {
				b.Ahead, _ = strconv.Atoi(strings.TrimSpace(n))
			}
		}
		out.Branches = append(out.Branches, b)
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
