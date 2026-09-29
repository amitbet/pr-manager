//go:build !windows

package main

import "os"

// linkDir links link to the directory target.
func linkDir(target, link string) error {
	return os.Symlink(target, link)
}
