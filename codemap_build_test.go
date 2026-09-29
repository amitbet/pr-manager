package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/amitbet/pr-manager/codemap"
	"github.com/amitbet/pr-manager/triage"
)

func gitInit(t *testing.T, dir, remote string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
		if remote == "" && args[0] == "remote" {
			continue
		}
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

func TestScanCheckouts(t *testing.T) {
	root := t.TempDir()
	gitInit(t, filepath.Join(root, "api"), "git@github.com:acme/api.git")
	gitInit(t, filepath.Join(root, "acme", "billing-checkout"), "https://ghe.corp.example/acme/billing")
	gitInit(t, filepath.Join(root, "scratch"), "")
	gitInit(t, filepath.Join(root, "a", "b", "too-deep"), "")
	gitInit(t, filepath.Join(root, "node_modules", "dep"), "")

	got, err := scanCheckouts(root)
	if err != nil {
		t.Fatal(err)
	}
	real := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }
	want := map[string]string{
		"github.com/acme/api":           real(filepath.Join(root, "api")),
		"ghe.corp.example/acme/billing": real(filepath.Join(root, "acme", "billing-checkout")), // named by its remote
		"local/scratch":                 real(filepath.Join(root, "scratch")),
	}
	if len(got) != len(want) {
		t.Errorf("found %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k].Dir != v {
			t.Errorf("%s: %s, want %s", k, got[k].Dir, v)
		}
	}
	if r := got["ghe.corp.example/acme/billing"].Ref; r.Host != "ghe.corp.example" || r.Owner != "acme" || r.Repo != "billing" {
		t.Errorf("billing ref %+v", r)
	}
	if _, err := scanCheckouts(filepath.Join(root, "missing")); err == nil {
		t.Error("a missing directory scanned without an error")
	}
}

func TestAddRepoLinksLocalCheckout(t *testing.T) {
	root, ws := t.TempDir(), t.TempDir()
	gitInit(t, filepath.Join(root, "api"), "git@github.com:acme/api.git")
	if err := os.MkdirAll(filepath.Join(ws, "code"), 0o755); err != nil {
		t.Fatal(err)
	}
	local, _ := scanCheckouts(root)
	api := triage.PRRef{Owner: "acme", Repo: "api"}
	src := local["github.com/acme/api"].Dir
	if name, err := addRepo(context.Background(), ws, api, local); err != nil || name != "api" {
		t.Fatalf("addRepo = %q %v", name, err)
	}
	if dst, err := os.Readlink(filepath.Join(ws, "code", "api")); err != nil || dst != src {
		t.Errorf("link %s %v, want %s", dst, err, src)
	}
	if name, err := addRepo(context.Background(), ws, api, local); err != nil || name != "api" {
		t.Errorf("linking again: %q %v", name, err)
	}
	if _, err := addRepo(context.Background(), ws, triage.PRRef{Repo: "other"}, local); err == nil {
		t.Error("a repo neither local nor in an org was added")
	}
}

