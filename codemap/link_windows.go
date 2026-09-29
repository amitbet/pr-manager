package codemap

import (
	"errors"
	"os"
	"path/filepath"
)

// ResolveLink is filepath.EvalSymlinks that also follows a directory
// junction. The workspace links checkouts in with one when a symlink
// needs a privilege the user does not have, and EvalSymlinks leaves
// junctions alone: Lstat reports them as irregular, not as symlinks.
// Only a junction at the end of the path is followed, which is where the
// workspace puts them.
func ResolveLink(p string) (string, error) {
	for range 8 {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			return "", err
		}
		fi, err := os.Lstat(r)
		if err != nil || fi.Mode()&os.ModeIrregular == 0 {
			return r, nil
		}
		t, err := os.Readlink(r)
		if err != nil {
			return r, nil // another kind of reparse point
		}
		if !filepath.IsAbs(t) {
			t = filepath.Join(filepath.Dir(r), t)
		}
		p = t
	}
	return "", errors.New(p + ": too many links")
}
