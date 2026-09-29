//go:build !windows

package llm

import (
	"os/exec"
	"syscall"
	"time"
)

// ownGroup starts cmd in its own session, so its own process group with no
// terminal to stop on, and makes its context kill
// the whole group, so the tools a CLI spawns (rg, shells) die with it and
// cannot hold its output pipes open. Past WaitDelay, Wait stops waiting for
// the pipes regardless.
func ownGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
}
