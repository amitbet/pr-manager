// Package proc builds subprocess commands that don't flash a console window.
//
// The desktop build is a Windows GUI app (-H windowsgui) with no console of
// its own, so every console program it starts (git, gh, the model CLIs)
// would otherwise get a fresh console window: one per call, stealing focus,
// and closing it kills the call.
package proc

import (
	"context"
	"os/exec"
)

// Command is exec.Command with Hide applied.
func Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	Hide(cmd)
	return cmd
}

// CommandContext is exec.CommandContext with Hide applied.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	Hide(cmd)
	return cmd
}
