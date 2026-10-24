package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/triage"
)

func TestParseRevRepo(t *testing.T) {
	t.Setenv("GH_HOST", "")
	for _, tc := range []struct {
		in, name, url string
	}{
		{"https://github.com/acme/api", "github.com/acme/api", ""},
		{"https://github.com/acme/api.git", "github.com/acme/api", ""},
		{"https://github.com/acme/api/tree/feature/x", "github.com/acme/api", ""},
		{"https://github.com/acme/api/pull/12", "github.com/acme/api", ""},
		{"acme/api", "github.com/acme/api", ""},
		{"acme/api#12", "github.com/acme/api", ""},
		{"git@github.com:acme/api.git", "github.com/acme/api", ""},
		{"https://gitlab.com/group/sub/api", "gitlab.com/group/sub/api", "https://gitlab.com/group/sub/api"},
		{"https://gitlab.com/group/sub/api/-/merge_requests/5", "gitlab.com/group/sub/api", "https://gitlab.com/group/sub/api"},
		{"https://gitlab.com/group/api/-/tree/main", "gitlab.com/group/api", "https://gitlab.com/group/api"},
		{"gitlab.com/group/api", "gitlab.com/group/api", "https://gitlab.com/group/api"},
	} {
		r, ok := parseRevRepo(tc.in)
		if !ok || r.Name() != tc.name || r.URL != tc.url {
			t.Errorf("%s: %v %q %q, want %q %q", tc.in, ok, r.Name(), r.URL, tc.name, tc.url)
		}
	}
	for _, in := range []string{"", "/src/api", "~/src/api", "./api", `C:\src\api`, "/src/api#feature"} {
		if r, ok := parseRevRepo(in); ok {
			t.Errorf("%q read as repo %s", in, r.Name())
		}
	}
}

