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
// directory, else to a workspace clone the picker makes.
func TestResolveRevPathClones(t *testing.T) {
	root, _ := revFixture(t)
	cache := filepath.Join(root, "cache")
	t.Setenv("PR_MANAGER_WORKSPACE", "")
	o := options{cache: cache}

	// Not GitHub, so git clones the URL; a file URL stands in for one.
	remote := "file://" + filepath.ToSlash(filepath.Join(root, "remote.git"))
	r := revRepo{Ref: triage.PRRef{Host: "git.example.com", Owner: "acme", Repo: "api"}, URL: remote}
	dir, dest, err := findRevRepo(o, r)
	if err != nil || dir != "" || filepath.Base(dest) != "api" {
		t.Fatalf("before cloning: %q %q %v", dir, dest, err)
	}
	if err := cloneRevRepo(context.Background(), r, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), ".api.cloning")); !os.IsNotExist(err) {
		t.Fatalf("temporary clone left: %v", err)
	}
	revs, err := listRevs(dest)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range revs.Branches {
		found = found || b.Name == "origin/feature"
	}
	if !found {
		t.Fatalf("clone lists %+v", revs.Branches)
	}

	// The clone's origin is the file URL, not git.example.com, so it isn't
	// taken for that repository; one whose origin matches is.
	gitTest(t, dest, "remote", "set-url", "origin", "https://git.example.com/acme/api")
	dir, _, err = findRevRepo(o, r)
	if want, _ := filepath.EvalSymlinks(dest); err != nil || dir != want {
		t.Fatalf("existing clone: %q %v, want %q", dir, err, want)
	}

	// A checkout in the code directory comes first.
	code := filepath.Join(root, "code")
	mine := filepath.Join(code, "api")
	gitTest(t, root, "clone", "-q", filepath.Join(root, "remote.git"), mine)
	gitTest(t, mine, "remote", "set-url", "origin", "https://git.example.com/acme/api")
	o.codeRoot = code
	dir, _, err = findRevRepo(o, r)
	if want, _ := filepath.EvalSymlinks(mine); err != nil || dir != want {
		t.Fatalf("code directory checkout: %q %v, want %q", dir, err, want)
	}
}
