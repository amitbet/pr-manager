package triage

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/amitbet/pr-manager/internal/activity"
	"github.com/amitbet/pr-manager/internal/proc"
)

type FileStatus string

const (
	StatusModified FileStatus = "modified"
	StatusAdded    FileStatus = "added"
	StatusDeleted  FileStatus = "deleted"
	StatusRenamed  FileStatus = "renamed"
)

type Hunk struct {
	Header   string   `json:"header"` // "@@ -a,b +c,d @@ context"
	OldStart int      `json:"old_start"`
	OldLines int      `json:"old_lines"`
	NewStart int      `json:"new_start"`
	NewLines int      `json:"new_lines"`
	Lines    []string `json:"lines"` // each prefixed with ' ', '+', '-' or '\'
}

func (h Hunk) String() string {
	return h.Header + "\n" + strings.Join(h.Lines, "\n")
}

type FileDiff struct {
	Path    string     `json:"path"` // deleted files keep the old path
	OldPath string     `json:"old_path,omitempty"`
	Status  FileStatus `json:"status"`
	Binary  bool       `json:"binary,omitempty"`
	Hunks   []Hunk     `json:"-"`
}

// Git runs git in dir and returns stdout.
func Git(dir string, args ...string) (string, error) {
	return GitCtx(context.Background(), dir, args...)
}

// gitConfig pins the settings that change output we parse: a user's
// diff.noprefix, mnemonic prefixes, submodule logs, quoted non-ASCII paths
// or blank context lines would otherwise break ParseDiff and path lists.
// core.longpaths lets Windows check out deep trees. Unknown keys are
// ignored by older git.
var gitConfig = []string{
	"-c", "core.quotePath=false",
	"-c", "core.longpaths=true",
	"-c", "diff.noprefix=false",
	"-c", "diff.mnemonicPrefix=false",
	"-c", "diff.srcPrefix=a/",
	"-c", "diff.dstPrefix=b/",
	"-c", "diff.submodule=short",
	"-c", "diff.suppressBlankEmpty=false",
	"-c", "diff.relative=false",
	"-c", "color.ui=never",
	"-c", "log.showSignature=false",
}

// GitArgs returns args with the -c flags that pin git's output format
// in front, for running git directly.
func GitArgs(args ...string) []string {
	return append(append([]string(nil), gitConfig...), args...)
}

// GitCtx is Git, logged to ctx's activity log. It is killed when ctx is
// done.
func GitCtx(ctx context.Context, dir string, args ...string) (_ string, err error) {
	_, done := activity.Command(ctx, dir, "git", args...)
	defer func() { done(err) }()
	cmd := proc.CommandContext(ctx, "git", GitArgs(args...)...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, ee.Stderr)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// GitDiff returns the diff between the merge base of base and head.
// ignoreWS drops whitespace and blank-line changes, which is how
// formatting-only files are detected.
func GitDiff(dir, base, head string, ignoreWS bool) (string, error) {
	args := []string{"diff", "--no-color", "--no-ext-diff", "--src-prefix=a/", "--dst-prefix=b/", "-M", "-U5"}
	if ignoreWS {
		args = append(args, "-w", "--ignore-blank-lines")
	}
	args = append(args, base+"..."+head)
	return Git(dir, args...)
}

// ParseDiff parses `git diff` unified output.
func ParseDiff(s string) ([]FileDiff, error) {
	var files []FileDiff
	var cur *FileDiff
	var hunk *Hunk
	flushHunk := func() {
		if cur != nil && hunk != nil {
			cur.Hunks = append(cur.Hunks, *hunk)
		}
		hunk = nil
	}
	flushFile := func() {
		flushHunk()
		if cur != nil {
			files = append(files, *cur)
		}
		cur = nil
	}
	// Lines are split by hand: a scanner's line limit would fail the
	// whole diff on one minified or generated line.
	for rest := s; rest != ""; {
		var line string
		line, rest, _ = strings.Cut(rest, "\n")
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flushFile()
			a, b := splitDiffGitPaths(strings.TrimPrefix(line, "diff --git "))
			cur = &FileDiff{Path: b, OldPath: a, Status: StatusModified}
		case cur == nil:
			continue
		case hunk == nil && strings.HasPrefix(line, "new file mode"):
			cur.Status = StatusAdded
		case hunk == nil && strings.HasPrefix(line, "deleted file mode"):
			cur.Status = StatusDeleted
			cur.Path = cur.OldPath
		case hunk == nil && strings.HasPrefix(line, "rename from "):
			cur.OldPath = unquotePath(strings.TrimPrefix(line, "rename from "))
			cur.Status = StatusRenamed
		case hunk == nil && strings.HasPrefix(line, "rename to "):
			cur.Path = unquotePath(strings.TrimPrefix(line, "rename to "))
		case hunk == nil && strings.HasPrefix(line, "Binary files "):
			cur.Binary = true
		case hunk == nil && (strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ")):
			// paths already known from the diff --git / rename lines
		case strings.HasPrefix(line, "@@ "):
			flushHunk()
			h, err := parseHunkHeader(line)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", cur.Path, err)
			}
			hunk = &h
		case hunk != nil:
			hunk.Lines = append(hunk.Lines, line)
		}
	}
	flushFile()
	return files, nil
}

