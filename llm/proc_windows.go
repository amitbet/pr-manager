package llm

import (
	"os/exec"
	"strconv"
	"time"

	"github.com/amitbet/pr-manager/internal/proc"
)

// ownGroup makes cmd's context kill its whole process tree, so the tools a
// CLI spawns (rg, shells) die with it and cannot hold its output pipes open.
// Past WaitDelay, Wait stops waiting for the pipes regardless.
func ownGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if proc.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run() == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 5 * time.Second
}
