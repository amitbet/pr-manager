package proc

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW: a console program gets a console it
// can use but no window for it.
const createNoWindow = 0x08000000

// Hide keeps cmd from opening a console window. It adds to cmd.SysProcAttr
// rather than replacing it, so call it before or after other setup.
func Hide(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
