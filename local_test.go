package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if _, err := publishLocal(&PRResult{PR: snap.info}); err == nil || !strings.Contains(err.Error(), "commit") {
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

func TestLocalRepoRef(t *testing.T) {
	for _, tc := range []struct{ remote, host, owner, repo string }{
		{"https://github.com/acme/widget.git", "", "acme", "widget"},
		{"git@github.com:acme/widget.git", "", "acme", "widget"},
		{"ssh://git@ghe.example.com/acme/widget.git", "ghe.example.com", "acme", "widget"},
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
