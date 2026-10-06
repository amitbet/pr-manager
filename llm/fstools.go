package llm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/internal/proc"
)

// The workspace as tools, for the API providers: read_file, list_dir,
// glob and grep over Dir and ReadDirs, and with Shell a git that runs only
// commands that read. Every path is resolved, symlinks and all, and must
// land in one of the directories; nothing here writes. These are what
// Claude Code gets as Read, Glob, Grep and its sandboxed shell, with the
// shell cut down to git: pipes and arbitrary commands would need the
// sandbox the CLIs have.

const (
	toolOutputMax = 60_000 // chars of one tool call's output
	readLines     = 500    // lines read_file returns by default
	readLinesMax  = 2000
	lineMax       = 2000 // chars of one line
	listMax       = 1000 // entries of list_dir and glob
	grepMax       = 200  // matches grep returns by default
	grepMaxMax    = 1000
	grepFileMax   = 4 << 20 // bytes of a file the fallback grep reads
	toolTimeout   = 60 * time.Second
)

// skipDirs are not walked by list_dir, glob and the fallback grep.
var skipDirs = map[string]bool{".git": true, "node_modules": true}

type wsTools struct {
	dir   string   // the working directory, resolved
	roots []string // every directory that may be read, dir first
	shell bool
}

func newWorkspaceTools(ws *Workspace) (*wsTools, error) {
	t := &wsTools{shell: ws.Shell}
	for i, d := range append([]string{ws.Dir}, ws.ReadDirs...) {
		r, err := resolveDir(d)
		if err != nil {
			if i == 0 {
				return nil, fmt.Errorf("workspace: %w", err)
			}
			continue
		}
		t.roots = append(t.roots, r)
	}
	t.dir = t.roots[0]
	return t, nil
}

func resolveDir(d string) (string, error) {
	abs, err := filepath.Abs(d)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// resolve is p, relative to the working directory, as a real path inside
// a root.
func (t *wsTools) resolve(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(t.dir, p)
	}
	r, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		// Whether a file outside exists is not the agent's to learn: a
		// missing path is outside if its nearest existing parent is.
		if !t.inside(nearestReal(filepath.Clean(p))) {
			return "", fmt.Errorf("%s is outside the directories you may read: %s", p, strings.Join(t.roots, ", "))
		}
		return "", fmt.Errorf("%s does not exist", p)
	}
	if !t.inside(r) {
		return "", fmt.Errorf("%s is outside the directories you may read: %s", p, strings.Join(t.roots, ", "))
	}
	return r, nil
}

// nearestReal is p with its nearest existing parent's symlinks resolved.
func nearestReal(p string) string {
	rest := ""
	for d := p; ; d = filepath.Dir(d) {
		if r, err := filepath.EvalSymlinks(d); err == nil {
			return filepath.Join(r, rest)
		}
		if filepath.Dir(d) == d {
			return p
		}
		rest = filepath.Join(filepath.Base(d), rest)
	}
}

