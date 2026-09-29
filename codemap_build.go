package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/amitbet/pr-manager/codemap"
	"github.com/amitbet/pr-manager/internal/activity"
	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/triage"
)

// codeMapBuildMu serializes code-map builds: a build re-ranks the whole
// workspace and writes the map, so two at once would clobber it.
var codeMapBuildMu sync.Mutex

// The indexer reads repos from a workspace, <ws>/code/<name>: PR_MANAGER_WORKSPACE
// if set, else <cache>/workspace. Repos get there three ways:
//   - a local code directory (-code-root, PR_MANAGER_CODE_ROOT): its git
//     checkouts are symlinked in (on Windows without the privilege for
//     that, junctioned in), so the map is built from the code on disk;
//   - an org (-org, PR_MANAGER_ORG): every non-archived, non-fork repo of a
//     GitHub or GitHub Enterprise org or user is cloned (blobless);
//   - on demand: a PR's repo that isn't in the map yet is linked from the
//     code directory, or cloned.
//
// Cross-repo impact (which repos depend on the changed code) only sees the
// repos in the workspace, so indexing the whole org matters.
//
// A repo's directory, which is also its name in the map, is the bare repo
// name (api) unless another repo already has it; then owner__api, then
// host__owner__api (see workspaceName). The map records each repo's
// identity (host/owner/repo from its origin remote), and lookups go by
// that, so acme/api never reads other/api's map.

func workspaceDir(o options) (string, error) {
	ws := os.Getenv("PR_MANAGER_WORKSPACE")
	if ws == "" {
		ws = filepath.Join(o.cache, "workspace")
	}
	ws, err := filepath.Abs(ws)
	if err != nil {
		return "", err
	}
	return ws, os.MkdirAll(filepath.Join(ws, "code"), 0o755)
}

// ensureCodeMap makes sure the PR's repo is in the code map before triage
// runs, so its impact scores, history and treemap exist.
func ensureCodeMap(ctx context.Context, o options, ref triage.PRRef, progress func(stage string, done, total int)) error {
	if o.codemapDir == "" || o.codemapDir == "off" {
		return nil
	}
	ws, err := workspaceDir(o)
	if err != nil {
		return err
	}
	if codeMapHas(loadCodeMap(o.codemapDir), ws, ref) {
		return nil
	}
	codeMapBuildMu.Lock()
	defer codeMapBuildMu.Unlock()
	if codeMapHas(loadCodeMap(o.codemapDir), ws, ref) {
		return nil // another job built it while this one waited
	}
	var local map[string]checkout
	if o.codeRoot != "" {
		if local, err = scanCheckouts(o.codeRoot); err != nil {
			return err
		}
	}
	progress("clone", 0, 0)
	name, err := addRepo(ctx, ws, ref, local)
	if err != nil {
		return skipUnlinked(err)
	}
	progress("codemap", 0, 0)
	if err := buildCodeMap(ctx, o, ws, name); err != nil {
		return err
	}
	if !codeMapHas(reloadCodeMap(o.codemapDir), ws, ref) {
		return fmt.Errorf("code map: built the map but %s is still not in %s", ref.RepoArg(), o.codemapDir)
	}
	return nil
}

// indexResult is what an explicit index run added.
type indexResult struct {
	Linked  []string `json:"linked"`  // from the local code directory
	Cloned  []string `json:"cloned"`  // cloned now
	Updated []string `json:"updated"` // clones that were already there, pulled
	Failed  []string `json:"failed,omitempty"`
	Repos   int      `json:"repos"` // repos in the map afterwards
}

