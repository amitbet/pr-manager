package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/triage"
)

// TestPRFileReadsTheFixCheckout: a fixed result's head side is the fix
// checkout, with its edits and the files it added; the base side stays the
// base commit, and a cleaned-up checkout falls back to the PR head. The
// temp dirs sit under /var on macOS, a symlink, so this also covers
// reading through an unresolved checkout root.
func TestPRFileReadsTheFixCheckout(t *testing.T) {
	cache := t.TempDir()
	tr, err := newTriager(options{cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	rv, err := newReviews(options{cache: cache}, tr.fetcher)
	if err != nil {
		t.Fatal(err)
	}
	ref := triage.PRRef{Owner: "acme", Repo: "web", Number: 7}
	repo := tr.fetcher.RepoDir(ref)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "init", "-b", "main")
	gitTest(t, repo, "config", "user.name", "Test")
	gitTest(t, repo, "config", "user.email", "test@example.com")
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(repo, "x.go", "base\n")
	gitTest(t, repo, "add", "x.go")
	gitTest(t, repo, "commit", "-m", "base")
	base := gitTest(t, repo, "rev-parse", "HEAD")
	write(repo, "x.go", "original\n")
	gitTest(t, repo, "commit", "-am", "head")
	head := gitTest(t, repo, "rev-parse", "HEAD")

	fix := t.TempDir()
	write(fix, "x.go", "fixed\n")
	write(fix, "y.go", "added by the fix\n")
	r := &PRResult{
		Key:         "acme__web__7__fix",
		PR:          &triage.PRInfo{PRRef: ref, BaseOid: base, HeadOid: head},
		LocalFixDir: fix,
	}
	b, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(tr.results, r.Key+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	rv.routes(mux, tr)

	get := func(path, side string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		url := "/api/prs/github.com/acme/web/7/file?key=" + r.Key + "&path=" + path + "&side=" + side
		mux.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
		if rec.Code != 200 {
			return rec.Code, rec.Body.String()
		}
		var out struct{ Lines []string }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return 200, strings.Join(out.Lines, "\n")
	}
	for _, c := range []struct{ path, side, want string }{
		{"x.go", "head", "fixed"},
		{"y.go", "head", "added by the fix"},
		{"x.go", "base", "base"},
	} {
		if code, got := get(c.path, c.side); code != 200 || got != c.want {
			t.Errorf("%s %s: got %d %q, want %q", c.side, c.path, code, got, c.want)
		}
	}
	if code, _ := get("../x.go", "head"); code != 400 {
		t.Errorf("an unsafe path read from the fix checkout: %d", code)
	}

	if err := os.RemoveAll(fix); err != nil {
		t.Fatal(err)
	}
	if code, got := get("x.go", "head"); code != 200 || got != "original" {
		t.Errorf("cleaned-up fix checkout: got %d %q, want the PR head", code, got)
	}
}
