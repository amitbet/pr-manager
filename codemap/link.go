//go:build !windows

package codemap

import "path/filepath"

// ResolveLink is filepath.EvalSymlinks, which on Windows also follows a
// directory junction: the workspace links checkouts in with one when a
// symlink needs a privilege the user does not have.
func ResolveLink(p string) (string, error) {
	return filepath.EvalSymlinks(p)
}