// indexSources puts every repo of the code directory and the org into the
// workspace and rebuilds the map. Only repos that changed are re-extracted.
func indexSources(ctx context.Context, o options, progress func(stage string, done, total int)) (*indexResult, error) {
	if o.codemapDir == "" || o.codemapDir == "off" {
		return nil, errors.New("the code map is off (-codemap off)")
	}
	if o.codeRoot == "" && o.org == "" {
		return nil, errors.New("nothing to index: set a code directory (-code-root, PR_MANAGER_CODE_ROOT) or an org (-org, PR_MANAGER_ORG)")
	}
	codeMapBuildMu.Lock()
	defer codeMapBuildMu.Unlock()
	ws, err := workspaceDir(o)
	if err != nil {
		return nil, err
	}
	res := &indexResult{}
	var local map[string]checkout
	if o.codeRoot != "" {
		if local, err = scanCheckouts(o.codeRoot); err != nil {
			return nil, err
		}
		if len(local) == 0 && o.org == "" {
			return nil, fmt.Errorf("no git checkouts in %s or its subdirectories", o.codeRoot)
		}
	}
	// With an org, the code directory only supplies its repos; without
	// one, every checkout in it is indexed.
	var refs []triage.PRRef
	if o.org != "" {
		host, owner, err := parseOrg(o.org)
		if err != nil {
			return nil, err
		}
		progress("list", 0, 0)
		names, err := orgRepos(ctx, host, owner)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			refs = append(refs, triage.PRRef{Host: host, Owner: owner, Repo: name})
		}
	} else {
		keys := make([]string, 0, len(local))
		for k := range local {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			refs = append(refs, local[k].Ref)
		}
	}
	for i, ref := range refs {
		progress("clone", i, len(refs))
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		_, isLocal := local[checkoutKey(ref)]
		name := workspaceName(ws, ref, local[checkoutKey(ref)].Dir)
		dir := filepath.Join(ws, "code", name)
		had := fileExists(filepath.Join(dir, ".git"))
		if _, err := addRepo(ctx, ws, ref, local); err != nil {
			log.Printf("index: %s: %v", name, err)
			res.Failed = append(res.Failed, name+": "+firstLine(err.Error()))
			continue
		}
		switch {
		case isLocal:
			res.Linked = append(res.Linked, name)
		case had:
			if _, err := triage.GitCtx(ctx, dir, "pull", "--ff-only", "--quiet"); err != nil {
				log.Printf("index: pulling %s: %v", name, err) // index what is there
			}
			res.Updated = append(res.Updated, name)
		default:
			res.Cloned = append(res.Cloned, name)
		}
	}
	progress("codemap", 0, 0)
	if err := buildCodeMap(ctx, o, ws, ""); err != nil {
		return nil, err
	}
	if m := reloadCodeMap(o.codemapDir); m != nil {
		res.Repos = len(m.Meta.Repos)
	}
	return res, nil
}

// addRepo puts one repo in the workspace: a link to its checkout in the
// code directory, else a blobless clone. A repo already there is kept.
// It returns the repo's directory name, its name in the map.
func addRepo(ctx context.Context, ws string, ref triage.PRRef, local map[string]checkout) (string, error) {
	co, isLocal := local[checkoutKey(ref)]
	name := workspaceName(ws, ref, co.Dir)
	dir := filepath.Join(ws, "code", name)
	if isLocal {
		if linksTo(dir, co.Dir) {
			return name, nil
		}
		if fileExists(filepath.Join(dir, ".git")) && !isLink(dir) {
			return name, nil // a clone from before the code directory was set
		}
		_ = os.Remove(dir) // a stale link
		if err := linkDir(co.Dir, dir); err != nil {
			return name, &linkError{repo: name, err: err}
		}
		return name, nil
	}
	if fileExists(filepath.Join(dir, ".git")) {
		return name, nil
	}
	if ref.Owner == "" || (ref.Owner == "local" && ref.Host == "") {
		return "", fmt.Errorf("%s is not in the code directory and there is no org to clone it from", ref.Repo)
	}
	args := []string{"repo", "clone", ref.RepoArg(), dir, "--", "--filter=blob:none", "--quiet"}
	cctx, done := activity.Command(ctx, "", "gh", args...)
	clone := proc.CommandContext(ctx, "gh", args...)
	out, err := clone.CombinedOutput()
	if msg := tail(out); msg != "" {
		activity.Printf(cctx, "%s", msg)
	}
	done(err)
	if err != nil {
		_ = os.RemoveAll(dir)
		if gerr := triage.GHError(err, out); gerr != nil {
			return "", fmt.Errorf("code map: cloning %s: %w", ref.RepoArg(), gerr)
		}
		return "", fmt.Errorf("code map: cloning %s into %s: %v\n%s", ref.RepoArg(), dir, err, tail(out))
	}
	return name, nil
}

// linkError is addRepo failing to link a local checkout in. Triage goes
// on without the repo's map rather than failing (see skipUnlinked): on
// Windows, a symlink needs a privilege and a junction a local path, and
// neither is something the user can fix from here.
type linkError struct {
	repo string
	err  error
}

func (e *linkError) Error() string {
	return fmt.Sprintf("code map: linking %s into the workspace: %v", e.repo, e.err)
}

func (e *linkError) Unwrap() error { return e.err }

