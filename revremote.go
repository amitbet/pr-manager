package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/triage"
)

// A repository without a checkout here is listed from its forge: GitHub
// through gh's GraphQL, GitLab through its REST API (GITLAB_TOKEN for a
// private one). A rev picked from the list is named on the repository's
// page, https://github.com/acme/api#feature or #main...feature, and its
// review fetches just that into the clone PR reviews use (remoteRevDir),
// which keeps no files on disk.

// revRemoteTimeout caps the forge's answer, so the picker doesn't hang on
// it.
const revRemoteTimeout = 30 * time.Second

// listRemoteRevs lists r's branches, newest first, and its default
// branch's recent commits.
func listRemoteRevs(ctx context.Context, r revRepo) (*revList, error) {
	ctx, cancel := context.WithTimeout(ctx, revRemoteTimeout)
	defer cancel()
	var out *revList
	var err error
	if r.URL == "" {
		out, err = githubRevs(ctx, r)
	} else {
		out, err = gitlabRevs(ctx, r)
	}
	if err != nil {
		return nil, err
	}
	out.Repo, out.Web, out.Remote = r.Name(), r.Web(), true
	sort.SliceStable(out.Branches, func(i, j int) bool { return out.Branches[i].Date > out.Branches[j].Date })
	return out, nil
}

type ghCommit struct {
	Oid      string `json:"oid"`
	Date     string `json:"committedDate"`
	Headline string `json:"messageHeadline"`
	Adds     int    `json:"additions"`
	Dels     int    `json:"deletions"`
	Files    *int   `json:"changedFilesIfAvailable"`
	Parents  struct {
		TotalCount int `json:"totalCount"`
	} `json:"parents"`
	Author struct {
		Name string `json:"name"`
	} `json:"author"`
}

const githubRevsQuery = `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    defaultBranchRef { name target { ... on Commit { history(first: 100) { nodes {
      oid committedDate messageHeadline additions deletions changedFilesIfAvailable parents { totalCount } author { name } } } } } }
    refs(refPrefix: "refs/heads/", first: 100, orderBy: {field: TAG_COMMIT_DATE, direction: DESC}) { nodes {
      name
      target { ... on Commit { oid committedDate messageHeadline author { name } } }
      associatedPullRequests(states: OPEN, first: 5) { nodes { number url headRefName isCrossRepository } } } }
  }
}`

func ghGraphQL(ctx context.Context, host string, query string, vars map[string]string, v any) error {
	args := []string{"api", "graphql", "--hostname", host, "-f", "query=" + query}
	for k, val := range vars {
		args = append(args, "-f", k+"="+val)
	}
	cmd := proc.CommandContext(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = ee.Stderr
		}
		if gerr := triage.GHError(err, stderr); gerr != nil {
			return gerr
		}
		return fmt.Errorf("gh api graphql: %v %s", err, strings.TrimSpace(string(stderr)))
	}
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 && len(resp.Data) == 0 {
		return errors.New(resp.Errors[0].Message)
	}
	return json.Unmarshal(resp.Data, v)
}

func githubRevs(ctx context.Context, r revRepo) (*revList, error) {
	var data struct {
		Repository *struct {
			DefaultBranchRef *struct {
				Name   string `json:"name"`
				Target struct {
					History struct {
						Nodes []ghCommit `json:"nodes"`
					} `json:"history"`
				} `json:"target"`
			} `json:"defaultBranchRef"`
			Refs struct {
				Nodes []struct {
					Name   string   `json:"name"`
					Target ghCommit `json:"target"`
					PRs    struct {
						Nodes []struct {
							Number int    `json:"number"`
							URL    string `json:"url"`
							Head   string `json:"headRefName"`
							Fork   bool   `json:"isCrossRepository"`
						} `json:"nodes"`
					} `json:"associatedPullRequests"`
				} `json:"nodes"`
			} `json:"refs"`
		} `json:"repository"`
	}
	host := r.Ref.HostName()
	if err := ghGraphQL(ctx, host, githubRevsQuery, map[string]string{"owner": r.Ref.Owner, "name": r.Ref.Repo}, &data); err != nil {
		return nil, fmt.Errorf("listing %s: %w", r.Name(), err)
	}
	if data.Repository == nil {
		return nil, fmt.Errorf("%s not found, or gh's login can't see it", r.Name())
	}
	out := &revList{Branches: []revBranch{}, Commits: []revCommit{}}
	if d := data.Repository.DefaultBranchRef; d != nil {
		out.BaseRef, out.Base = d.Name, d.Name
		for _, c := range d.Target.History.Nodes {
			files := -1
			if c.Files != nil {
				files = *c.Files
			}
			out.Commits = append(out.Commits, revCommit{Oid: c.Oid, Date: c.Date, Author: c.Author.Name, Title: c.Headline,
				Merge: c.Parents.TotalCount > 1, Files: files, Adds: c.Adds, Dels: c.Dels})
		}
	}
	for _, n := range data.Repository.Refs.Nodes {
		b := revBranch{Name: n.Name, Oid: n.Target.Oid, Date: n.Target.Date, Author: n.Target.Author.Name, Title: n.Target.Headline,
			Ahead: -1, Unpushed: -1, Unpulled: -1}
		// The branch's own PR: the list also has PRs into it, and from
		// forks' branches of the same name.
		for _, pr := range n.PRs.Nodes {
			if pr.Head == n.Name && !pr.Fork {
				b.PR, b.PRNumber = pr.URL, pr.Number
				break
			}
		}
		out.Branches = append(out.Branches, b)
	}
	if out.BaseRef != "" {
		githubAhead(ctx, r, out)
	}
	return out, nil
}

