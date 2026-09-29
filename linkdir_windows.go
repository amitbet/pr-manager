package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"
)

// linkDir links link to the directory target: a symlink, else a directory
// junction. Creating a symlink takes Developer Mode or an elevated
// process ("A required privilege is not held by the client"); a junction
// takes neither, but only points at a local absolute path.
func linkDir(target, link string) error {
	serr := os.Symlink(target, link)
	if serr == nil {
		return nil
	}
	jerr := createJunction(target, link)
	if jerr == nil {
		return nil
	}
	return fmt.Errorf("%v; junction: %v", serr, jerr)
}

const (
	fsctlSetReparsePoint   = 0x000900A4
	ioReparseTagMountPoint = 0xA0000003
)

// createJunction makes link an empty directory and turns it into a mount
// point (junction) for target, what mklink /J does.
func createJunction(target, link string) error {
	target, err := filepath.Abs(strings.TrimPrefix(target, `\\?\`))
	if err != nil {
		return err
	}
	if strings.HasPrefix(target, `\\`) {
		return errors.New("a junction cannot point at a network path")
	}
	if err := os.Mkdir(link, 0o755); err != nil {
		return err
	}
	if err := setMountPoint(link, target); err != nil {
		_ = os.Remove(link)
		return err
	}
	return nil
}

// setMountPoint writes a mount point reparse buffer onto the empty
// directory dir: the target as an NT path (\??\C:\...) to follow, and as
// is to show.
func setMountPoint(dir, target string) error {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)

	sub := utf16.Encode([]rune(`\??\` + target))
	show := utf16.Encode([]rune(target))
	names := (len(sub) + 1 + len(show) + 1) * 2 // each NUL-terminated
	if 8+names > 0xFFFF {
		return errors.New("path too long for a junction")
	}
	// REPARSE_DATA_BUFFER: tag, data length, reserved, then the
	// MountPointReparseBuffer's name offsets and lengths and its names.
	buf := make([]byte, 16+names)
	le := binary.LittleEndian
	le.PutUint32(buf[0:], ioReparseTagMountPoint)
	le.PutUint16(buf[4:], uint16(8+names))
	le.PutUint16(buf[8:], 0)
	le.PutUint16(buf[10:], uint16(len(sub)*2))
	le.PutUint16(buf[12:], uint16((len(sub)+1)*2))
	le.PutUint16(buf[14:], uint16(len(show)*2))
	off := 16
	for _, c := range sub {
		le.PutUint16(buf[off:], c)
		off += 2
	}
	off += 2
	for _, c := range show {
		le.PutUint16(buf[off:], c)
		off += 2
	}
	var n uint32
	return syscall.DeviceIoControl(h, fsctlSetReparsePoint, &buf[0], uint32(len(buf)), nil, 0, &n, nil)
}
