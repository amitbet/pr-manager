package triage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/amitbet/pr-manager/internal/activity"
)

// PRRef names a PR. Host is a GitHub Enterprise Server (or ghe.com) host;
// empty is github.com, so refs, cache keys and draft files from before
// Enterprise support keep their names.
type PRRef struct {
	Host   string `json:"host,omitempty"`
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

func (r PRRef) Slug() string { return r.Owner + "/" + r.Repo }

// HostName is the GitHub host, github.com when Host is empty.
func (r PRRef) HostName() string {
	if r.Host == "" {
		return "github.com"
	}
	return r.Host
}

// RepoArg is the repo as gh takes it, HOST/OWNER/REPO, so gh picks the
// right login whatever GH_HOST says.
func (r PRRef) RepoArg() string { return r.HostName() + "/" + r.Slug() }

func (r PRRef) URL() string {
	return fmt.Sprintf("https://%s/%s/%s/pull/%d", r.HostName(), r.Owner, r.Repo, r.Number)
}

// FileKey names the PR in cache file names: owner__repo__N, with the host
// in front for Enterprise hosts.
func (r PRRef) FileKey() string {
	k := fmt.Sprintf("%s__%s__%d", r.Owner, r.Repo, r.Number)
	if r.Host != "" {
		k = r.Host + "__" + k
	}
	return k
}

// NormalizeHost maps github.com spellings to "" and lowercases the rest.
func NormalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	h = strings.TrimRight(h, "/")
	if h == "github.com" || h == "www.github.com" || h == "api.github.com" {
		return ""
	}
	return h
}

// DefaultHost is the host for refs that don't name one (owner/repo#N):
// GH_HOST, the variable gh reads for the same purpose.
func DefaultHost() string { return NormalizeHost(os.Getenv("GH_HOST")) }

var prURLRe = regexp.MustCompile(`(?:^|https?://|\s)([a-zA-Z0-9.-]+\.[a-zA-Z]{2,}(?::\d+)?)/([^/\s]+)/([^/\s]+)/pull/(\d+)`)
var prShortRe = regexp.MustCompile(`^(?:([a-zA-Z0-9.-]+\.[a-zA-Z]{2,}(?::\d+)?)/)?([^/\s]+)/([^/\s#]+)#(\d+)$`)

// ParsePRRef accepts a PR URL on github.com or an Enterprise host (any
// /pull/N/... suffix), owner/repo#N (on GH_HOST, else github.com) or
// host/owner/repo#N.
func ParsePRRef(s string) (PRRef, error) {
	s = strings.TrimSpace(s)
	m := prURLRe.FindStringSubmatch(s)
	if m == nil {
		if m = prShortRe.FindStringSubmatch(s); m != nil && m[1] == "" {
			m[1] = DefaultHost()
		}
	}
	if m == nil {
		return PRRef{}, fmt.Errorf("not a GitHub PR link: %q", s)
	}
	n, _ := strconv.Atoi(m[4])
	return PRRef{Host: NormalizeHost(m[1]), Owner: m[2], Repo: strings.TrimSuffix(m[3], ".git"), Number: n}, nil
}

// ParseRepo reads owner/repo or host/owner/repo (a URL works too) for the
// prs subcommand.
func ParseRepo(s string) (host, owner, repo string, err error) {
	s = strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(s), "/"), ".git")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	parts := strings.Split(s, "/")
	switch len(parts) {
	case 2:
		return DefaultHost(), parts[0], parts[1], nil
	case 3:
		return NormalizeHost(parts[0]), parts[1], parts[2], nil
	}
	return "", "", "", fmt.Errorf("repo %q: want owner/repo or host/owner/repo", s)
}

type PRInfo struct {
	PRRef
	LocalPath    string `json:"local_path,omitempty"`
	Ahead        int    `json:"ahead,omitempty"`
	Behind       int    `json:"behind,omitempty"`
	Uncommitted  bool   `json:"uncommitted,omitempty"`
	SnapshotHash string `json:"snapshot_hash,omitempty"`
	URL          string `json:"url"`
	Title        string `json:"title"`
	Author       string `json:"author"`
	State        string `json:"state"`
	HeadOid      string `json:"head_oid"`
	BaseOid      string `json:"base_oid"` // merge base actually diffed against
	BaseRef      string `json:"base_ref"`
	HeadRef      string `json:"head_ref"`
	Body         string `json:"body,omitempty"`
	// Commits are the messages of the PR's commits, oldest first.
	Commits  []string `json:"commits,omitempty"`
	Adds     int      `json:"additions"`
	Dels     int      `json:"deletions"`
	MergedAt string   `json:"merged_at,omitempty"`
	mergeOid string   // merge commit of a merged PR, for Source
}

