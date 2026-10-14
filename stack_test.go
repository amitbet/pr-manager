package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/triage"
)

// stackRepo is a checkout whose branch "second" is built on "first",
// both pushed: first adds a.txt to main, second adds b.txt on top.
func stackRepo(t *testing.T) (repo string, firstOid string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	gitTest(t, root, "init", "-q", "--bare", remote)
	repo = filepath.Join(root, "repo")
	gitTest(t, root, "init", "-q", "-b", "main", repo)
	gitTest(t, repo, "config", "user.name", "Test")
	gitTest(t, repo, "config", "user.email", "test@example.com")
	commit := func(name string) {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitTest(t, repo, "add", name)
		gitTest(t, repo, "commit", "-q", "-m", "add "+name)
	}
	commit("base.txt")
	gitTest(t, repo, "remote", "add", "origin", remote)
	gitTest(t, repo, "push", "-q", "-u", "origin", "main")
	gitTest(t, repo, "remote", "set-head", "origin", "main")
	gitTest(t, repo, "switch", "-q", "-c", "first")
	commit("a.txt")
	gitTest(t, repo, "push", "-q", "-u", "origin", "first")
	firstOid = gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "switch", "-q", "-c", "second")
	commit("b.txt")
	gitTest(t, repo, "push", "-q", "-u", "origin", "second")
	return repo, firstOid
}

// fakePRs replaces gh pr list with prs, filtered by --head.
func fakePRs(t *testing.T, prs ...openPR) {
	t.Helper()
	old := listOpenPRs
	t.Cleanup(func() { listOpenPRs = old })
	listOpenPRs = func(_ context.Context, _ *triage.PRInfo, args ...string) ([]openPR, error) {
		head := ""
		if i := slices.Index(args, "--head"); i >= 0 {
			head = args[i+1]
		}
		var out []openPR
		for _, o := range prs {
			if head == "" || o.HeadRefName == head {
				out = append(out, o)
			}
		}
		return out, nil
	}
}

