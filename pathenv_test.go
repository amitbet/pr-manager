package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestMergePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses : separated lists")
	}
	got := mergePath("/usr/bin:/bin", "/opt/homebrew/bin::/usr/bin", "", "/usr/local/bin:/bin")
	want := "/usr/bin:/bin:/opt/homebrew/bin:/usr/local/bin"
	if got != want {
		t.Fatalf("mergePath = %q, want %q", got, want)
	}
}

func TestLoginShellPathIgnoresHeldStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no login shell probe on Windows")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "sleeper.pid")
	// An rc file that prints noise and backgrounds a job holding stdout,
	// then runs the probe's -c command.
	shell := filepath.Join(dir, "fakeshell")
	script := "#!/bin/sh\necho noise\nsleep 60 &\necho $! > '" + pidFile + "'\nPATH=/shell/bin:/usr/bin\neval \"$2\"\necho more noise\n"
	if err := os.WriteFile(shell, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)
	start := time.Now()
	got := loginShellPath()
	if got != "/shell/bin:/usr/bin" {
		t.Fatalf("loginShellPath = %q", got)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("loginShellPath took %v", d)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscan(string(raw), &pid); err != nil {
		t.Fatal(err)
	}
	// The sleeper was killed with the shell's group; wait for it to go.
	for deadline := time.Now().Add(2 * time.Second); alive(pid); {
		if time.Now().After(deadline) {
			t.Fatalf("background job %d outlived the probe", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p.Signal(syscall.Signal(0)) == nil
}
