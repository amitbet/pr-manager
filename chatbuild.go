package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/internal/activity"
	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/llm"
)

// The chat agent on codex and claude-code may build and test Go code in
// its sandbox (see llm/build.go): its build output goes to a directory of
// the conversation's under the temp directory, and the modules the code
// needs are downloaded first, outside the sandbox, which has no network.
// go mod download runs nothing of the PR's. Windows has no sandbox for
// Claude Code, so there it doesn't build.

var modsFetched sync.Map // dir + go.sum's mtime -> struct{}

// builds lets ws build, for a Go module, on codex and claude-code.
func (cv *chatConv) builds(ctx context.Context, ws *llm.Workspace) {
	if runtime.GOOS == "windows" || !goModule(ws.Dir) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fetchModules(ctx, ws.Dir)
	dir := filepath.Join(os.TempDir(), "pr-manager-agent-go", orDefault(cv.hash, "default"))
	for _, d := range []string{"cache", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return
		}
	}
	ws.Builds, ws.Scratch = dir, cv.scratch
}

// goModule reports whether dir is a Go module, at its top.
func goModule(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}

// fetchModules downloads dir's modules into the module cache, once per
// go.sum, for at most a minute.
func fetchModules(ctx context.Context, dir string) {
	sum, err := os.Stat(filepath.Join(dir, "go.sum"))
	if err != nil {
		return
	}
	if _, done := modsFetched.LoadOrStore(dir+"\x00"+sum.ModTime().String(), struct{}{}); done {
		return
	}
	if _, err := exec.LookPath("go"); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	ctx, done := activity.Command(ctx, dir, "go", "mod", "download")
	cmd := proc.CommandContext(ctx, "go", "mod", "download")
	cmd.Dir = dir
	done(cmd.Run())
}
