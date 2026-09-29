//go:build !windows

package proc

import "os/exec"

// Hide is a no-op: only Windows opens a window for a console subprocess.
func Hide(cmd *exec.Cmd) {}
