package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := triage.Git(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func TestInspectLocalIncludesWorkingTreeWithoutChangingIndex(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	gitTest(t, root, "init", "--bare", remote)
	repo := filepath.Join(root, "repo")
	gitTest(t, root, "init", "-b", "main", repo)
	gitTest(t, repo, "config", "user.name", "Test")
	gitTest(t, repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("one\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", "file.txt")
	gitTest(t, repo, "commit", "-m", "initial")
	gitTest(t, repo, "remote", "add", "origin", remote)
	gitTest(t, repo, "push", "-u", "origin", "main")
	gitTest(t, repo, "switch", "-c", "feature")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("one\ntwo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", "file.txt")
	gitTest(t, repo, "commit", "-m", "feature change")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("one\ntwo\nthree\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new\n"), 0644); err != nil {
		t.Fatal(err)
	}
	before := gitTest(t, repo, "ls-files", "--stage")
	snap, err := inspectLocal(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if snap.info.Ahead != 1 || snap.info.Behind != 0 || !snap.info.Uncommitted {
		t.Fatalf("distance: %+v", snap.info)
	}
	if len(snap.src.Files) != 2 || !strings.Contains(snap.raw, "+three") || !strings.Contains(snap.raw, "new.txt") {
		t.Fatalf("diff: %s", snap.raw)
	}
	after := gitTest(t, repo, "ls-files", "--stage")
	if before != after {
		t.Fatal("inspection changed the repository index")
	}
	if _, err := publishLocal(&PRResult{PR: snap.info}, nil); err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("publish error: %v", err)
	}
	// A fix trusts a triage whose changes were committed since: the
	// snapshot is the change from the base, wherever it lives.
	if dirty, err := hasUncommitted(repo); err != nil || !dirty {
		t.Fatalf("hasUncommitted before commit: %v, %v", dirty, err)
	}
	gitTest(t, repo, "add", "-A")
	gitTest(t, repo, "commit", "-m", "the rest")
	committed, err := inspectLocal(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if committed.info.SnapshotHash != snap.info.SnapshotHash || committed.info.HeadOid == snap.info.HeadOid || committed.info.Uncommitted {
		t.Fatalf("after commit: %+v", committed.info)
	}
	if dirty, err := hasUncommitted(repo); err != nil || dirty {
		t.Fatalf("hasUncommitted after commit: %v, %v", dirty, err)
	}
}

// A local review reads the working tree as it was when the diff was
// taken: a save during the review changes neither the content the prompt
// and policy read nor the commit the reviewer's workspace checks out.
func TestInspectLocalFreezesWorkingTree(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	gitTest(t, root, "init", "--bare", remote)
	repo := filepath.Join(root, "repo")
	gitTest(t, root, "init", "-b", "main", repo)
	gitTest(t, repo, "config", "user.name", "Test")
	gitTest(t, repo, "config", "user.email", "test@example.com")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("x.go", "package x\n\nconst X = 1\n")
	write(".gitignore", "*.log\n")
	gitTest(t, repo, "add", "-A")
	gitTest(t, repo, "commit", "-m", "initial")
	gitTest(t, repo, "remote", "add", "origin", remote)
	gitTest(t, repo, "push", "-u", "origin", "main")
	gitTest(t, repo, "switch", "-c", "feature")
	write("x.go", "package x\n\nconst X = 2\n")
	write("new.go", "package x\n")
	write("debug.log", "ignored\n")
	head := gitTest(t, repo, "rev-parse", "HEAD")
	index := gitTest(t, repo, "ls-files", "--stage")

	snap, err := inspectLocal(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	src := snap.src
	resolved, _ := filepath.EvalSymlinks(repo)
	if src.Snapshot == "" || src.Snapshot == head || src.Head != head || src.HeadDir != "" || snap.info.LocalPath != resolved {
		t.Fatalf("source: snapshot %q head %q headDir %q path %q", src.Snapshot, src.Head, src.HeadDir, snap.info.LocalPath)
	}
	if !strings.Contains(snap.raw, "+const X = 2") || !strings.Contains(snap.raw, "new.go") || strings.Contains(snap.raw, "debug.log") {
		t.Fatalf("diff: %s", snap.raw)
	}
	key := localCacheKey(snap, options{})

	write("x.go", "package x\n\nconst X = 3\n")
	write("new.go", "package y\n")

	if b, err := src.Content("x.go"); err != nil || !strings.Contains(string(b), "X = 2") {
		t.Fatalf("content after save: %q, %v", b, err)
	}
	if b, err := src.Content("new.go"); err != nil || string(b) != "package x\n" {
		t.Fatalf("untracked content after save: %q, %v", b, err)
	}
	if _, err := src.Content("debug.log"); err == nil {
		t.Fatal("ignored file in the snapshot")
	}
	// The reviewer's workspace is a worktree of the snapshot.
	if got := gitTest(t, repo, "show", src.Snapshot+":x.go"); !strings.Contains(got, "X = 2") {
		t.Fatalf("snapshot x.go: %q", got)
	}
	if got := gitTest(t, repo, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD moved to %s", got)
	}
	if got := gitTest(t, repo, "ls-files", "--stage"); got != index {
		t.Fatal("inspection changed the repository index")
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "x.go")); !strings.Contains(string(b), "X = 3") {
		t.Fatalf("working tree: %q", b)
	}

	// Inspecting again sees the save, under another key; the same tree
	// makes the same snapshot.
	again, err := inspectLocal(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(again.raw, "+const X = 3") || localCacheKey(again, options{}) == key {
		t.Fatalf("after save: %s", again.raw)
	}
	same, err := inspectLocal(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if same.src.Snapshot != again.src.Snapshot {
		t.Fatalf("snapshot not stable: %s, %s", same.src.Snapshot, again.src.Snapshot)
	}

	// With nothing uncommitted the snapshot is the head commit itself.
	gitTest(t, repo, "add", "-A")
	gitTest(t, repo, "commit", "-m", "the rest")
	clean, err := inspectLocal(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if clean.src.Snapshot != clean.src.Head {
		t.Fatalf("clean snapshot %s, head %s", clean.src.Snapshot, clean.src.Head)
	}
}

func TestLocalRepoRef(t *testing.T) {
	for _, tc := range []struct{ remote, host, owner, repo string }{
		{"https://github.com/acme/widget.git", "", "acme", "widget"},
		{"git@github.com:acme/widget.git", "", "acme", "widget"},
		{"ssh://git@ghe.example.com/acme/widget.git", "ghe.example.com", "acme", "widget"},
		{"https://gitlab.com/g/sub/r.git", "gitlab.com", "g/sub", "r"},
		{"git@gitlab.com:g/sub/r.git", "gitlab.com", "g/sub", "r"},
		{"https://bitbucket.corp.com/scm/proj/repo.git", "bitbucket.corp.com", "proj", "repo"},
		{"https://dev.azure.com/org/proj/_git/repo", "dev.azure.com", "org/proj", "repo"},
		{"git@ssh.dev.azure.com:v3/org/proj/repo", "dev.azure.com", "org/proj", "repo"},
	} {
		dir := t.TempDir()
		gitTest(t, dir, "init")
		gitTest(t, dir, "remote", "add", "origin", tc.remote)
		got := localRepoRef(dir)
		if got.Host != tc.host || got.Owner != tc.owner || got.Repo != tc.repo {
			t.Errorf("%s: %+v", tc.remote, got)
		}
	}
}

func TestInspectLocalRev(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	gitTest(t, root, "init", "--bare", remote)
	repo := filepath.Join(root, "repo")
	gitTest(t, root, "init", "-b", "main", repo)
	gitTest(t, repo, "config", "user.name", "Test")
	gitTest(t, repo, "config", "user.email", "test@example.com")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("file.txt", "one\n")
	gitTest(t, repo, "add", "-A")
	gitTest(t, repo, "commit", "-m", "initial")
	rootCommit := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "remote", "add", "origin", remote)
	gitTest(t, repo, "push", "-u", "origin", "main")
	gitTest(t, repo, "switch", "-c", "feature")
	write("file.txt", "one\ntwo\n")
	gitTest(t, repo, "commit", "-am", "add two", "-m", "because")
	first := gitTest(t, repo, "rev-parse", "HEAD")
	write("file.txt", "one\ntwo\nthree\n")
	gitTest(t, repo, "commit", "-am", "add three")
	gitTest(t, repo, "switch", "main")
	write("file.txt", "dirty\n") // the working tree is not read

	c, err := inspectLocal(context.Background(), repo+"#"+first[:7])
	if err != nil {
		t.Fatal(err)
	}
	if !c.info.SingleCommit || c.info.HeadOid != first || c.info.Title != "add two" || c.info.Body != "because" || c.info.Uncommitted {
		t.Fatalf("commit: %+v", c.info)
	}
	if !strings.Contains(c.raw, "+two") || strings.Contains(c.raw, "three") || strings.Contains(c.raw, "dirty") {
		t.Fatalf("commit diff: %s", c.raw)
	}
	if b, err := c.src.Content("file.txt"); err != nil || string(b) != "one\ntwo\n" {
		t.Fatalf("commit content: %q, %v", b, err)
	}

	b, err := inspectLocal(context.Background(), repo+"#feature")
	if err != nil {
		t.Fatal(err)
	}
	if b.info.SingleCommit || b.info.HeadRef != "feature" || b.info.BaseRef != "main" || b.info.Ahead != 2 || b.info.BaseOid != rootCommit {
		t.Fatalf("branch: %+v", b.info)
	}
	if !strings.Contains(b.raw, "+two") || !strings.Contains(b.raw, "+three") || strings.Contains(b.raw, "dirty") {
		t.Fatalf("branch diff: %s", b.raw)
	}

	r, err := inspectLocal(context.Background(), repo+"#"+rootCommit)
	if err != nil {
		t.Fatal(err)
	}
	if r.info.BaseRef != "empty tree" || !strings.Contains(r.raw, "+one") {
		t.Fatalf("root commit: %+v %s", r.info, r.raw)
	}

	if _, err := inspectLocal(context.Background(), repo+"#nope"); err == nil {
		t.Fatal("unknown rev: no error")
	}
	if localCacheKey(c, options{}) == localCacheKey(b, options{}) {
		t.Fatal("commit and branch share a cache key")
	}
	revs, err := listRevs(repo + "#" + first)
	if err != nil {
		t.Fatal(err)
	}
	if revs.Current != "main" || revs.BaseRef != "main" || len(revs.Commits) != 3 {
		t.Fatalf("revs: %+v", revs)
	}
	if c := revs.Commits[1]; c.Oid != first || c.Files != 1 || c.Adds != 1 || c.Dels != 0 || c.Title != "add two" {
		t.Fatalf("commit entry: %+v", c)
	}
	ahead := map[string]int{}
	for _, b := range revs.Branches {
		ahead[b.Name] = b.Ahead
	}
	if ahead["feature"] != 2 || ahead["main"] != 0 || ahead["origin/main"] != 0 {
		t.Fatalf("branches: %+v", revs.Branches)
	}

	// Fixing asks how, and checking out needs a clean tree.
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	u := &triage.Unit{ID: "file.txt", File: "file.txt", Issues: []triage.Issue{{Severity: "medium", Title: "x"}}}
	res := &PRResult{Key: "local__rev", PR: c.info, Files: []resultFile{{FileDiff: c.src.Files[0], Units: []resultUnit{{Unit: u}}}}}
	raw, _ := json.Marshal(res)
	if err := os.WriteFile(filepath.Join(tr.results, res.Key+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	req := fixRequest{Key: res.Key, All: true, MaxRounds: 1}
	if _, err := tr.startFix(req); !errors.Is(err, errRev) {
		t.Fatalf("fix without a choice: %v", err)
	}
	req.Rev = "checkout"
	if _, err := tr.startFix(req); err == nil || !strings.Contains(err.Error(), "stash") {
		t.Fatalf("checkout with a dirty tree: %v", err)
	}
	gitTest(t, repo, "checkout", "--", "file.txt")

	if name, err := checkoutRev(c.info); err != nil || name != "pr-manager/"+first[:10] || gitTest(t, repo, "rev-parse", "HEAD") != first {
		t.Fatalf("checkout commit: %q, %v", name, err)
	}
	if name, err := checkoutRev(c.info); err != nil || name != "pr-manager/"+first[:10] {
		t.Fatalf("checkout commit again: %q, %v", name, err)
	}
	if name, err := checkoutRev(b.info); err != nil || name != "feature" || gitTest(t, repo, "symbolic-ref", "--short", "HEAD") != "feature" {
		t.Fatalf("checkout branch: %q, %v", name, err)
	}
	gitTest(t, repo, "push", "origin", "feature:remote-only")
	gitTest(t, repo, "fetch", "origin")
	o, err := inspectLocal(context.Background(), repo+"#origin/remote-only")
	if err != nil {
		t.Fatal(err)
	}
	if o.info.HeadRef != "origin/remote-only" {
		t.Fatalf("origin branch: %+v", o.info)
	}
	if name, err := checkoutRev(o.info); err != nil || name != "remote-only" || gitTest(t, repo, "rev-parse", "--abbrev-ref", "remote-only@{upstream}") != "origin/remote-only" {
		t.Fatalf("checkout origin branch: %q, %v", name, err)
	}

	// Other remotes, tags, relative revs, full ref names, and a tag named
	// like a branch.
	gitTest(t, repo, "remote", "add", "upstream", remote)
	gitTest(t, repo, "fetch", "upstream")
	up, err := inspectLocal(context.Background(), repo+"#upstream/remote-only")
	if err != nil || up.info.SingleCommit || up.info.HeadRef != "upstream/remote-only" {
		t.Fatalf("upstream branch: %+v, %v", up, err)
	}
	gitTest(t, repo, "switch", "main")
	gitTest(t, repo, "branch", "-D", "remote-only")
	if name, err := checkoutRev(up.info); err != nil || name != "remote-only" || gitTest(t, repo, "rev-parse", "--abbrev-ref", "remote-only@{upstream}") != "upstream/remote-only" {
		t.Fatalf("checkout upstream branch: %q, %v", name, err)
	}
	gitTest(t, repo, "tag", "-a", "v1", "-m", "v1", first)
	if tg, err := inspectLocal(context.Background(), repo+"#v1"); err != nil || !tg.info.SingleCommit || tg.info.HeadOid != first {
		t.Fatalf("annotated tag: %+v, %v", tg, err)
	}
	if rel, err := inspectLocal(context.Background(), repo+"#feature~1"); err != nil || !rel.info.SingleCommit || rel.info.HeadOid != first {
		t.Fatalf("feature~1: %+v, %v", rel, err)
	}
	if full, err := inspectLocal(context.Background(), repo+"#refs/heads/feature"); err != nil || full.info.SingleCommit || full.info.HeadRef != "feature" {
		t.Fatalf("refs/heads/feature: %+v, %v", full, err)
	}
	gitTest(t, repo, "tag", "feature", rootCommit)
	if br, err := inspectLocal(context.Background(), repo+"#feature"); err != nil || br.info.SingleCommit || br.info.HeadOid == rootCommit {
		t.Fatalf("branch shadowed by a tag: %+v, %v", br, err)
	}
	blob := gitTest(t, repo, "rev-parse", "HEAD:file.txt")
	if _, err := inspectLocal(context.Background(), repo+"#"+blob); err == nil {
		t.Fatal("a blob reviewed as a commit")
	}
}

func TestSourceConfigTrust(t *testing.T) {
	files := func(m map[string]string) triage.ContentFunc {
		return func(path string) ([]byte, error) {
			if s, ok := m[path]; ok {
				return []byte(s), nil
			}
			return nil, os.ErrNotExist
		}
	}
	src := &triage.Source{
		BaseContent: files(map[string]string{".triage.yaml": "thresholds: {none: 0.9}\n"}),
		Content: files(map[string]string{
			".triage.yaml":   "thresholds: {none: 0.5}\n",
			".gitattributes": "gen/** linguist-generated\n",
		}),
	}
	p, attrs, err := sourceConfig(src, true)
	if err != nil || p.Thresholds.None != 0.5 || len(attrs) != 1 {
		t.Fatalf("trusted head: none=%v attrs=%v err=%v", p.Thresholds.None, attrs, err)
	}
	p, attrs, err = sourceConfig(src, false)
	if err != nil || p.Thresholds.None != 0.9 || len(attrs) != 0 {
		t.Fatalf("untrusted head: none=%v attrs=%v err=%v", p.Thresholds.None, attrs, err)
	}
	src.Content = files(map[string]string{".triage.yaml": "thresholds: [\n"})
	if _, _, err := sourceConfig(src, true); err == nil {
		t.Fatal("a broken trusted .triage.yaml is not reported")
	}
	if _, _, err := sourceConfig(src, false); err != nil {
		t.Fatalf("a broken untrusted head .triage.yaml fails the triage: %v", err)
	}
}

func TestShutdownCancelsJobs(t *testing.T) {
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	j, ctx, _ := tr.newJob("fix", "x")
	stopped := make(chan struct{})
	go func() {
		<-ctx.Done()
		// Signal completion before finish releases shutdown's wait counter.
		close(stopped)
		j.finish(ctx.Err())
	}()
	tr.shutdown(5 * time.Second)
	select {
	case <-stopped:
	default:
		t.Fatal("shutdown returned before the job stopped")
	}
	if _, ctx, _ := tr.newJob("fix", "y"); ctx.Err() == nil {
		t.Fatal("a job started after shutdown is not cancelled")
	}
	tr.shutdown(time.Second) // idempotent
}

func TestInspectRevShallowBoundary(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	gitTest(t, root, "init", "-b", "main", src)
	gitTest(t, src, "config", "user.name", "Test")
	gitTest(t, src, "config", "user.email", "test@example.com")
	for i, body := range []string{"one\n", "one\ntwo\n"} {
		if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gitTest(t, src, "add", "-A")
		gitTest(t, src, "commit", "-m", "c"+string(rune('0'+i)))
	}
	clone := filepath.Join(root, "clone")
	gitTest(t, root, "clone", "--depth", "1", "file://"+filepath.ToSlash(src), clone)
	head := gitTest(t, clone, "rev-parse", "HEAD")

	_, err := inspectLocal(context.Background(), clone+"#"+head)
	if err == nil || !strings.Contains(err.Error(), "shallow") || !strings.Contains(err.Error(), "--deepen") {
		t.Fatalf("shallow boundary: %v", err)
	}
	revs, err := listRevs(clone)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs.Commits) != 0 {
		t.Fatalf("boundary commit listed: %+v", revs.Commits)
	}

	gitTest(t, clone, "fetch", "--deepen=1")
	c, err := inspectLocal(context.Background(), clone+"#"+head)
	if err != nil {
		t.Fatal(err)
	}
	if c.info.BaseRef == "empty tree" || !strings.Contains(c.raw, "+two") || strings.Contains(c.raw, "+one") {
		t.Fatalf("deepened: %+v %s", c.info, c.raw)
	}
}