func inspectSynced(t *testing.T, repo string) *triage.PRInfo {
	t.Helper()
	s, err := inspectLocal(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	s, ok, err := syncStack(context.Background(), s)
	if err != nil || !ok {
		t.Fatalf("syncStack: %v, %v", ok, err)
	}
	return s.info
}

func files(t *testing.T, repo string) []string {
	t.Helper()
	s, err := inspectLocal(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range s.src.Files {
		out = append(out, f.Path)
	}
	return out
}

// A branch without a PR, or with one into the default branch, is diffed
// from the default branch as before, with nothing written to its config.
func TestStackRegularBranch(t *testing.T) {
	repo, _ := stackRepo(t)
	gitTest(t, repo, "switch", "-q", "first")
	fakePRs(t)
	p := inspectSynced(t, repo)
	if p.BaseRef != "main" || p.StackedOn != "" || p.URL != "" || len(p.StackCandidates) != 0 {
		t.Fatalf("no PR: %+v", p)
	}
	fakePRs(t, openPR{URL: "https://github.com/o/r/pull/1", Number: 1, HeadRefName: "first", BaseRefName: "main"})
	p = inspectSynced(t, repo)
	if p.BaseRef != "main" || p.StackedOn != "" || p.URL != "https://github.com/o/r/pull/1" {
		t.Fatalf("PR into main: %+v", p)
	}
	if _, err := triage.Git(repo, "config", "--get", stackConfig("first")); err == nil {
		t.Fatal("a branch PR'd into main got a stack base")
	}
	// A setting naming a branch origin doesn't have is ignored.
	gitTest(t, repo, "config", stackConfig("first"), "gone")
	if got := files(t, repo); !slices.Equal(got, []string{"a.txt"}) {
		t.Fatalf("stale stack base: %v", got)
	}
}

// A branch whose PR targets another branch is diffed from that branch,
// and every later inspection agrees, so publish and fix see the same
// diff; retargeting the PR to main puts it back.
func TestStackFollowsPRBase(t *testing.T) {
	repo, _ := stackRepo(t)
	first := openPR{URL: "https://github.com/o/r/pull/1", Number: 1, HeadRefName: "first", BaseRefName: "main"}
	second := openPR{URL: "https://github.com/o/r/pull/2", Number: 2, HeadRefName: "second", BaseRefName: "first"}
	fakePRs(t, first, second)
	if got := files(t, repo); !slices.Equal(got, []string{"a.txt", "b.txt"}) {
		t.Fatalf("before sync: %v", got)
	}
	p := inspectSynced(t, repo)
	if p.BaseRef != "first" || p.StackedOn != "first" || p.DefaultBase != "main" || p.URL != second.URL || p.StackPR != first.URL || p.Ahead != 1 {
		t.Fatalf("stacked: %+v", p)
	}
	if got := files(t, repo); !slices.Equal(got, []string{"b.txt"}) {
		t.Fatalf("after sync: %v", got)
	}
	second.BaseRefName = "main"
	fakePRs(t, first, second)
	p = inspectSynced(t, repo)
	if p.BaseRef != "main" || p.StackedOn != "" || p.StackPR != "" {
		t.Fatalf("retargeted: %+v", p)
	}
	if got := files(t, repo); !slices.Equal(got, []string{"a.txt", "b.txt"}) {
		t.Fatalf("after retarget: %v", got)
	}
}

// A branch without a PR is offered the unmerged PRs it builds on, and
// is stacked only when asked to.
func TestStackCandidates(t *testing.T) {
	repo, firstOid := stackRepo(t)
	first := openPR{URL: "https://github.com/o/r/pull/1", Number: 1, Title: "First", HeadRefName: "first", HeadRefOid: firstOid, BaseRefName: "main"}
	other := openPR{URL: "https://github.com/o/r/pull/3", Number: 3, HeadRefName: "other", HeadRefOid: strings.Repeat("0", 40), BaseRefName: "main"}
	fakePRs(t, first, other)
	p := inspectSynced(t, repo)
	if p.StackedOn != "" || len(p.StackCandidates) != 1 || p.StackCandidates[0].Branch != "first" || p.StackCandidates[0].Number != 1 {
		t.Fatalf("candidates: %+v", p)
	}
	if err := setStackBase(repo, "second", "first"); err != nil {
		t.Fatal(err)
	}
	p = inspectSynced(t, repo)
	if p.StackedOn != "first" || p.StackPR != first.URL || len(p.StackCandidates) != 0 {
		t.Fatalf("stacked by choice: %+v", p)
	}
	if err := setStackBase(repo, "second", ""); err != nil {
		t.Fatal(err)
	}
	if err := setStackBase(repo, "second", ""); err != nil {
		t.Fatalf("unstacking twice: %v", err)
	}
	if p = inspectSynced(t, repo); p.StackedOn != "" || p.BaseRef != "main" {
		t.Fatalf("unstacked: %+v", p)
	}
	// Once first is merged, it isn't offered.
	gitTest(t, repo, "push", "-q", "origin", "first:main")
	gitTest(t, repo, "fetch", "-q", "origin")
	if p = inspectSynced(t, repo); len(p.StackCandidates) != 0 {
		t.Fatalf("merged candidate offered: %+v", p.StackCandidates)
	}
}

// The fix command's -base wins over a stack.
func TestStackBaseOverride(t *testing.T) {
	repo, _ := stackRepo(t)
	gitTest(t, repo, "config", stackConfig("second"), "first")
	baseOverride = "origin/main"
	t.Cleanup(func() { baseOverride = "" })
	if got := files(t, repo); !slices.Equal(got, []string{"a.txt", "b.txt"}) {
		t.Fatalf("with -base: %v", got)
	}
}

// githubOrigin points repo's origin at github.com/o/r, over an ssh that
// serves the test's bare remote, so the checkout is a GitHub one to
// publish but nothing leaves the machine.
func githubOrigin(t *testing.T, repo string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ssh is a shell script")
	}
	bare := filepath.Join(filepath.Dir(repo), "remote.git")
	ssh := filepath.Join(t.TempDir(), "ssh")
	script := "#!/bin/sh\nfor a; do cmd=\"$a\"; done\ncase \"$cmd\" in\n" +
		"git-upload-pack*) exec git-upload-pack '" + bare + "';;\n" +
		"git-receive-pack*) exec git-receive-pack '" + bare + "';;\nesac\nexit 1\n"
	if err := os.WriteFile(ssh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_COMMAND", ssh)
	t.Setenv("GIT_SSH_VARIANT", "simple")
	gitTest(t, repo, "remote", "set-url", "origin", "ssh://git@github.com/o/r.git")
}

// publishGH is a gh on PATH for publishLocal: pr list answers with the
// URL in its existing file, pr create opens pull/9, pr view says "old",
// and create and edit keep the body they are given.
type publishGH struct{ dir string }

func fakePublishGH(t *testing.T) publishGH {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake gh is a shell script")
	}
	g := publishGH{t.TempDir()}
	script := "#!/bin/sh\ncd '" + g.dir + "'\necho \"$@\" >> calls\ncase \"$1 $2\" in\n" +
		"'pr list') cat existing 2>/dev/null;;\n" +
		"'pr create') cat > body; echo https://github.com/o/r/pull/9;;\n" +
		"'pr view') echo old;;\n" +
		"'pr edit') cat > body;;\n" +
		"*) echo 'fake gh: no' >&2; exit 1;;\nesac\n"
	if err := os.WriteFile(filepath.Join(g.dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", g.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return g
}

func (g publishGH) read(t *testing.T, name string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(g.dir, name))
	return string(b)
}

func (g publishGH) reset(t *testing.T, existing string) {
	t.Helper()
	for _, f := range []string{"calls", "body", "existing"} {
		os.Remove(filepath.Join(g.dir, f))
	}
	if existing != "" {
		if err := os.WriteFile(filepath.Join(g.dir, "existing"), []byte(existing+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A stacked branch's PR is opened into the branch it is stacked on, its
// description says what it is stacked on, and a result triaged from
// another base isn't published. A regular branch's PR is opened into the
// default branch, with the description as it is.
func TestStackPublish(t *testing.T) {
	repo, firstOid := stackRepo(t)
	githubOrigin(t, repo)
	gh := fakePublishGH(t)
	first := openPR{URL: "https://github.com/o/r/pull/1", Number: 1, HeadRefName: "first", HeadRefOid: firstOid, BaseRefName: "main"}
	fakePRs(t, first)
	describe := func(*triage.PRInfo) string { return "What it does" }

	unstacked := inspectSynced(t, repo)
	if err := setStackBase(repo, "second", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := publishLocal(&PRResult{PR: unstacked}, describe); err == nil || !strings.Contains(err.Error(), "changed since triage") {
		t.Fatalf("published a result triaged from main onto first: %v", err)
	}
	if calls := gh.read(t, "calls"); strings.Contains(calls, "pr create") {
		t.Fatalf("gh was asked to create: %s", calls)
	}

	gh.reset(t, "")
	p := inspectSynced(t, repo)
	out, err := publishLocal(&PRResult{PR: p}, describe)
	if err != nil {
		t.Fatal(err)
	}
	if out.URL != "https://github.com/o/r/pull/9" || out.Updated || out.Pushed {
		t.Fatalf("create: %+v", out)
	}
	if calls := gh.read(t, "calls"); !strings.Contains(calls, "pr create --base first --head second --fill") {
		t.Fatalf("create calls: %s", calls)
	}
	want := "Stacked on " + first.URL + "\n\nWhat it does"
	if body := gh.read(t, "body"); body != want {
		t.Fatalf("create body = %q, want %q", body, want)
	}

	// Its PR, opened since, gets the same description.
	gh.reset(t, "https://github.com/o/r/pull/2")
	out, err = publishLocal(&PRResult{PR: p}, describe)
	if err != nil {
		t.Fatal(err)
	}
	if out.URL != "https://github.com/o/r/pull/2" || !out.Updated || out.Previous != "old" {
		t.Fatalf("update: %+v", out)
	}
	calls := gh.read(t, "calls")
	if !strings.Contains(calls, "--head second --base first") || !strings.Contains(calls, "pr edit https://github.com/o/r/pull/2") {
		t.Fatalf("update calls: %s", calls)
	}
	if body := gh.read(t, "body"); body != want {
		t.Fatalf("update body = %q, want %q", body, want)
	}

	gh.reset(t, "")
	gitTest(t, repo, "switch", "-q", "first")
	fakePRs(t)
	if out, err = publishLocal(&PRResult{PR: inspectSynced(t, repo)}, describe); err != nil || out.URL != "https://github.com/o/r/pull/9" {
		t.Fatalf("regular create: %+v, %v", out, err)
	}
	if calls := gh.read(t, "calls"); !strings.Contains(calls, "pr create --base main --head first --fill") {
		t.Fatalf("regular create calls: %s", calls)
	}
	if body := gh.read(t, "body"); body != "What it does" {
		t.Fatalf("regular create body = %q", body)
	}
}

// The stack endpoint stacks a branch only on a PR it was offered, and
// not one that has a PR, a commit's result, or a checkout since switched.
func TestStackEndpoint(t *testing.T) {
	repo, firstOid := stackRepo(t)
	fakePRs(t, openPR{URL: "https://github.com/o/r/pull/1", Number: 1, HeadRefName: "first", HeadRefOid: firstOid, BaseRefName: "main"})
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	tr.stackRoutes(mux)
	r := &PRResult{Key: "local__stack", PR: inspectSynced(t, repo)}
	save := func() {
		t.Helper()
		if err := tr.saveResult(r); err != nil {
			t.Fatal(err)
		}
	}
	save()
	post := func(key, body string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/local/"+key+"/stack", strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}
	stacked := func() string {
		out, _ := triage.Git(repo, "config", "--get", stackConfig("second"))
		return strings.TrimSpace(out)
	}
	refused := func(what, key, body, msg string) {
		t.Helper()
		if code, out := post(key, body); code/100 != 4 || !strings.Contains(out, msg) {
			t.Fatalf("%s: %d %s", what, code, out)
		}
		if s := stacked(); s != "" {
			t.Fatalf("%s: stacked on %q", what, s)
		}
	}

	refused("unknown result", "local__none", `{"base":"first"}`, "")
	refused("not offered", r.Key, `{"base":"main"}`, "not a branch this one can be stacked on")
	refused("option-like", r.Key, `{"base":"--help"}`, "not a branch this one can be stacked on")
	gitTest(t, repo, "switch", "-q", "first")
	refused("switched", r.Key, `{"base":"first"}`, "another branch")
	gitTest(t, repo, "switch", "-q", "second")

	if code, out := post(r.Key, `{"base":"first"}`); code != 200 || !strings.Contains(out, `"path":"`+r.PR.LocalPath+`"`) {
		t.Fatalf("stack: %d %s", code, out)
	}
	if s := stacked(); s != "first" {
		t.Fatalf("stacked on %q", s)
	}
	if code, out := post(r.Key, `{"base":""}`); code != 200 {
		t.Fatalf("unstack: %d %s", code, out)
	}
	if s := stacked(); s != "" {
		t.Fatalf("unstacked, still on %q", s)
	}

	r.PR.URL = "https://github.com/o/r/pull/2"
	save()
	refused("has a PR", r.Key, `{"base":"first"}`, "has a PR already")
	r.PR.URL, r.PR.Rev = "", "HEAD"
	save()
	refused("a commit", r.Key, `{"base":"first"}`, "not a local branch result")
}

// A cached result of a local branch gets the PR lookups of the triage
// that reuses it, saved, and keeps a PR link a lookup misses.
func TestStackCacheHit(t *testing.T) {
	repo, firstOid := stackRepo(t)
	first := openPR{URL: "https://github.com/o/r/pull/1", Number: 1, HeadRefName: "first", HeadRefOid: firstOid, BaseRefName: "main"}
	second := openPR{URL: "https://github.com/o/r/pull/2", Number: 2, HeadRefName: "second", BaseRefName: "main"}
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s, err := inspectLocal(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	key := localCacheKey(s, tr.options(jobOptions{}))
	if err := tr.saveResult(&PRResult{Key: key, PromptVersion: triage.PromptVersion, PR: s.info}); err != nil {
		t.Fatal(err)
	}
	run := func(what string) (got, saved *triage.PRInfo) {
		t.Helper()
		r, err := tr.RunLocal(context.Background(), repo, jobOptions{}, func(string, int, int) {})
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if r.Key != key {
			t.Fatalf("%s: triaged again as %s", what, r.Key)
		}
		l, err := tr.Load(key)
		if err != nil {
			t.Fatal(err)
		}
		return r.PR, l.PR
	}

	fakePRs(t, first)
	got, saved := run("first opened")
	for _, p := range []*triage.PRInfo{got, saved} {
		if p.URL != "" || len(p.StackCandidates) != 1 || p.StackCandidates[0].URL != first.URL {
			t.Fatalf("first opened: %+v", p)
		}
	}
	fakePRs(t, first, second)
	got, saved = run("second opened")
	for _, p := range []*triage.PRInfo{got, saved} {
		if p.URL != second.URL || len(p.StackCandidates) != 0 || p.StackedOn != "" {
			t.Fatalf("second opened: %+v", p)
		}
	}
	// gh failing, or not seeing the PR, doesn't lose it, nor offer a
	// stack to a branch that has one.
	old := listOpenPRs
	listOpenPRs = func(context.Context, *triage.PRInfo, ...string) ([]openPR, error) { return nil, errors.New("offline") }
	if got, saved = run("offline"); got.URL != second.URL || saved.URL != second.URL {
		t.Fatalf("offline: %+v, saved %+v", got, saved)
	}
	listOpenPRs = old
	fakePRs(t, first)
	if got, saved = run("missed"); got.URL != second.URL || saved.URL != second.URL || len(got.StackCandidates) != 0 || len(saved.StackCandidates) != 0 {
		t.Fatalf("missed: %+v, saved %+v", got, saved)
	}
}
