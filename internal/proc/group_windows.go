package proc

import "os/exec"

// KillGroup leaves Windows' default: the context's cancel kills cmd only.
func KillGroup(cmd *exec.Cmd) {}
