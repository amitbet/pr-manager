package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// repoPlaces are the code outside the PR's repository the chat agent may
// read: every repository the code map indexes (the workspace), the ones
// that depend on the changed code by name, the reader's own checkout of
// this repository, with its installed dependencies, the reader's code
// directory, and the dependency caches whose sources are on disk.
func (t *triager) repoPlaces(r *PRResult) []chatPlace {
	var out []chatPlace
	add := func(dir, what string) {
		if dir == "" {
			return
		}
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			out = append(out, chatPlace{dir, what})
		}
	}
	// A repo in the workspace is a symlink to the reader's checkout or a
	// clone: the place is where it really is, which Claude Code's file
	// tools need.
	real := func(p string) string {
		if d, err := filepath.EvalSymlinks(p); err == nil {
			return d
		}
		return p
	}
	o := t.opts
	if wsDir, err := workspaceDir(o); err == nil {
		code := filepath.Join(wsDir, "code")
		add(code, "every repository the code map indexes, one directory each, named as in the code map (a symlink to the reader's own checkout, or the app's clone)")
		m := loadCodeMap(o.codemapDir)
		if m != nil && r.PR.Repo != "" && r.PR.LocalPath == "" {
			own := filepath.Join(code, codeMapRepo(m, r.PR.PRRef))
			if fi, err := os.Lstat(own); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				add(real(own), "the reader's own checkout of this repository: on whatever branch they have out, but with its installed dependencies (node_modules, a virtualenv, vendor) and build output")
			}
		}
		deps := map[string]bool{}
		for _, u := range resultUnits(r) {
			if u.Impact != nil {
				for _, d := range u.Impact.DepRepos {
					deps[d] = true
				}
			}
		}
		names := sortedKeys(deps)
		if len(names) > 20 {
			names = names[:20]
		}
		for _, d := range names {
			add(real(filepath.Join(code, d)), fmt.Sprintf("repository %s, which reaches the changed code (code map)", d))
		}
	}
	if o.codeRoot != "" {
		add(o.codeRoot, "the reader's code directory: their own git checkouts")
	}
	home, _ := os.UserHomeDir()
	gomod := os.Getenv("GOMODCACHE")
	if gomod == "" {
		gopath := os.Getenv("GOPATH")
		if gopath == "" && home != "" {
			gopath = filepath.Join(home, "go")
		}
		if gopath != "" {
			gomod = filepath.Join(strings.Split(gopath, string(os.PathListSeparator))[0], "pkg", "mod")
		}
	}
	add(gomod, "the Go module cache: the source of every Go dependency downloaded on this machine, as <module>@<version>")
	if home != "" {
		add(filepath.Join(home, ".cargo", "registry", "src"), "the Cargo registry: the source of downloaded Rust crates")
	}
	return dedupePlaces(out)
}

// dedupePlaces drops a place whose directory is already listed.
func dedupePlaces(ps []chatPlace) []chatPlace {
	seen := map[string]bool{}
	var out []chatPlace
	for _, p := range ps {
		if !seen[p.dir] {
			seen[p.dir] = true
			out = append(out, p)
		}
	}
	return out
}