// A link that reads back spelled differently from the checkout (a
// junction on Windows reads back normalized) still counts as linking it,
// a link to a checkout that is gone is replaced, and a link that cannot
// be made is a linkError the run goes on without.
func TestAddRepoRelinks(t *testing.T) {
	root, ws := t.TempDir(), t.TempDir()
	gitInit(t, filepath.Join(root, "api"), "git@github.com:acme/api.git")
	if err := os.MkdirAll(filepath.Join(root, "elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	code := filepath.Join(ws, "code")
	if err := os.MkdirAll(code, 0o755); err != nil {
		t.Fatal(err)
	}
	local, _ := scanCheckouts(root)
	api := triage.PRRef{Owner: "acme", Repo: "api"}
	src := local["github.com/acme/api"].Dir
	dir := filepath.Join(code, "api")

	if err := os.Symlink(filepath.Join(src, "."+string(filepath.Separator)), dir); err != nil {
		t.Skip("no symlinks here:", err)
	}
	if !linksTo(dir, src) {
		t.Error("a link to the checkout by another spelling is not a link to it")
	}
	if linksTo(dir, filepath.Join(root, "elsewhere")) || linksTo(src, src) {
		t.Error("linksTo matched a link elsewhere, or a directory that is not a link")
	}

	// A link to a checkout that is gone.
	gone := filepath.Join(root, "gone")
	gitInit(t, gone, "")
	_ = os.Remove(dir)
	if err := os.Symlink(gone, dir); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(gone)
	if name, err := addRepo(context.Background(), ws, api, local); err != nil || name != "api" || !linksTo(dir, src) {
		t.Errorf("stale link not replaced: %q %v", name, err)
	}

	// A workspace the link cannot be written to. Windows does not make a
	// directory read-only by its mode, nor does Unix for root.
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	_ = os.Remove(dir)
	if err := os.Chmod(code, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(code, 0o755)
	_, err := addRepo(context.Background(), ws, api, local)
	var le *linkError
	if !errors.As(err, &le) {
		t.Fatalf("addRepo into a read-only workspace = %v, want a linkError", err)
	}
	if skipUnlinked(err) != nil {
		t.Error("a link failure stopped the run")
	}
	if other := errors.New("clone failed"); skipUnlinked(other) != other {
		t.Error("skipUnlinked swallowed another error")
	}
}

// Adding a repo rebuilds the map, and that must not change the settings
// every run of every other repo is found by.
func TestCodeMapSettingsIgnoresRebuilds(t *testing.T) {
	a := &codemap.Map{Meta: codemap.Meta{Version: 1, GeneratedAt: "2026-09-01T00:00:00Z", Repos: map[string]codemap.RepoMeta{"api": {}}}}
	b := &codemap.Map{Meta: codemap.Meta{Version: 1, GeneratedAt: "2026-09-30T00:00:00Z", Repos: map[string]codemap.RepoMeta{"api": {}, "web": {}}}}
	if codeMapSettings(a) != codeMapSettings(b) {
		t.Errorf("a rebuild changed the settings: %s, %s", codeMapSettings(a), codeMapSettings(b))
	}
	if codeMapSettings(nil) == codeMapSettings(a) {
		t.Error("with and without a map share settings")
	}
	if codeMapVersion(a) == codeMapVersion(b) {
		t.Error("the build version did not change with the build")
	}
}

// Two repos with the same name get a directory each, and each is found
// in the map by who it is.
func TestSameNamedReposInWorkspaceAndMap(t *testing.T) {
	root, ws := t.TempDir(), t.TempDir()
	gitInit(t, filepath.Join(root, "other", "api"), "git@github.com:other/api.git")
	gitInit(t, filepath.Join(root, "acme", "api"), "https://github.com/acme/api.git")
	gitInit(t, filepath.Join(root, "g", "r"), "git@gitlab.com:g/sub/r.git")
	if err := os.MkdirAll(filepath.Join(ws, "code"), 0o755); err != nil {
		t.Fatal(err)
	}
	local, err := scanCheckouts(root)
	if err != nil || len(local) != 3 {
		t.Fatalf("scanCheckouts = %v %v", local, err)
	}
	other := triage.PRRef{Owner: "other", Repo: "api"}
	acme := triage.PRRef{Owner: "acme", Repo: "api"}
	sub := local["gitlab.com/g/sub/r"].Ref
	// In order: the first repo added gets the bare name.
	for _, c := range []struct {
		ref  triage.PRRef
		want string
	}{{other, "api"}, {acme, "acme__api"}, {sub, "r"}} {
		name, err := addRepo(context.Background(), ws, c.ref, local)
		if err != nil || name != c.want {
			t.Errorf("addRepo(%+v) = %q %v, want %q", c.ref, name, err, c.want)
		}
	}
	// Adding again keeps each where it is.
	if name, _ := addRepo(context.Background(), ws, acme, local); name != "acme__api" {
		t.Errorf("acme/api moved to %q", name)
	}
	if name := workspaceName(ws, triage.PRRef{Owner: "g/other", Repo: "r"}, ""); name != "g%2Fother__r" {
		t.Errorf("subgroup repo dir %q", name)
	}

	m := &codemap.Map{Meta: codemap.Meta{Repos: map[string]codemap.RepoMeta{
		"api":       {Remote: "github.com/other/api"},
		"acme__api": {Remote: "github.com/acme/api"},
	}}}
	if !codeMapHas(m, ws, acme) || codeMapRepo(m, acme) != "acme__api" {
		t.Errorf("acme/api: has %v, repo %q", codeMapHas(m, ws, acme), codeMapRepo(m, acme))
	}
	if !codeMapHas(m, ws, other) || codeMapRepo(m, other) != "api" {
		t.Errorf("other/api: has %v, repo %q", codeMapHas(m, ws, other), codeMapRepo(m, other))
	}
	third := triage.PRRef{Owner: "third", Repo: "api"}
	if codeMapHas(m, ws, third) || codeMapRepo(m, third) == "api" {
		t.Errorf("third/api used other/api's map: repo %q", codeMapRepo(m, third))
	}

	// A map from before identities were recorded: "api" is trusted only
	// while its workspace directory is not a checkout of another repo.
	legacy := &codemap.Map{Meta: codemap.Meta{Repos: map[string]codemap.RepoMeta{"api": {}, "gone": {}}}}
	if !codeMapHas(legacy, ws, other) {
		t.Error("legacy map: other/api not found by name")
	}
	if codeMapHas(legacy, ws, acme) {
		t.Error("legacy map: acme/api matched other/api's directory")
	}
	if !codeMapHas(legacy, ws, triage.PRRef{Owner: "acme", Repo: "gone"}) {
		t.Error("legacy map: a repo not in this workspace was not found by name")
	}
}

func TestParseOrg(t *testing.T) {
	t.Setenv("GH_HOST", "")
	for in, want := range map[string][2]string{
		"acme":                              {"", "acme"},
		"https://github.com/acme/":          {"", "acme"},
		"ghe.corp.example/platform":         {"ghe.corp.example", "platform"},
		"https://ghe.corp.example/platform": {"ghe.corp.example", "platform"},
	} {
		h, o, err := parseOrg(in)
		if err != nil || [2]string{h, o} != want {
			t.Errorf("parseOrg(%q) = %q %q %v, want %v", in, h, o, err, want)
		}
	}
	for _, bad := range []string{"", "acme/api", "https://github.com/acme/api"} {
		if _, _, err := parseOrg(bad); err == nil {
			t.Errorf("parseOrg(%q) accepted", bad)
		}
	}
}