// splitDiffGitPaths splits "a/x b/y", either side of which git may
// C-quote ("a/t\tab"). Unquoted paths with spaces are ambiguous in this
// line; rename lines override it when present.
func splitDiffGitPaths(s string) (string, string) {
	if strings.HasPrefix(s, `"`) {
		if a, rest, ok := cutQuoted(s); ok {
			b := strings.TrimPrefix(rest, " ")
			if strings.HasPrefix(b, `"`) {
				if q, _, ok := cutQuoted(b); ok {
					b = q
				}
			}
			return strings.TrimPrefix(a, "a/"), strings.TrimPrefix(b, "b/")
		}
	}
	if i := strings.Index(s, ` "b/`); i >= 0 {
		if b, _, ok := cutQuoted(s[i+1:]); ok {
			return strings.TrimPrefix(s[:i], "a/"), strings.TrimPrefix(b, "b/")
		}
	}
	if i := strings.Index(s, " b/"); i >= 0 {
		return strings.TrimPrefix(s[:i], "a/"), s[i+3:]
	}
	return s, s
}

// cutQuoted unquotes the C-quoted string s starts with and returns it and
// what follows it. Git's escapes (\t, \", \\, \303 octal bytes) are a
// subset of Go's.
func cutQuoted(s string) (string, string, bool) {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			u, err := strconv.Unquote(s[:i+1])
			if err != nil {
				return "", s, false
			}
			return u, s[i+1:], true
		}
	}
	return "", s, false
}

// unquotePath decodes a path git C-quoted, and returns others as is.
func unquotePath(s string) string {
	if strings.HasPrefix(s, `"`) {
		if u, rest, ok := cutQuoted(s); ok && rest == "" {
			return u
		}
	}
	return s
}

func parseHunkHeader(line string) (Hunk, error) {
	h := Hunk{Header: line}
	end := strings.Index(line[3:], " @@")
	if end < 0 {
		return h, fmt.Errorf("bad hunk header %q", line)
	}
	for _, f := range strings.Fields(line[3 : 3+end]) {
		start, count, err := parseRange(f[1:])
		if err != nil {
			return h, fmt.Errorf("bad hunk header %q: %w", line, err)
		}
		// Git gives an empty range as the line before it ("+12,0" sits
		// between 12 and 13). Normalize to the next line so every start
		// means "first line at or after this hunk", like split hunks use.
		if count == 0 {
			start++
		}
		if f[0] == '-' {
			h.OldStart, h.OldLines = start, count
		} else {
			h.NewStart, h.NewLines = start, count
		}
	}
	return h, nil
}

func parseRange(s string) (int, int, error) {
	startS, countS, hasCount := strings.Cut(s, ",")
	start, err := strconv.Atoi(startS)
	if err != nil {
		return 0, 0, err
	}
	if !hasCount {
		return start, 1, nil
	}
	count, err := strconv.Atoi(countS)
	return start, count, err
}
