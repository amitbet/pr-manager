package main

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/amitbet/pr-manager/codemap"
	"github.com/amitbet/pr-manager/triage"
)

// A repository typed in the box as a URL or owner/repo, rather than a
// path, is picked from in its checkout in the code directory or the
// workspace (codemap_build.go) when there is one. Otherwise its branches
// and commits come from the forge's API (revremote.go), nothing is cloned
// to list them, and the one picked is fetched for its review into the
// clone PR reviews use, which holds no files on disk.

// revRepo is a repository named by URL or owner/repo.
type revRepo struct {
	Ref triage.PRRef
	// URL is what git clones for a host gh doesn't serve (GitLab and the
	// like); empty for GitHub, which gh clones with its own login.
	URL string
}

func (r revRepo) Name() string { return r.Ref.RepoArg() }

// Web is the repository's page, which a picked rev is named on:
// https://github.com/acme/api#feature.
func (r revRepo) Web() string { return "https://" + r.Name() }

// CloneURL is what git clones.
func (r revRepo) CloneURL() string {
	if r.URL != "" {
		return r.URL
	}
	return r.Web() + ".git"
}

var drivePath = regexp.MustCompile(`^[A-Za-z]:`)

// looksLikePath says whether s names a local path rather than a repo:
// it starts like one, or exists.
func looksLikePath(s string) bool {
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, `\`) || drivePath.MatchString(s) {
		return true
	}
	p, _ := splitRev(s)
	_, err := os.Stat(p)
	return err == nil
}

// githubHost says whether gh serves host, as PRRef.Host has it.
func githubHost(host string) bool {
	return host == "" || host == triage.DefaultHost() || strings.Contains(host, "github")
}

// parseRevRepo reads s as a repository: a GitHub or GitLab URL of it or of
// anything in it (a PR, a merge request, a branch, a file), an ssh remote,
// owner/repo or host/owner/repo. ok is false for a local path.
func parseRevRepo(s string) (revRepo, bool) {
	s = strings.TrimSpace(s)
	if s == "" || looksLikePath(s) {
		return revRepo{}, false
	}
	if ref, err := triage.ParsePRRef(s); err == nil {
		ref.Number = 0
		return revRepo{Ref: ref}, true
	}
	s, _, _ = strings.Cut(s, "#") // a rev on it: acme/api#feature
	remote := s
	if u, err := url.Parse(s); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
		p := strings.Trim(u.Path, "/")
		if i := strings.Index(p, "/-/"); i >= 0 {
			p = p[:i] // GitLab: group/sub/repo/-/merge_requests/5
		} else if parts := strings.Split(p, "/"); !strings.Contains(u.Host, "gitlab") && len(parts) > 2 {
			p = strings.Join(parts[:2], "/") // GitHub: owner/repo/tree/main
		}
		remote = u.Scheme + "://" + u.Host + "/" + p
	} else if !strings.Contains(s, "@") && !strings.Contains(s, "://") {
		host, owner, repo, err := triage.ParseRepo(s)
		if err != nil {
			return revRepo{}, false
		}
		remote = "https://" + triage.PRRef{Host: host}.HostName() + "/" + owner + "/" + repo
	}
	host, owner, repo, ok := codemap.ParseRemote(remote)
	if !ok {
		return revRepo{}, false
	}
	r := revRepo{Ref: triage.PRRef{Host: triage.NormalizeHost(host), Owner: owner, Repo: repo}}
	if !githubHost(r.Ref.Host) {
		r.URL = remote
	}
	return r, true
}

// findRevRepo finds r's checkout: in the code directory, else the
// workspace's; empty when it has none.
func findRevRepo(o options, r revRepo) (string, error) {
	if o.codeRoot != "" {
		if cos, err := scanCheckouts(o.codeRoot); err == nil {
			if co, ok := cos[checkoutKey(r.Ref)]; ok {
				return co.Dir, nil
			}
		}
	}
	ws, err := workspaceDir(o)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(ws, "code", workspaceName(ws, r.Ref, ""))
	if fileExists(filepath.Join(dir, ".git")) && dirIdentity(dir) == repoIdentity(r.Ref) {
		return codemap.ResolveLink(dir) // a link to a checkout in the code directory
	}
	return "", nil
}

// resolveRevPath resolves what the box holds to a checkout: a path's root,
// or a repository's checkout. repo is set for a repository; dir is empty
// for one without a checkout here.
func resolveRevPath(o options, s string) (dir string, repo *revRepo, err error) {
	r, ok := parseRevRepo(s)
	if !ok {
		path, _ := splitRev(s)
		dir, err = checkoutRoot(path)
		return dir, nil, err
	}
	dir, err = findRevRepo(o, r)
	return dir, &r, err
}

// noPromptEnv is the environment for git talking to a remote from the UI:
// a remote that needs a password fails rather than waiting for one.
func noPromptEnv() []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	return env
}