// revFixture is a checkout of an origin with main and a feature branch,
// whose feature has a pushed and an unpushed commit.
func revFixture(t *testing.T) (root, clone string) {
	root = t.TempDir()
	remote := filepath.Join(root, "remote.git")
	gitTest(t, root, "init", "--bare", "-b", "main", remote)
	clone = filepath.Join(root, "clone")
	gitTest(t, root, "init", "-b", "main", clone)
	gitTest(t, clone, "config", "user.name", "Test")
	gitTest(t, clone, "config", "user.email", "test@example.com")
	gitTest(t, clone, "remote", "add", "origin", remote)
	commit := func(file, body string) {
		if err := os.WriteFile(filepath.Join(clone, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gitTest(t, clone, "add", "-A")
		gitTest(t, clone, "commit", "-q", "-m", file)
	}
	commit("base.txt", "base\n")
	gitTest(t, clone, "push", "-q", "-u", "origin", "main")
	gitTest(t, clone, "remote", "set-head", "origin", "main")
	gitTest(t, clone, "switch", "-q", "-c", "feature")
	commit("pushed.txt", "pushed\n")
	gitTest(t, clone, "push", "-q", "-u", "origin", "feature")
	commit("local.txt", "local\n")
	return root, clone
}

func TestInspectRevComparesWithBase(t *testing.T) {
	_, clone := revFixture(t)
	ctx := context.Background()

	s, err := inspectLocal(ctx, clone+"#origin/feature...feature")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.raw, "local.txt") || strings.Contains(s.raw, "pushed.txt") {
		t.Fatalf("unpushed diff:\n%s", s.raw)
	}
	if s.info.Ahead != 1 || s.info.BaseRef != "feature" || s.info.HeadRef != "feature" || s.info.Rev != "origin/feature...feature" || s.info.SingleCommit {
		t.Fatalf("unpushed info: %+v", s.info)
	}
	if real, _ := filepath.EvalSymlinks(clone); localSource(s.info) != real+"#origin/feature...feature" {
		t.Fatalf("source: %s", localSource(s.info))
	}

	s, err = inspectLocal(ctx, clone+"#main...feature")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.raw, "local.txt") || !strings.Contains(s.raw, "pushed.txt") || s.info.Ahead != 2 || s.info.BaseRef != "main" {
		t.Fatalf("from main: %+v\n%s", s.info, s.raw)
	}

	// A commit as the base reads as its short SHA, and can't publish.
	first := gitTest(t, clone, "rev-parse", "main")
	s, err = inspectLocal(ctx, clone+"#"+first+"...feature")
	if err != nil {
		t.Fatal(err)
	}
	if s.info.BaseRef != first[:10] {
		t.Fatalf("commit base: %+v", s.info)
	}
	if _, err := publishLocal(&PRResult{PR: s.info}, nil); err == nil || !strings.Contains(err.Error(), "not a branch") {
		t.Fatalf("publish from a commit base: %v", err)
	}

	for _, bad := range []string{"main..." + first, "...feature", "main...", "-x...feature", "nope...feature"} {
		if _, err := inspectLocal(ctx, clone+"#"+bad); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
	if _, err := inspectLocal(ctx, clone+"#feature...feature"); err == nil || !strings.Contains(err.Error(), "no changes") {
		t.Fatalf("same branch: %v", err)
	}
}

func TestListRevsCountsUnpushed(t *testing.T) {
	_, clone := revFixture(t)
	gitTest(t, clone, "branch", "lonely", "main")
	revs, err := listRevs(clone)
	if err != nil {
		t.Fatal(err)
	}
	if revs.Base != "origin/main" {
		t.Fatalf("base: %q", revs.Base)
	}
	got := map[string]revBranch{}
	for _, b := range revs.Branches {
		got[b.Name] = b
	}
	if f := got["feature"]; f.Upstream != "origin/feature" || f.Unpushed != 1 || f.Unpulled != 0 || f.Ahead != 2 {
		t.Fatalf("feature: %+v", f)
	}
	if m := got["main"]; m.Upstream != "origin/main" || m.Unpushed != 0 {
		t.Fatalf("main: %+v", m)
	}
	if l := got["lonely"]; l.Upstream != "" || l.Unpushed != -1 {
		t.Fatalf("branch not on origin: %+v", l)
	}
	if o := got["origin/feature"]; !o.Remote || o.Upstream != "" {
		t.Fatalf("origin branch: %+v", o)
	}
}

// A single-branch clone has only its branch's refspec; the picker's fetch
// still lists every branch of origin.
func TestFetchRevsSingleBranchClone(t *testing.T) {
	root, clone := revFixture(t)
	single := filepath.Join(root, "single")
	gitTest(t, root, "clone", "-q", "--single-branch", "-b", "main", filepath.Join(root, "remote.git"), single)
	has := func() bool {
		revs, err := listRevs(single)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range revs.Branches {
			if b.Name == "origin/feature" {
				return true
			}
		}
		return false
	}
	if has() {
		t.Fatal("single-branch clone lists origin/feature before fetching")
	}
	if err := fetchRevs(context.Background(), single); err != nil {
		t.Fatal(err)
	}
	if !has() {
		t.Fatal("origin/feature not listed after fetching")
	}
	_ = clone
}

// A repository named by URL resolves to its checkout in the code
// directory, else the workspace's, else none: nothing is cloned for it.
func TestResolveRevPathFindsCheckouts(t *testing.T) {
	root, _ := revFixture(t)
	t.Setenv("PR_MANAGER_WORKSPACE", "")
	o := options{cache: filepath.Join(root, "cache")}
	r := revRepo{Ref: triage.PRRef{Host: "git.example.com", Owner: "acme", Repo: "api"}, URL: "https://git.example.com/acme/api"}
	if dir, err := findRevRepo(o, r); err != nil || dir != "" {
		t.Fatalf("no checkout: %q %v", dir, err)
	}
	if _, err := os.Stat(filepath.Join(root, "cache", "workspace", "code", "api")); !os.IsNotExist(err) {
		t.Fatalf("something was made in the workspace: %v", err)
	}

	ws := filepath.Join(root, "cache", "workspace", "code", "api")
	gitTest(t, root, "clone", "-q", filepath.Join(root, "remote.git"), ws)
	gitTest(t, ws, "remote", "set-url", "origin", "https://git.example.com/acme/api")
	if dir, err := findRevRepo(o, r); err != nil || dir != mustReal(t, ws) {
		t.Fatalf("workspace checkout: %q %v", dir, err)
	}

	code := filepath.Join(root, "code")
	mine := filepath.Join(code, "api")
	gitTest(t, root, "clone", "-q", filepath.Join(root, "remote.git"), mine)
	gitTest(t, mine, "remote", "set-url", "origin", "https://git.example.com/acme/api")
	o.codeRoot = code
	if dir, err := findRevRepo(o, r); err != nil || dir != mustReal(t, mine) {
		t.Fatalf("code directory checkout: %q %v", dir, err)
	}
}

func mustReal(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSplitRemoteRev(t *testing.T) {
	t.Setenv("GH_HOST", "")
	for _, tc := range []struct{ in, name, rev string }{
		{"https://github.com/acme/api#feature", "github.com/acme/api", "feature"},
		{"https://github.com/acme/api#main...feat/x", "github.com/acme/api", "main...feat/x"},
		{"https://gitlab.com/group/sub/api#fix", "gitlab.com/group/sub/api", "fix"},
	} {
		r, rev, ok := splitRemoteRev(tc.in)
		if !ok || r.Name() != tc.name || rev != tc.rev {
			t.Errorf("%s: %v %q %q", tc.in, ok, r.Name(), rev)
		}
	}
	for _, in := range []string{"https://github.com/acme/api", "https://github.com/acme/api#12", "acme/api#feature", "/src/api#feature", "https://github.com/acme/api#"} {
		if r, rev, ok := splitRemoteRev(in); ok {
			t.Errorf("%s: read as %s#%s", in, r.Name(), rev)
		}
	}
}

// A rev picked from a forge's list is fetched into a clone without files
// and reviewed there; fixes are refused for it.
func TestFetchRemoteRev(t *testing.T) {
	root, clone := revFixture(t)
	gitTest(t, clone, "push", "-q", "origin", "feature")
	cache := filepath.Join(root, "repos")
	r := revRepo{Ref: triage.PRRef{Host: "git.example.com", Owner: "acme", Repo: "api"}, URL: "file://" + filepath.ToSlash(filepath.Join(root, "remote.git"))}
	ctx := context.Background()
	var stages []string
	progress := func(s string, _, _ int) { stages = append(stages, s) }

	dir, local, err := fetchRemoteRev(ctx, cache, r, "feature", progress)
	if err != nil {
		t.Fatal(err)
	}
	if local != "origin/feature" || strings.Join(stages, ",") != "clonerev,fetchrev" {
		t.Fatalf("local %q, stages %v", local, stages)
	}
	if _, err := os.Stat(filepath.Join(dir, "base.txt")); !os.IsNotExist(err) {
		t.Fatalf("files were checked out: %v", err)
	}
	remoteRevRoot = cache
	t.Cleanup(func() { remoteRevRoot = "" })
	if !remoteRevDir(dir) || remoteRevDir(clone) {
		t.Fatal("remoteRevDir")
	}
	s, err := inspectLocal(ctx, dir+"#"+local)
	if err != nil {
		t.Fatal(err)
	}
	if s.info.Ahead != 2 || s.info.BaseRef != "main" || !strings.Contains(s.raw, "local.txt") {
		t.Fatalf("review: %+v\n%s", s.info, s.raw)
	}
	if err := fixable(&PRResult{PR: s.info}, "worktree"); err == nil || !strings.Contains(err.Error(), "forge") {
		t.Fatalf("fixable: %v", err)
	}

	// A new commit on origin is fetched on the next pick, and a range
	// and a commit name origin's refs and the commit.
	gitTest(t, clone, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "more")
	gitTest(t, clone, "push", "-q", "origin", "feature")
	stages = nil
	if _, local, err = fetchRemoteRev(ctx, cache, r, "main...feature", progress); err != nil || local != "origin/main...origin/feature" || strings.Join(stages, ",") != "fetchrev" {
		t.Fatalf("range: %q %v %v", local, err, stages)
	}
	head := gitTest(t, clone, "rev-parse", "HEAD")
	if got := gitTest(t, dir, "rev-parse", "origin/feature"); got != head {
		t.Fatalf("origin/feature at %s, want %s", got, head)
	}
	if _, local, err = fetchRemoteRev(ctx, cache, r, head, progress); err != nil || local != head {
		t.Fatalf("commit: %q %v", local, err)
	}
	for _, bad := range []string{"-x", "a b", "main...", "x:y"} {
		if _, _, err := fetchRemoteRev(ctx, cache, r, bad, progress); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

// A commit compared with another is the changes since their merge base,
// whichever side is a commit.
func TestInspectRevCommitVersusCommit(t *testing.T) {
	_, clone := revFixture(t)
	ctx := context.Background()
	first := gitTest(t, clone, "rev-parse", "main")
	pushed := gitTest(t, clone, "rev-parse", "feature~1")
	s, err := inspectLocal(ctx, clone+"#"+first+"..."+pushed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.raw, "pushed.txt") || strings.Contains(s.raw, "local.txt") || s.info.SingleCommit {
		t.Fatalf("commit vs commit:\n%s", s.raw)
	}
	if s.info.HeadRef != pushed[:10] || s.info.BaseRef != first[:10] || s.info.Ahead != 1 || s.info.BaseOid != first {
		t.Fatalf("info: %+v", s.info)
	}
	// A branch as the base of a commit.
	if s, err = inspectLocal(ctx, clone+"#main..."+pushed); err != nil || s.info.BaseRef != "main" || s.info.HeadRef != pushed[:10] {
		t.Fatalf("branch base: %v %+v", err, s)
	}
	if _, err := publishLocal(&PRResult{PR: s.info}, nil); err == nil {
		t.Fatal("published a commit")
	}
}

// A fork's branches are compared with its upstream: the upstream's
// default branch is fetched into the fork, and each branch's fork point
// is where it left it, whatever the upstream did since.
func TestForkComparesWithUpstream(t *testing.T) {
	root := t.TempDir()
	up := filepath.Join(root, "up.git")
	gitTest(t, root, "init", "-q", "--bare", "-b", "main", up)
	seed := filepath.Join(root, "seed")
	gitTest(t, root, "init", "-q", "-b", "main", seed)
	commit := func(dir, file string) string {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(file+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitTest(t, dir, "add", "-A")
		gitTest(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", file)
		return gitTest(t, dir, "rev-parse", "HEAD")
	}
	forked := commit(seed, "a.txt")
	gitTest(t, seed, "push", "-q", up, "main")

	fork := filepath.Join(root, "fork")
	gitTest(t, root, "clone", "-q", up, fork)
	gitTest(t, fork, "switch", "-q", "-c", "feature")
	commit(fork, "mine.txt")
	// The upstream moves on after the fork.
	upstreamNow := commit(seed, "theirs.txt")
	gitTest(t, seed, "push", "-q", up, "main")
	gitTest(t, fork, "remote", "add", "upstream", up)

	ctx := context.Background()
	p, err := localParent(ctx, fork)
	if err != nil || p == nil || p.Remote != "upstream" || p.Default != "main" {
		t.Fatalf("parent: %+v %v", p, err)
	}
	tok, err := fetchUpstream(ctx, fork, p, "main")
	if err != nil || tok != "upstream/main" {
		t.Fatalf("fetch: %q %v", tok, err)
	}
	revs, err := listRevs(fork)
	if err != nil {
		t.Fatal(err)
	}
	if err := revs.countFrom(tok); err != nil {
		t.Fatal(err)
	}
	for _, b := range revs.Branches {
		if b.Name == "feature" && (b.Fork != forked || b.Ahead != 1) {
			t.Fatalf("feature from upstream: %+v", b)
		}
	}
	if revs.Compared != "upstream/main" {
		t.Fatalf("compared: %q", revs.Compared)
	}
	if err := revs.countFrom("nope"); err == nil {
		t.Fatal("counted from a missing base")
	}

	s, err := inspectLocal(ctx, fork+"#upstream/main...feature")
	if err != nil {
		t.Fatal(err)
	}
	if s.info.BaseOid != forked || s.info.Behind != 1 || !strings.Contains(s.raw, "mine.txt") || strings.Contains(s.raw, "theirs.txt") {
		t.Fatalf("fork review: %+v\n%s", s.info, s.raw)
	}
	if _, err := publishLocal(&PRResult{PR: s.info}, nil); err == nil || !strings.Contains(err.Error(), "upstream") {
		t.Fatalf("publish into upstream: %v", err)
	}

	// An upstream commit is fetched by SHA and kept.
	sha, err := fetchUpstream(ctx, fork, p, upstreamNow)
	if err != nil || sha != upstreamNow {
		t.Fatalf("fetch commit: %q %v", sha, err)
	}
	if got := gitTest(t, fork, "rev-parse", "refs/pr-manager/upstream/"+upstreamNow); got != upstreamNow {
		t.Fatalf("kept ref: %s", got)
	}
	if _, err := fetchUpstream(ctx, fork, p, "-x"); err == nil {
		t.Fatal("fetched a bad branch")
	}

	// Without an upstream remote and with no forge to ask, it's no fork.
	lone := filepath.Join(root, "lone")
	gitTest(t, root, "init", "-q", lone)
	if p, err := localParent(ctx, lone); err != nil || p != nil {
		t.Fatalf("lone: %+v %v", p, err)
	}
}
