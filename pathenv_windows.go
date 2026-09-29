package main

import "os/exec"

// ownGroup is a no-op: the login shell PATH probe doesn't run on Windows.
func ownGroup(cmd *exec.Cmd) (reap func()) { return func() {} }
