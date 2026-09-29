//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// ownGroup starts cmd in its own session, so its own process group with no
// terminal (an interactive shell in a group of its own under a terminal
// stops itself trying to take it over), and makes its context kill the
// whole group, so jobs an rc file backgrounds die with the shell. The
// returned func kills whatever is left in the group once cmd has exited.
func ownGroup(cmd *exec.Cmd) (reap func()) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return func() {
		if cmd.Process != nil {
			// The group outlives the shell while any member runs, so its id
			// can't have been reused.
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
}