// PRFetcher keeps blobless clones under Dir and diffs PRs locally, so
// Go files can be parsed at head and the -w pass works.
type PRFetcher struct {
	Dir string
	mu  sync.Map // repo slug -> *sync.Mutex
}

func (f *PRFetcher) lock(slug string) func() {
	m, _ := f.mu.LoadOrStore(slug, &sync.Mutex{})
	m.(*sync.Mutex).Lock()
	return m.(*sync.Mutex).Unlock
}

func run(ctx context.Context, dir, name string, args ...string) (out string, err error) {
	_, done := activity.Command(ctx, dir, name, args...)
	defer func() { done(err) }()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	b, err := cmd.Output()
	if err != nil {
		if name == "gh" {
			var stderr []byte
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = ee.Stderr
			}
			if gerr := GHError(err, stderr); gerr != nil {
				return "", gerr
			}
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(b), nil
}

var ghTooOld = regexp.MustCompile(`(?i)unknown (JSON field|flag|shorthand flag|command)`)

// GHError explains a gh failure the user can fix: gh missing, too old for
// a field or flag this tool uses, or not logged in. Other failures return
// nil so the caller keeps its own message.
func GHError(err error, output []byte) error {
	if errors.Is(err, exec.ErrNotFound) {
		return errors.New("gh (GitHub CLI) is not installed: install it from https://cli.github.com (brew install gh), then run gh auth login")
	}
	msg := strings.TrimSpace(string(output))
	if m := ghTooOld.FindString(msg); m != "" {
		line, _, _ := strings.Cut(msg[strings.Index(msg, m):], "\n")
		return fmt.Errorf("your gh is too old (%s): upgrade it (brew upgrade gh) and try again. gh said: %s", ghVersion(), line)
	}
	if strings.Contains(msg, "gh auth login") {
		return errors.New("gh is not logged in: run gh auth login (for GitHub Enterprise: gh auth login --hostname HOST)")
	}
	return nil
}

func ghVersion() string {
	out, err := exec.Command("gh", "--version").Output()
	if err != nil {
		return "unknown version"
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line
}

// RepoDir is where the PR's repo is cloned: Dir/owner/repo, under a
// directory per Enterprise host.
func (f *PRFetcher) RepoDir(ref PRRef) string {
	if ref.Host != "" {
		return filepath.Join(f.Dir, ref.Host, ref.Owner, ref.Repo)
	}
	return filepath.Join(f.Dir, ref.Owner, ref.Repo)
}

// Fetch resolves the PR with gh, fetches its commits and returns its diff.
func (f *PRFetcher) Fetch(ctx context.Context, ref PRRef) (*PRInfo, *Source, error) {
	info, err := f.Resolve(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	src, err := f.Source(ctx, info)
	return info, src, err
}

// Resolve looks the PR up with gh, without touching git, so a caller can
// check its head against saved results before the slower fetch.
func (f *PRFetcher) Resolve(ctx context.Context, ref PRRef) (*PRInfo, error) {
	raw, err := run(ctx, "", "gh", "pr", "view", strconv.Itoa(ref.Number), "-R", ref.RepoArg(), "--json",
		"url,title,author,state,headRefOid,baseRefName,headRefName,body,additions,deletions,mergedAt,mergeCommit")
	if err != nil {
		return nil, err
	}
	var v struct {
		URL         string                 `json:"url"`
		Title       string                 `json:"title"`
		Author      struct{ Login string } `json:"author"`
		State       string                 `json:"state"`
		HeadRefOid  string                 `json:"headRefOid"`
		BaseRefName string                 `json:"baseRefName"`
		HeadRefName string                 `json:"headRefName"`
		Body        string                 `json:"body"`
		Additions   int                    `json:"additions"`
		Deletions   int                    `json:"deletions"`
		MergedAt    string                 `json:"mergedAt"`
		MergeCommit *struct {
			Oid string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, err
	}
	info := &PRInfo{
		PRRef: ref, URL: v.URL, Title: v.Title, Author: v.Author.Login, State: v.State,
		HeadOid: v.HeadRefOid, BaseRef: v.BaseRefName, HeadRef: v.HeadRefName, Body: v.Body,
		Adds: v.Additions, Dels: v.Deletions, MergedAt: v.MergedAt,
	}
	if v.MergeCommit != nil {
		info.mergeOid = v.MergeCommit.Oid
	}
	return info, nil
}

// Source fetches a resolved PR's commits and returns its diff, setting
// info.BaseOid to the merge base.
func (f *PRFetcher) Source(ctx context.Context, info *PRInfo) (*Source, error) {
	ref := info.PRRef
	unlock := f.lock(ref.RepoArg())
	defer unlock()
	dir := f.RepoDir(ref)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return nil, err
		}
		if _, err := run(ctx, "", "gh", "repo", "clone", ref.RepoArg(), dir, "--", "--filter=blob:none", "--no-checkout", "--quiet"); err != nil {
			return nil, err
		}
	}

	// A merged PR's head is already in its base branch, so diffing
	// against the current base tip is empty. Diff against the fork point
	// from the base branch as it was just before the merge.
	// Open PRs diff against the base branch tip, fetched by name because
	// older gh releases don't know the baseRefOid field.
	fetch := []string{"fetch", "--quiet", "--no-tags", "origin",
		fmt.Sprintf("+refs/pull/%d/head:refs/triage/pr/%d", ref.Number, ref.Number)}
	var baseCandidate string
	if info.mergeOid != "" {
		baseCandidate = info.mergeOid + "^1"
		fetch = append(fetch, info.mergeOid)
	} else {
		baseCandidate = fmt.Sprintf("refs/triage/base/%d", ref.Number)
		fetch = append(fetch, fmt.Sprintf("+refs/heads/%s:%s", info.BaseRef, baseCandidate))
	}
	if _, err := GitCtx(ctx, dir, fetch...); err != nil {
		return nil, err
	}
	base, err := GitCtx(ctx, dir, "merge-base", baseCandidate, info.HeadOid)
	if err != nil {
		return nil, err
	}
	info.BaseOid = strings.TrimSpace(base)
	info.Commits = CommitMessages(ctx, dir, info.BaseOid, info.HeadOid)
	src, err := FromGit(dir, info.BaseOid, info.HeadOid)
	if src != nil {
		src.Title = info.Title
	}
	return src, err
}

// ListPRs returns PR refs by author (state: open|closed|merged|all). repo
// is owner/repo or host/owner/repo.
func ListPRs(repoArg, author, state string, limit int) ([]PRRef, error) {
	host, owner, repo, err := ParseRepo(repoArg)
	if err != nil {
		return nil, err
	}
	base := PRRef{Host: host, Owner: owner, Repo: repo}
	raw, err := run(context.Background(), "", "gh", "pr", "list", "-R", base.RepoArg(), "--author", author, "--state", state,
		"--limit", strconv.Itoa(limit), "--json", "number")
	if err != nil {
		return nil, err
	}
	var prs []struct{ Number int }
	if err := json.Unmarshal([]byte(raw), &prs); err != nil {
		return nil, err
	}
	out := make([]PRRef, len(prs))
	for i, p := range prs {
		out[i] = base
		out[i].Number = p.Number
	}
	return out, nil
}

// ReviewComment is a single-line comment. Line is a new-file line for
// side RIGHT and an old-file line for side LEFT, and must be inside the
// PR's diff on GitHub (changed lines plus 3 lines of context).
type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

type Review struct {
	CommitID string          `json:"commit_id,omitempty"`
	Event    string          `json:"event"` // COMMENT | APPROVE | REQUEST_CHANGES
	Body     string          `json:"body,omitempty"`
	Comments []ReviewComment `json:"comments,omitempty"`
}

func (r Review) Validate() error {
	switch r.Event {
	case "COMMENT", "APPROVE", "REQUEST_CHANGES":
	default:
		return fmt.Errorf("event must be COMMENT, APPROVE or REQUEST_CHANGES, got %q", r.Event)
	}
	if r.Event != "APPROVE" && r.Body == "" && len(r.Comments) == 0 {
		return errors.New("a review needs a body or at least one comment")
	}
	if r.Event == "REQUEST_CHANGES" && r.Body == "" {
		return errors.New("request changes needs a review body")
	}
	return nil
}

// SubmitReview posts the review with `gh api` and returns its URL.
func SubmitReview(ref PRRef, r Review) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("gh", "api", "--hostname", ref.HostName(), "--method", "POST",
		fmt.Sprintf("repos/%s/pulls/%d/reviews", ref.Slug(), ref.Number), "--input", "-")
	cmd.Stdin = bytes.NewReader(b)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if gerr := GHError(err, stderr.Bytes()); gerr != nil {
			return "", gerr
		}
		// gh prints GitHub's JSON error body on stdout.
		return "", fmt.Errorf("gh api: %w: %s %s", err, strings.TrimSpace(string(out)), strings.TrimSpace(stderr.String()))
	}
	var resp struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", err
	}
	return resp.HTMLURL, nil
}
