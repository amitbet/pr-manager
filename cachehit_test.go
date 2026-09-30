package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

// fakeGH puts a gh on PATH that answers pr view with view, fails
// everything else, and logs each call's arguments to the returned file.
func fakeGH(t *testing.T, view string) (calls string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake gh is a shell script")
	}
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	if err := os.WriteFile(filepath.Join(dir, "view.json"), []byte(view), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$@\" >> '" + calls + "'\n" +
		"if [ \"$1 $2\" = \"pr view\" ]; then cat '" + filepath.Join(dir, "view.json") + "'; exit 0; fi\n" +
		"echo 'fake gh: no' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// cachedOpenPR saves a result for head000000 of acme/web#7, open against
// main.
func cachedOpenPR(t *testing.T, tr *triager) *PRResult {
	t.Helper()
	r := saveRun(t, tr, "acme__web__7__head000000__cfg", "base1", "head000000", time.Hour)
	r.PromptVersion = triage.PromptVersion
	r.PR.State, r.PR.Title, r.PR.BaseRef, r.PR.URL = "OPEN", "Old title", "main", "https://github.com/acme/web/pull/7"
	if err := tr.saveResult(r); err != nil {
		t.Fatal(err)
	}
	return r
}

// view is gh pr view's answer for head000000 of acme/web#7.
func view(state, base, mergedAt string) string {
	return fmt.Sprintf(`{"url":"https://github.com/acme/web/pull/7","title":"New title","author":{"login":"dev"},"state":"%s","headRefOid":"head000000","baseRefName":"%s","headRefName":"feature","additions":1,"deletions":0,"mergedAt":"%s"}`, state, base, mergedAt)
}

// A cached result gets the PR's current state and title, saved, since a
// PR is merged or renamed without a new push.
func TestCacheHitRefreshesPRDetails(t *testing.T) {
	calls := fakeGH(t, view("MERGED", "main", "2026-09-01T00:00:00Z"))
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cached := cachedOpenPR(t, tr)
	ref := triage.PRRef{Owner: "acme", Repo: "web", Number: 7}
	got, err := tr.Run(context.Background(), ref, jobOptions{}, func(string, int, int) {})
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != cached.Key {
		t.Fatalf("Run = %s, want the cached %s", got.Key, cached.Key)
	}
	if got.PR.State != "MERGED" || got.PR.Title != "New title" || got.PR.MergedAt == "" || got.PR.HeadRef != "feature" {
		t.Errorf("returned PR = %+v, want the resolved state and title", got.PR)
	}
	if got.PR.BaseOid != "base1" {
		t.Errorf("base commit = %q; the saved diff's must stay", got.PR.BaseOid)
	}
	saved, err := tr.Load(cached.Key)
	if err != nil {
		t.Fatal(err)
	}
	if saved.PR.State != "MERGED" || saved.PR.Title != "New title" {
		t.Errorf("saved PR = %s %q, want MERGED \"New title\"", saved.PR.State, saved.PR.Title)
	}
	if b, _ := os.ReadFile(calls); strings.Contains(string(b), "repo clone") {
		t.Error("a cache hit fetched the PR")
	}
}

// A PR retargeted to another branch without a push is triaged again: the
// saved diff is against the old base.
func TestCacheMissWhenBaseBranchChanged(t *testing.T) {
	calls := fakeGH(t, view("OPEN", "release", ""))
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cached := cachedOpenPR(t, tr)
	ref := triage.PRRef{Owner: "acme", Repo: "web", Number: 7}
	if _, err := tr.latestCached(ref, "head000000", "release"); err == nil {
		t.Error("latestCached reused a result diffed against main for a PR on release")
	}
	if _, err := tr.latestCached(ref, "head000000", "main"); err != nil {
		t.Errorf("latestCached on the same base: %v", err)
	}
	// The fake gh cannot clone, so the re-run stops at the fetch.
	if _, err := tr.Run(context.Background(), ref, jobOptions{}, func(string, int, int) {}); err == nil {
		t.Fatal("Run returned the result diffed against the old base")
	}
	if b, _ := os.ReadFile(calls); !strings.Contains(string(b), "repo clone") {
		t.Errorf("Run did not fetch the PR again; gh calls:\n%s", b)
	}
	if saved, err := tr.Load(cached.Key); err != nil || saved.PR.BaseRef != "main" {
		t.Errorf("the stale result was changed before the re-run: %v", err)
	}
}
