package proc

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestHideSetsNoWindow(t *testing.T) {
	cmd := Command("cmd", "/c", "exit")
	if a := cmd.SysProcAttr; a == nil || !a.HideWindow || a.CreationFlags&createNoWindow == 0 {
		t.Fatalf("SysProcAttr = %+v, want HideWindow and CREATE_NO_WINDOW", a)
	}
}

func TestHideKeepsExistingAttrs(t *testing.T) {
	const newGroup = 0x00000200 // CREATE_NEW_PROCESS_GROUP
	cmd := exec.Command("cmd", "/c", "exit")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: newGroup}
	Hide(cmd)
	if f := cmd.SysProcAttr.CreationFlags; f&newGroup == 0 || f&createNoWindow == 0 {
		t.Fatalf("CreationFlags = %#x, want both flags", f)
	}
}