// skipUnlinked turns a linkError into a logged warning, so the run goes on
// without a map for that repo; other errors are returned.
func skipUnlinked(err error) error {
	var le *linkError
	if errors.As(err, &le) {
		log.Printf("warn: %v; triaging without the code map for %s", le, le.repo)
		return nil
	}
	return err
}

// isLink reports whether dir is a symlink or, on Windows, a junction.
func isLink(dir string) bool {
	_, err := os.Readlink(dir)
	return err == nil
}

// linksTo reports whether dir is a link to target. Readlink's answer is
// not always spelled like target (a junction reads back normalized), so
// failing an exact match, the two are compared as files.
func linksTo(dir, target string) bool {
	cur, err := os.Readlink(dir)
	if err != nil {
		return false
	}
	if cur == target {
		return true
	}
	a, err := os.Stat(dir)
	if err != nil {
		return false
	}
	b, err := os.Stat(target)
	return err == nil && os.SameFile(a, b)
}

// repoIdentity is the ref's codemap.RepoID; "" for a checkout without a
// remote (owner "local"), which only its bare name can find.
func repoIdentity(ref triage.PRRef) string {
	if ref.Owner == "local" && ref.Host == "" {
		return ""
	}
	return codemap.RepoID(ref.Host, ref.Owner, ref.Repo)
}

// checkout is a git checkout in the code directory.
type checkout struct {
	Ref triage.PRRef
	Dir string
}

// checkoutKey keys scanCheckouts' result: the repo's identity, or
// local/<name> for a checkout without a remote.
func checkoutKey(ref triage.PRRef) string {
	if id := repoIdentity(ref); id != "" {
		return id
	}
	return "local/" + ref.Repo
}

