package triage

import (
	"os"
	"path/filepath"
	"strings"
)

// Source is a parsed diff plus what the presort and unit grouping need
// from the head revision.
type Source struct {
	Files []FileDiff
	// Content returns a file at head; nil groups Go hunks by git's hunk
	// context and skips the generated-header check.
	Content ContentFunc
	// BaseContent returns a file at the merge base (the old side of the
	// diff); nil means hunks can only be placed by line numbers.
	BaseContent ContentFunc
	// RealChanges is the set of files that still differ under
	// `git diff -w --ignore-blank-lines`; nil skips the formatting check.
	RealChanges map[string]bool
	// Dir, Base and Head locate the change in a git checkout (Base is the
	// merge base); empty for saved diffs. Title is the PR title, if any.
	// They feed author experience and the "fix PR" signal.
	Dir, Base, Head string
	Title           string
	// HeadDir, if set, is a directory holding the head revision (a saved
	// fixture), used as the reviewer's workspace instead of a git worktree.
	HeadDir string
	// Snapshot, if set, is a commit in Dir holding the files Content
	// serves: a local review's working tree, frozen when its diff was
	// taken. The reviewer's workspace is a detached worktree of it in
	// place of Head, with its agent files kept, as the checkout is the
	// user's own; Head stays the branch's commit for its history.
	Snapshot string
}

// FromGit diffs base...head in dir.
func FromGit(dir, base, head string) (*Source, error) {
	raw, err := GitDiff(dir, base, head, false)
	if err != nil {
		return nil, err
	}
	files, err := ParseDiff(raw)
	if err != nil {
		return nil, err
	}
	rawWS, err := GitDiff(dir, base, head, true)
	if err != nil {
		return nil, err
	}
	filesWS, err := ParseDiff(rawWS)
	if err != nil {
		return nil, err
	}
	// The diff is base...head, so its old side is the merge base.
	mb, err := Git(dir, "merge-base", base, head)
	if err != nil {
		return nil, err
	}
	mergeBase := strings.TrimSpace(mb)
	real := map[string]bool{}
	for _, f := range filesWS {
		if len(f.Hunks) > 0 || f.Binary {
			real[f.Path] = true
		}
	}
	return &Source{
		Files: files,
		Content: func(path string) ([]byte, error) {
			s, err := Git(dir, "show", head+":"+path)
			return []byte(s), err
		},
		BaseContent: func(path string) ([]byte, error) {
			s, err := Git(dir, "show", mergeBase+":"+path)
			return []byte(s), err
		},
		RealChanges: real,
		Dir:         dir, Base: mergeBase, Head: head,
	}, nil
}

// FromDiff parses a saved diff. headDir and baseDir, if set, hold the head
// and merge-base versions of the changed files at their repo paths.
func FromDiff(raw, headDir, baseDir string) (*Source, error) {
	files, err := ParseDiff(raw)
	if err != nil {
		return nil, err
	}
	src := &Source{Files: files, HeadDir: headDir}
	src.Content = dirContent(headDir)
	src.BaseContent = dirContent(baseDir)
	return src, nil
}

func dirContent(dir string) ContentFunc {
	if dir == "" {
		return nil
	}
	return func(path string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
	}
}