// githubAhead counts how far the newest branches are ahead of the
// default branch, in a query of their own: comparing every branch can
// take GitHub longer than it allows. A failure leaves them uncounted.
func githubAhead(ctx context.Context, r revRepo, out *revList) {
	sort.SliceStable(out.Branches, func(i, j int) bool { return out.Branches[i].Date > out.Branches[j].Date })
	n := min(len(out.Branches), revAheadLimit/2)
	if n == 0 {
		return
	}
	vars := map[string]string{"owner": r.Ref.Owner, "name": r.Ref.Repo, "base": out.BaseRef}
	var params, fields strings.Builder
	for i := range n {
		k := "n" + strconv.Itoa(i)
		vars[k] = "refs/heads/" + out.Branches[i].Name
		fmt.Fprintf(&params, ", $%s: String!", k)
		// The branch is compare's base: its commits the default branch
		// lacks are behindBy.
		fmt.Fprintf(&fields, " b%d: ref(qualifiedName: $%s) { compare(headRef: $base) { behindBy } }", i, k)
	}
	q := fmt.Sprintf("query($owner: String!, $name: String!, $base: String!%s) { repository(owner: $owner, name: $name) {%s } }", params.String(), fields.String())
	var data struct {
		Repository map[string]*struct {
			Compare *struct {
				BehindBy int `json:"behindBy"`
			} `json:"compare"`
		} `json:"repository"`
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ghGraphQL(ctx, r.Ref.HostName(), q, vars, &data); err != nil {
		return
	}
	for i := range n {
		if ref := data.Repository["b"+strconv.Itoa(i)]; ref != nil && ref.Compare != nil {
			out.Branches[i].Ahead = ref.Compare.BehindBy
		}
	}
}

// gitlabGet reads path from r's GitLab API into v.
func gitlabGet(ctx context.Context, r revRepo, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://"+r.Ref.HostName()+"/api/v4/projects/"+url.PathEscape(r.Ref.Owner+"/"+r.Ref.Repo)+path, nil)
	if err != nil {
		return err
	}
	if tok := firstEnv("GITLAB_TOKEN", "GL_TOKEN", "GITLAB_PRIVATE_TOKEN"); tok != "" {
		req.Header.Set("PRIVATE-TOKEN", tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode == 404 || resp.StatusCode == 401 {
			return fmt.Errorf("%s not found, or private: set GITLAB_TOKEN to a token that can read it (%s)", r.Name(), resp.Status)
		}
		return fmt.Errorf("%s: %s %s", r.Name(), resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

type glCommit struct {
	ID      string   `json:"id"`
	Date    string   `json:"committed_date"`
	Title   string   `json:"title"`
	Author  string   `json:"author_name"`
	Parents []string `json:"parent_ids"`
	Stats   *struct {
		Adds int `json:"additions"`
		Dels int `json:"deletions"`
	} `json:"stats"`
}

func gitlabRevs(ctx context.Context, r revRepo) (*revList, error) {
	var project struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := gitlabGet(ctx, r, "", &project); err != nil {
		return nil, err
	}
	var branches []struct {
		Name   string   `json:"name"`
		Commit glCommit `json:"commit"`
	}
	if err := gitlabGet(ctx, r, "/repository/branches?per_page=100&sort=updated_desc", &branches); err != nil {
		return nil, err
	}
	var commits []glCommit
	if err := gitlabGet(ctx, r, "/repository/commits?per_page=100&with_stats=true", &commits); err != nil {
		return nil, err
	}
	out := &revList{BaseRef: project.DefaultBranch, Base: project.DefaultBranch, Branches: []revBranch{}, Commits: []revCommit{}}
	for _, b := range branches {
		out.Branches = append(out.Branches, revBranch{Name: b.Name, Oid: b.Commit.ID, Date: utc(b.Commit.Date), Author: b.Commit.Author, Title: b.Commit.Title,
			Ahead: -1, Unpushed: -1, Unpulled: -1})
	}
	for _, c := range commits {
		rc := revCommit{Oid: c.ID, Date: utc(c.Date), Author: c.Author, Title: c.Title, Merge: len(c.Parents) > 1, Files: -1}
		if c.Stats != nil {
			rc.Adds, rc.Dels = c.Stats.Adds, c.Stats.Dels
		}
		out.Commits = append(out.Commits, rc)
	}
	return out, nil
}

// utc writes a forge's timestamp in UTC, so the list sorts by its text.
func utc(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format(time.RFC3339)
}

// splitRemoteRev reads s as a rev on a repository's page,
// https://host/owner/repo#rev, as the picker names one. A number after #
// is a PR, not a rev.
func splitRemoteRev(s string) (revRepo, string, bool) {
	s = strings.TrimSpace(s)
	i := strings.LastIndex(s, "#")
	if i < 0 || !strings.HasPrefix(s, "https://") {
		return revRepo{}, "", false
	}
	rev := s[i+1:]
	if _, err := strconv.Atoi(rev); err == nil || rev == "" {
		return revRepo{}, "", false
	}
	r, ok := parseRevRepo(s[:i])
	return r, rev, ok
}

// remoteRevRoot is where PR reviews clone repositories, which reviews of
// a rev picked from a forge use too; set by the server.
var remoteRevRoot string

// remoteRevDir says whether dir is one of those clones, which have no
// files on disk to fix or map.
func remoteRevDir(dir string) bool {
	if remoteRevRoot == "" || dir == "" {
		return false
	}
	root := remoteRevRoot
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

var remoteRevMu sync.Mutex

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// fetchRemoteRev fetches rev (a commit, a branch or base...branch) and the
// default branch from r into its clone under root, cloning it first
// without files, blobs on demand, as PR reviews do. It returns the clone
// and rev as named there: origin/feature.
func fetchRemoteRev(ctx context.Context, root string, r revRepo, rev string, progress func(string, int, int)) (string, string, error) {
	remoteRevMu.Lock()
	defer remoteRevMu.Unlock()
	dir := (&triage.PRFetcher{Dir: root}).RepoDir(r.Ref)
	git := func(args ...string) (string, error) {
		cmd := proc.CommandContext(ctx, "git", triage.GitArgs(args...)...)
		cmd.Dir, cmd.Env = dir, noPromptEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			msg := tail(out)
			if msg == "" {
				msg = err.Error()
			}
			return "", fmt.Errorf("git %s: %s", args[0], msg)
		}
		return string(out), nil
	}
	if !fileExists(filepath.Join(dir, ".git")) {
		progress("clonerev", 0, 0)
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", "", err
		}
		flags := []string{"--filter=blob:none", "--no-checkout", "--quiet", "--config=core.longpaths=true"}
		var args []string
		name := "git"
		if r.URL == "" {
			name, args = "gh", append([]string{"repo", "clone", r.Ref.RepoArg(), dir, "--"}, flags...)
		} else {
			args = append(append([]string{"clone"}, flags...), "--", r.CloneURL(), dir)
		}
		cmd := proc.CommandContext(ctx, name, args...)
		cmd.Env = noPromptEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			_ = os.RemoveAll(dir) // a killed clone leaves a .git that isn't one
			if gerr := triage.GHError(err, out); gerr != nil && r.URL == "" {
				return "", "", fmt.Errorf("cloning %s: %w", r.Name(), gerr)
			}
			return "", "", fmt.Errorf("cloning %s: %s", r.Name(), tail(out))
		}
	}
	progress("fetchrev", 0, 0)
	if _, err := triage.Git(dir, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err != nil {
		if _, err := git("remote", "set-head", "origin", "--auto"); err != nil {
			return "", "", err
		}
	}
	def, err := triage.Git(dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", "", err
	}
	def = strings.TrimPrefix(strings.TrimSpace(def), "origin/")
	specs := []string{"+refs/heads/" + def + ":refs/remotes/origin/" + def}
	name := func(part string) (string, error) {
		switch {
		case fullSHA.MatchString(part):
			specs = append(specs, part)
			return part, nil
		case part == "" || strings.HasPrefix(part, "-") || strings.ContainsAny(part, " :~^?*[\\"):
			return "", fmt.Errorf("bad revision %q", rev)
		}
		specs = append(specs, "+refs/heads/"+part+":refs/remotes/origin/"+part)
		return "origin/" + part, nil
	}
	local := ""
	if base, head, ok := revRange(rev); ok {
		b, err := name(base)
		if err != nil {
			return "", "", err
		}
		h, err := name(head)
		if err != nil {
			return "", "", err
		}
		local = b + "..." + h
	} else if local, err = name(rev); err != nil {
		return "", "", err
	}
	if _, err := git(append([]string{"fetch", "--quiet", "--no-tags", "origin"}, specs...)...); err != nil {
		return "", "", err
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", err
	}
	return real, local, nil
}