func (t *wsTools) inside(p string) bool {
	for _, root := range t.roots {
		if rel, err := filepath.Rel(root, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// show is how p reads in output: relative to the working directory when
// it is under it, else whole.
func (t *wsTools) show(p string) string {
	if rel, err := filepath.Rel(t.dir, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return p
}

func (t *wsTools) tools() []LocalTool {
	out := []LocalTool{
		{Def: ToolDefinition{Name: "read_file", Description: "Read a text file, with line numbers. Paths are relative to the working directory, or absolute in any directory you may read. Long files come in pages: read on with offset.",
			InputSchema: schema(map[string]any{
				"path":   str("The file."),
				"offset": num("First line to read, from 1 (default 1)."),
				"limit":  num(fmt.Sprintf("Lines to read (default %d, at most %d).", readLines, readLinesMax)),
			}, "path")}, Run: t.readFile},
		{Def: ToolDefinition{Name: "list_dir", Description: "List a directory: subdirectories end in /, files have their size. .git and node_modules are listed but not entered.",
			InputSchema: schema(map[string]any{
				"path":  str("The directory (default: the working directory)."),
				"depth": num("Levels to list, 1 to 4 (default 1)."),
			})}, Run: t.listDir},
		{Def: ToolDefinition{Name: "glob", Description: "Find files by name: * and ? within a name, ** across directories, {a,b} for either. A pattern without / matches file names at any depth, e.g. *_test.go; with /, the path under path, e.g. cmd/**/main.go.",
			InputSchema: schema(map[string]any{
				"pattern": str("The pattern."),
				"path":    str("The directory to search (default: the working directory)."),
			}, "pattern")}, Run: t.glob},
		{Def: ToolDefinition{Name: "grep", Description: "Search file contents with a regular expression (RE2/Rust syntax), like rg: matches as path:line:text. Skips .gitignored, binary and very large files.",
			InputSchema: schema(map[string]any{
				"pattern":     str("The regular expression."),
				"path":        str("A directory or file to search (default: the working directory)."),
				"glob":        str("Only files whose name or path matches this, e.g. *.go or src/**/*.ts."),
				"ignore_case": map[string]any{"type": "boolean"},
				"context":     num("Lines of context around each match, 0 to 10 (default 0)."),
				"files_only":  map[string]any{"type": "boolean", "description": "List the matching files only."},
				"max_results": num(fmt.Sprintf("Most lines of output (default %d, at most %d).", grepMax, grepMaxMax)),
			}, "pattern")}, Run: t.grep},
	}
	if t.shell {
		var cmds []string
		for c := range gitReadCommands {
			cmds = append(cmds, c)
		}
		sort.Strings(cmds)
		out = append(out, LocalTool{Def: ToolDefinition{Name: "git", Description: "Run a git command that reads, in a repository you may read: " + strings.Join(cmds, ", ") + ". For history: log -L <start>,<end>:<file>, log -S <text>, blame -L, show <rev>:<file>, diff <a>..<b>. There is no shell: no pipes, so filter with git's own options (-n, --stat, -- <path>).",
			InputSchema: schema(map[string]any{
				"args": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": `The arguments after "git", one per item, e.g. ["log", "-n", "5", "--oneline", "--", "main.go"].`},
				"dir":  str("The repository (default: the working directory)."),
			}, "args")}, Run: t.git})
	}
	return out
}

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

func argStr(a map[string]any, k string) string {
	s, _ := a[k].(string)
	return s
}

// argInt is a[k] as a number between lo and hi, def when it isn't there.
func argInt(a map[string]any, k string, def, lo, hi int) int {
	n := def
	switch v := a[k].(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	case string:
		fmt.Sscan(v, &n)
	}
	return min(max(n, lo), hi)
}

func argBool(a map[string]any, k string) bool {
	switch v := a[k].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

func isBinary(head []byte) bool { return bytes.IndexByte(head[:min(len(head), 8000)], 0) >= 0 }

func (t *wsTools) readFile(_ context.Context, a map[string]any) (string, error) {
	p, err := t.resolve(argStr(a, "path"))
	if err != nil {
		return "", err
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s is a directory: use list_dir", t.show(p))
	}
	from := argInt(a, "offset", 1, 1, 1<<30)
	limit := argInt(a, "limit", readLines, 1, readLinesMax)
	r := bufio.NewReaderSize(f, 64<<10)
	if head, _ := r.Peek(8000); isBinary(head) {
		return fmt.Sprintf("%s is a binary file of %d bytes.", t.show(p), st.Size()), nil
	}
	var body strings.Builder
	n, shown, cut := 0, 0, 0
	for {
		line, err := r.ReadString('\n')
		if line == "" && err != nil {
			break
		}
		n++
		if n >= from && shown < limit && cut == 0 {
			line = strings.TrimRight(line, "\r\n")
			if len(line) > lineMax {
				line = line[:lineMax] + " [line cut]"
			}
			if body.Len()+len(line) > toolOutputMax {
				cut = n
			} else {
				fmt.Fprintf(&body, "%6d\t%s\n", n, line)
				shown++
			}
		}
		if err != nil {
			break
		}
	}
	switch {
	case n == 0:
		return t.show(p) + " is empty.", nil
	case shown == 0:
		return fmt.Sprintf("%s has %d lines; offset %d is past its end.", t.show(p), n, from), nil
	}
	end := from + shown - 1
	head := fmt.Sprintf("%s, lines %d-%d of %d:\n", t.show(p), from, end, n)
	if end < n {
		body.WriteString(fmt.Sprintf("[%d more lines: read on with offset %d]\n", n-end, end+1))
	}
	return head + body.String(), nil
}

func (t *wsTools) listDir(_ context.Context, a map[string]any) (string, error) {
	p, err := t.resolve(argStr(a, "path"))
	if err != nil {
		return "", err
	}
	depth := argInt(a, "depth", 1, 1, 4)
	var b strings.Builder
	n := 0
	var walk func(dir string, level int) bool
	walk = func(dir string, level int) bool {
		es, err := os.ReadDir(dir)
		if err != nil {
			fmt.Fprintf(&b, "%s[%v]\n", strings.Repeat("  ", level+1), err)
			return true
		}
		for _, e := range es {
			if n++; n > listMax {
				fmt.Fprintf(&b, "[more entries left out: list a subdirectory]\n")
				return false
			}
			indent := strings.Repeat("  ", level+1)
			if e.IsDir() {
				fmt.Fprintf(&b, "%s%s/\n", indent, e.Name())
				if level+1 < depth && !skipDirs[e.Name()] && !walk(filepath.Join(dir, e.Name()), level+1) {
					return false
				}
				continue
			}
			size := ""
			if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
				size = fmt.Sprintf(" (%d bytes)", info.Size())
			} else if e.Type()&fs.ModeSymlink != 0 {
				size = " (symlink)"
			}
			fmt.Fprintf(&b, "%s%s%s\n", indent, e.Name(), size)
		}
		return true
	}
	fmt.Fprintf(&b, "%s/\n", t.show(p))
	walk(p, 0)
	return b.String(), nil
}

// globRegexp is a glob as a regular expression over slash paths.
func globRegexp(g string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	depth := 0
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch {
		case c == '*' && strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case c == '*' && strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '{':
			b.WriteString("(?:")
			depth++
		case c == '}' && depth > 0:
			b.WriteString(")")
			depth--
		case c == ',' && depth > 0:
			b.WriteString("|")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// globMatcher matches a slash path relative to the search root: by its
// name when the pattern has no /.
func globMatcher(pattern string) (func(rel string) bool, error) {
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "./")
	re, err := globRegexp(pattern)
	if err != nil {
		return nil, fmt.Errorf("bad pattern %q: %w", pattern, err)
	}
	if !strings.Contains(pattern, "/") {
		return func(rel string) bool { return re.MatchString(rel[strings.LastIndex(rel, "/")+1:]) }, nil
	}
	return re.MatchString, nil
}

// walkFiles calls fn on the regular files under root, not entering
// skipDirs or following symlinks, until fn returns false.
func walkFiles(ctx context.Context, root string, fn func(path, rel string) bool) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if !fn(p, filepath.ToSlash(rel)) {
			return filepath.SkipAll
		}
		return nil
	})
}

func (t *wsTools) glob(ctx context.Context, a map[string]any) (string, error) {
	root, err := t.resolve(argStr(a, "path"))
	if err != nil {
		return "", err
	}
	match, err := globMatcher(argStr(a, "pattern"))
	if err != nil {
		return "", err
	}
	var hits []string
	more := false
	err = walkFiles(ctx, root, func(p, rel string) bool {
		if !match(rel) {
			return true
		}
		if len(hits) == listMax {
			more = true
			return false
		}
		hits = append(hits, t.show(p))
		return true
	})
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "No files match.", nil
	}
	sort.Strings(hits)
	out := strings.Join(hits, "\n") + "\n"
	if more {
		out += "[more files match: narrow the pattern or the path]\n"
	}
	return out, nil
}

func (t *wsTools) grep(ctx context.Context, a map[string]any) (string, error) {
	pattern := argStr(a, "pattern")
	if pattern == "" {
		return "", errors.New("no pattern")
	}
	root, err := t.resolve(argStr(a, "path"))
	if err != nil {
		return "", err
	}
	q := grepQuery{pattern: pattern, glob: argStr(a, "glob"), icase: argBool(a, "ignore_case"), filesOnly: argBool(a, "files_only"),
		context: argInt(a, "context", 0, 0, 10), max: argInt(a, "max_results", grepMax, 1, grepMaxMax)}
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	var lines []string
	if rg, err := exec.LookPath("rg"); err == nil {
		lines, err = t.ripgrep(ctx, rg, root, q)
		if err != nil {
			return "", err
		}
	} else if lines, err = t.goGrep(ctx, root, q); err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "No matches.", nil
	}
	return capLines(lines, q.max, "narrow the pattern, the path or the glob"), nil
}

