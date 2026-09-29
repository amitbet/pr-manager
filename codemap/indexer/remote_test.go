package indexer

import (
	"os/exec"
	"testing"
)

func TestRepoRemote(t *testing.T) {
	dir := t.TempDir()
	if repoRemote(dir) != "" {
		t.Error("a directory that is not a checkout has a remote")
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@gitlab.com:G/sub/API.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if got := repoRemote(dir); got != "gitlab.com/g/sub/api" {
		t.Errorf("repoRemote = %q", got)
	}
}
