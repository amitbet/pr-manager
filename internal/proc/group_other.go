//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
)

// KillGroup starts cmd in a process group of its own and has its context's
// cancel kill the whole group, so a test binary a build tool started dies
// with it instead of holding its output open.
func KillGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