type grepQuery struct {
	pattern, glob    string
	icase, filesOnly bool
	context, max     int
}

// capLines joins lines, at most n of them and toolOutputMax chars.
func capLines(lines []string, n int, hint string) string {
	var b strings.Builder
	for i, l := range lines {
		if i == n || b.Len()+len(l) > toolOutputMax {
			fmt.Fprintf(&b, "[%d more lines left out: %s]\n", len(lines)-i, hint)
			break
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

// ripgrep runs rg with no config file and no preprocessor, the pattern
// and the path behind -e and --.
func (t *wsTools) ripgrep(ctx context.Context, rg, root string, q grepQuery) ([]string, error) {
	args := []string{"--no-config", "--color=never", "--no-heading", "--with-filename", "-n", "--hidden", "-g", "!.git/", "--max-columns=500", "--max-columns-preview", "--max-filesize=4M"}
	if q.icase {
		args = append(args, "-i")
	}
	if q.filesOnly {
		args = append(args, "-l")
	}
	if q.context > 0 {
		args = append(args, "-C", fmt.Sprint(q.context))
	}
	if q.glob != "" {
		args = append(args, "-g", q.glob)
	}
	target := root
	if rel := t.show(root); !filepath.IsAbs(rel) {
		target = filepath.FromSlash(rel)
	}
	args = append(args, "-e", q.pattern, "--", target)
	cmd := proc.CommandContext(ctx, rg, args...)
	cmd.Dir = t.dir
	out := &capWriter{max: 4 * toolOutputMax}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit) && exit.ExitCode() == 1: // no matches
		return nil, nil
	case out.buf.Len() == 0:
		return nil, fmt.Errorf("rg: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	text := strings.TrimRight(out.buf.String(), "\n")
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = filepath.ToSlash(strings.TrimPrefix(l, "./"))
	}
	return lines, nil
}

// goGrep is grep where rg isn't installed.
func (t *wsTools) goGrep(ctx context.Context, root string, q grepQuery) ([]string, error) {
	pat := q.pattern
	if q.icase {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, fmt.Errorf("bad pattern: %w", err)
	}
	match := func(string) bool { return true }
	if q.glob != "" {
		if match, err = globMatcher(q.glob); err != nil {
			return nil, err
		}
	}
	var out []string
	limit := q.max + 1
	search := func(p, rel string) bool {
		if !match(rel) {
			return true
		}
		if st, err := os.Stat(p); err != nil || st.Size() > grepFileMax {
			return true
		}
		b, err := os.ReadFile(p)
		if err != nil || isBinary(b) {
			return true
		}
		name := t.show(p)
		lines := strings.Split(string(b), "\n")
		shownTo := -1
		for i, l := range lines {
			if !re.MatchString(l) {
				continue
			}
			if q.filesOnly {
				out = append(out, name)
				break
			}
			from := max(i-q.context, shownTo+1)
			if q.context > 0 && shownTo >= 0 && from > shownTo+1 {
				out = append(out, "--")
			}
			for k := from; k <= min(i+q.context, len(lines)-1); k++ {
				sep := "-"
				if re.MatchString(lines[k]) {
					sep = ":"
				}
				text := strings.TrimRight(lines[k], "\r")
				if len(text) > 500 {
					text = text[:500] + " [line cut]"
				}
				out = append(out, fmt.Sprintf("%s%s%d%s%s", name, sep, k+1, sep, text))
				shownTo = k
			}
			if len(out) >= limit {
				return false
			}
		}
		return len(out) < limit
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		search(root, filepath.Base(root))
		return out, nil
	}
	return out, walkFiles(ctx, root, search)
}

// gitReadCommands are the git commands the git tool runs: ones that read.
var gitReadCommands = map[string]bool{
	"log": true, "show": true, "blame": true, "diff": true, "grep": true, "status": true, "ls-files": true, "ls-tree": true,
	"rev-parse": true, "rev-list": true, "cat-file": true, "merge-base": true, "name-rev": true, "describe": true,
	"shortlog": true, "for-each-ref": true, "show-ref": true, "whatchanged": true, "range-diff": true,
}

// gitDenied are options of those commands that write a file, run a
// program, or read a file outside the repository.
var gitDenied = []string{"--output", "--no-index", "--open-files-in-pager", "--ext-diff", "--orderfile", "--contents", "--ignore-revs-file", "--exec"}

func gitArgsOK(args []string) error {
	if len(args) == 0 {
		return errors.New("no git command")
	}
	if !gitReadCommands[args[0]] {
		return fmt.Errorf("git %s is not one of the commands that read", args[0])
	}
	for _, a := range args[1:] {
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "--") {
			// git takes any unambiguous prefix of a long option.
			name, _, _ := strings.Cut(a, "=")
			if name == "--ignore-rev" { // not --ignore-revs-file
				continue
			}
			for _, d := range gitDenied {
				if len(name) > 3 && strings.HasPrefix(d, name) {
					return fmt.Errorf("git option %s is not allowed", d)
				}
			}
			if args[0] == "grep" && len(name) > 3 && strings.HasPrefix("--file", name) {
				return fmt.Errorf("git option %s is not allowed", a)
			}
			continue
		}
		// Short options: -O<file> (diff order file, grep's pager), and in a
		// bundle (-nO) too; grep -f and blame -S read a file.
		if !strings.HasPrefix(a, "-") {
			continue
		}
		bad := "O"
		switch args[0] {
		case "grep":
			bad = "Of"
		case "blame":
			bad = "OS"
		}
		if (args[0] == "grep" || args[0] == "blame" || strings.HasPrefix(a, "-O")) && strings.ContainsAny(a[1:], bad) {
			return fmt.Errorf("git option %s is not allowed", a)
		}
	}
	return nil
}

// gitPathsOK: no argument names a file outside the roots. git diff
// compares any two files, as --no-index, outside a repository or when
// one of them is outside it.
func (t *wsTools) gitPathsOK(dir string, args []string) error {
	for _, a := range args[1:] {
		if a == "" || (strings.HasPrefix(a, "-") && a != "-") {
			continue
		}
		p := a
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if _, err := os.Lstat(p); err != nil {
			continue // a revision, a pattern, or a path to come
		}
		r, err := filepath.EvalSymlinks(p)
		if err != nil || !t.inside(r) {
			return fmt.Errorf("%s is outside the directories you may read", a)
		}
	}
	return nil
}

func (t *wsTools) git(ctx context.Context, a map[string]any) (string, error) {
	var args []string
	switch v := a["args"].(type) {
	case []any:
		for _, x := range v {
			args = append(args, fmt.Sprint(x))
		}
	case []string:
		args = v
	case string:
		args = strings.Fields(v)
	}
	if len(args) > 0 && args[0] == "git" {
		args = args[1:]
	}
	if err := gitArgsOK(args); err != nil {
		return "", err
	}
	dir, err := t.resolve(argStr(a, "dir"))
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", t.show(dir))
	}
	if err := t.gitPathsOK(dir, args); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	cmd := proc.CommandContext(ctx, "git", append([]string{"--no-pager", "-c", "color.ui=never", "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "PAGER=cat")
	out := &capWriter{max: toolOutputMax}
	cmd.Stdout, cmd.Stderr = out, out
	err = cmd.Run()
	text := out.buf.String()
	if out.over {
		text += "\n[output cut: narrow it with -n, -L, --stat or a path]\n"
	}
	if err != nil {
		if text == "" {
			return "", fmt.Errorf("git %s: %v", args[0], err)
		}
		return "", fmt.Errorf("git %s: %v\n%s", args[0], err, text)
	}
	if text == "" {
		return "(no output)", nil
	}
	return text, nil
}

// capWriter keeps the first max bytes written to it.
type capWriter struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room < len(p) {
		w.over = true
		w.buf.Write(p[:max(room, 0)])
	} else {
		w.buf.Write(p)
	}
	return len(p), nil
}
