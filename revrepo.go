package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/amitbet/pr-manager/codemap"
	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/triage"
)

// A repository typed in the box as a URL or owner/repo, rather than a
// path, is picked from in a checkout of it: the one in the code directory,
// else a clone in the workspace (codemap_build.go), which the picker makes
// when asked to. The code map then finds the same clone.

// revRepo is a repository named by URL or owner/repo.
type revRepo struct {
	Ref triage.PRRef
	// URL is what git clones for a host gh doesn't serve (GitLab and the
	// like); empty for GitHub, which gh clones with its own login.
	URL string
}

func (r revRepo) Name() string { return r.Ref.RepoArg() }

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
// workspace's clone. With none, dir is empty and dest is where a clone
// would go.
func findRevRepo(o options, r revRepo) (dir, dest string, err error) {
	if o.codeRoot != "" {
		if cos, err := scanCheckouts(o.codeRoot); err == nil {
			if co, ok := cos[checkoutKey(r.Ref)]; ok {
				return co.Dir, "", nil
			}
		}
	}
	ws, err := workspaceDir(o)
	if err != nil {
		return "", "", err
	}
	dest = filepath.Join(ws, "code", workspaceName(ws, r.Ref, ""))
	if fileExists(filepath.Join(dest, ".git")) && dirIdentity(dest) == repoIdentity(r.Ref) {
		real, err := codemap.ResolveLink(dest) // a link to a checkout in the code directory
		return real, "", err
	}
	return "", dest, nil
}

// resolveRevPath resolves what the box holds to a checkout: a path's root,
// or a repository's checkout. repo is set for a repository, and dest is
// where it would be cloned when it has no checkout yet.
func resolveRevPath(o options, s string) (dir string, repo *revRepo, dest string, err error) {
	r, ok := parseRevRepo(s)
	if !ok {
		path, _ := splitRev(s)
		dir, err = checkoutRoot(path)
		return dir, nil, "", err
	}
	dir, dest, err = findRevRepo(o, r)
	return dir, &r, dest, err
}

var revCloneMu sync.Mutex

// cloneRevRepo clones r into dest, blobs on demand as the workspace's
// other clones are. It clones next to dest and renames, so the code map
// never sees half a clone. It never prompts.
func cloneRevRepo(ctx context.Context, r revRepo, dest string) error {
	revCloneMu.Lock()
	defer revCloneMu.Unlock()
	if fileExists(filepath.Join(dest, ".git")) {
		return nil
	}
	tmp := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".cloning")
	_ = os.RemoveAll(tmp)
	flags := []string{"--filter=blob:none", "--quiet", "--config=core.longpaths=true"}
	var cmd *exec.Cmd
	if r.URL == "" {
		cmd = proc.CommandContext(ctx, "gh", append([]string{"repo", "clone", r.Ref.RepoArg(), tmp, "--"}, flags...)...)
	} else {
		cmd = proc.CommandContext(ctx, "git", append(append([]string{"clone"}, flags...), "--", r.URL, tmp)...)
	}
	cmd.Env = noPromptEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(tmp)
		if r.URL == "" {
			if gerr := triage.GHError(err, out); gerr != nil {
				return fmt.Errorf("cloning %s: %w", r.Name(), gerr)
			}
		}
		msg := tail(out)
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("cloning %s: %s", r.Name(), msg)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return nil
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