// pathPart makes s safe as part of one file name: path separators (a
// GitLab subgroup, an Azure DevOps org/project) and % are escaped, so
// distinct names stay distinct and names without them are unchanged.
func pathPart(s string) string {
	return strings.NewReplacer("%", "%25", "/", "%2F", `\`, "%5C").Replace(s)
}

// workspaceName picks ref's directory under <ws>/code: the first of repo,
// owner__repo, host__owner__repo that is free or already holds this repo
// (by origin remote, or a link to src, its checkout in the code
// directory). Existing workspaces keep their bare names.
func workspaceName(ws string, ref triage.PRRef, src string) string {
	id := repoIdentity(ref)
	host := ref.Host
	if host == "" {
		host = "github.com"
	}
	var cands []string
	if ref.Repo != "repos" { // repos.jsonl is the map's repo index
		cands = append(cands, pathPart(ref.Repo))
	}
	cands = append(cands, pathPart(ref.Owner)+"__"+pathPart(ref.Repo), pathPart(host)+"__"+pathPart(ref.Owner)+"__"+pathPart(ref.Repo))
	for _, c := range cands {
		dir := filepath.Join(ws, "code", c)
		if _, err := os.Lstat(dir); err != nil {
			return c
		}
		if _, err := os.Stat(dir); err != nil {
			return c // a link to a checkout that is gone
		}
		if src != "" && linksTo(dir, src) {
			return c
		}
		if id == "" || dirIdentity(dir) == id {
			return c
		}
	}
	return cands[len(cands)-1]
}

// dirIdentity is the identity of the checkout in dir by its origin remote.
func dirIdentity(dir string) string {
	url, err := triage.Git(dir, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return codemap.RemoteID(strings.TrimSpace(url))
}

// buildCodeMap runs the indexer bundled in this executable over the
// workspace. repo forces that repo to be re-extracted; the rest reuse
// their cached graphs unless they changed.
func buildCodeMap(ctx context.Context, o options, ws, repo string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"codemap", "build", "-workspace", ws, "-output", o.codemapDir, "-cache", filepath.Join(o.cache, "graphs")}
	if repo != "" {
		args = append(args, "-repo", repo)
	}
	if o.codemapConfig != "" {
		args = append(args, "-config", o.codemapConfig)
	}
	cctx, done := activity.Command(ctx, "", "pr-manager", args...)
	var buf bytes.Buffer
	lw := activity.Writer(cctx, "")
	cmd := proc.CommandContext(ctx, exe, args...)
	cmd.Stdout = io.MultiWriter(&buf, lw)
	cmd.Stderr = cmd.Stdout
	err = cmd.Run()
	lw.Flush()
	done(err)
	out := buf.Bytes()
	if err != nil {
		what := "the workspace"
		if repo != "" {
			what = repo
		}
		return fmt.Errorf("code map: building %s: %v\n%s", what, err, tail(out))
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "warn:") {
			log.Print(line)
		}
	}
	return nil
}

// scanCheckouts finds the git checkouts in root and one level below it
// (~/code/<repo>, ~/code/<org>/<repo>, a workspace's code/), keyed by
// checkoutKey: the repo's identity from its origin remote, so a checkout
// in a renamed directory still matches the PR's repo and two orgs' api
// repos are both found, else local/<directory name>.
func scanCheckouts(root string) (map[string]checkout, error) {
	root, err := filepath.Abs(expandHome(root))
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("code directory %s: not a directory", root)
	}
	out := map[string]checkout{}
	add := func(dir string) {
		ref := localRepoRef(dir)
		if k := checkoutKey(ref); out[k].Dir == "" {
			out[k] = checkout{Ref: ref, Dir: dir}
		}
	}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" {
				continue
			}
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err != nil || !st.IsDir() {
				continue
			}
			if fileExists(filepath.Join(p, ".git")) {
				if real, err := codemap.ResolveLink(p); err == nil {
					add(real)
				}
			} else if depth < 2 {
				walk(p, depth+1)
			}
		}
	}
	if fileExists(filepath.Join(root, ".git")) {
		add(root) // the directory is itself one checkout
	} else {
		walk(root, 1)
	}
	return out, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// parseOrg reads an org or user: name, host/name, or its URL
// (https://github.com/acme, https://ghe.corp.com/platform).
func parseOrg(s string) (host, owner string, err error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s = strings.Trim(s, "/")
	parts := strings.Split(s, "/")
	switch {
	case len(parts) == 1 && parts[0] != "":
		return triage.DefaultHost(), parts[0], nil
	case len(parts) == 2 && strings.Contains(parts[0], "."):
		return triage.NormalizeHost(parts[0]), parts[1], nil
	}
	return "", "", fmt.Errorf("org %q: want name, host/name or https://host/name", s)
}

// orgRepos lists an org's (or user's) repos with gh, leaving out archived
// repos and forks.
func orgRepos(ctx context.Context, host, owner string) ([]string, error) {
	args := []string{"repo", "list", owner, "--no-archived", "--source", "--limit", "2000", "--json", "name"}
	_, done := activity.Command(ctx, "", "gh", args...)
	cmd := proc.CommandContext(ctx, "gh", args...)
	cmd.Env = append(os.Environ(), "GH_HOST="+orDefault(host, "github.com")) // gh repo list has no --hostname
	out, err := cmd.Output()
	done(err)
	if err != nil {
		var stderr []byte
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = ee.Stderr
		}
		if gerr := triage.GHError(err, stderr); gerr != nil {
			return nil, gerr
		}
		return nil, fmt.Errorf("listing repos of %s: %v %s", path.Join(orDefault(host, "github.com"), owner), err, strings.TrimSpace(string(stderr)))
	}
	var repos []struct{ Name string }
	if err := json.Unmarshal(out, &repos); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("%s has no repos you can see (archived repos and forks are left out)", owner)
	}
	return names, nil
}

// codeMapHas reports whether the map holds ref's repo. A repo the map
// knows only by name (a map built before identities were recorded) counts
// unless its workspace directory is a checkout of another repo.
func codeMapHas(m *codemap.Map, ws string, ref triage.PRRef) bool {
	id := repoIdentity(ref)
	name := m.RepoName(id, ref.Repo)
	if name == "" {
		return false
	}
	if id == "" || m.Meta.Repos[name].Remote != "" {
		return true
	}
	dir := filepath.Join(ws, "code", name)
	if _, err := os.Stat(dir); err != nil {
		return true // not in this workspace: the name is all there is to go by
	}
	got := dirIdentity(dir)
	return got == "" || got == id
}

// codeMapRepo is ref's repo name in the map, for triage.CodeMap. A repo
// the map does not have gets a name the map cannot hold (owner/repo), so
// another repo's same-named entry is never used for it.
func codeMapRepo(m *codemap.Map, ref triage.PRRef) string {
	if name := m.RepoName(repoIdentity(ref), ref.Repo); name != "" {
		return name
	}
	if _, taken := m.Meta.Repos[ref.Repo]; taken {
		return ref.Slug()
	}
	return ref.Repo
}

// tail keeps the last lines of a command's output for an error message.
func tail(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	return strings.Join(lines, "\n")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
