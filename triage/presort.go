package triage

import (
	"bytes"
	"go/scanner"
	"go/token"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Presorter applies deterministic rules. Units it decides never reach an LLM.
type Presorter struct {
	Policy Policy
	// GitattributesGenerated are linguist-generated patterns.
	GitattributesGenerated []string
}

// Presort sets Decision on units a rule can decide and returns the rest.
func (p *Presorter) Presort(units []*Unit, src *Source) (rest []*Unit) {
	files := map[string]FileDiff{}
	for _, f := range src.Files {
		files[f.Path] = f
	}
	for _, u := range units {
		if d, ok := p.rule(u, files[u.File], src); ok {
			d.Source = "rule"
			u.Decision = d
			continue
		}
		rest = append(rest, u)
	}
	return rest
}

func (p *Presorter) rule(u *Unit, f FileDiff, src *Source) (Decision, bool) {
	// Forced-human wins over everything, including "generated".
	if pat, ok := MatchAny(p.Policy.ForceHuman, u.File); ok {
		return Decision{Bucket: BucketHuman, ChangeKind: "config", Reason: "path matches force_human policy " + pat}, true
	}
	// A rename away from a forced path (go.mod -> go.mod.bak) is as much a
	// change to it as an edit.
	if f.OldPath != "" && f.OldPath != u.File {
		if pat, ok := MatchAny(p.Policy.ForceHuman, f.OldPath); ok {
			return Decision{Bucket: BucketHuman, ChangeKind: "config", Reason: "old path " + f.OldPath + " matches force_human policy " + pat}, true
		}
	}
	if f.Binary {
		return Decision{Bucket: BucketHuman, ChangeKind: "binary", Reason: "binary file"}, true
	}
	if pat, ok := MatchAny(p.Policy.Generated, u.File); ok {
		return Decision{Bucket: BucketNone, ChangeKind: "generated", Reason: "generated file (" + pat + ")"}, true
	}
	if pat, ok := MatchAny(p.GitattributesGenerated, u.File); ok {
		return Decision{Bucket: BucketNone, ChangeKind: "generated", Reason: "linguist-generated in .gitattributes (" + pat + ")"}, true
	}
	// The head's header is the PR's own claim: a PR could put it on a
	// hand-written file to keep it from review. It skips review only when
	// the file already carried it at the merge base; a new file, or a
	// header this PR added, is still reviewed.
	if u.Status != StatusDeleted && src.Content != nil && hasGeneratedHeader(src.Content, u.File) {
		if baseGenerated(u, f, src) {
			return Decision{Bucket: BucketNone, ChangeKind: "generated", Reason: "file has a Code generated ... DO NOT EDIT header, as it did at the merge base"}, true
		}
		return Decision{Bucket: BucketSkim, ChangeKind: "generated", Reason: "generated header the merge base did not have (new file or new header), so it is still reviewed", Confidence: 1}, true
	}
	if u.Status == StatusRenamed && len(u.Hunks) == 0 && inertRename(f.OldPath, u.File) {
		return Decision{Bucket: BucketNone, ChangeKind: "rename", Reason: "pure rename, content identical"}, true
	}
	if src.RealChanges != nil && len(u.Hunks) > 0 && !src.RealChanges[u.File] && trailingSpaceOnly(u) && !lineEndingsChanged(u, f, src) && !inMultilineString(u, f, src) {
		return Decision{Bucket: BucketNone, ChangeKind: "format", Reason: "trailing whitespace / blank lines only"}, true
	}
	if strings.HasSuffix(u.File, ".go") {
		if d, ok := goBoilerplate(u, f, src); ok {
			return d, true
		}
	}
	if isFixture(u.File) {
		return Decision{Bucket: BucketSkim, ChangeKind: "fixture", Reason: "test fixture", Confidence: 1}, true
	}
	if isDocs(u.File) {
		return Decision{Bucket: BucketSkim, ChangeKind: "docs", Reason: "documentation file", Confidence: 1}, true
	}
	return Decision{}, false
}

// trailingSpaceOnly reports whether the unit's removed and added lines are
// the same once trailing whitespace and blank lines are dropped. git diff -w
// also ignores indentation and spaces inside a line, which can change a
// string literal or, in some languages, the program. Two cases where
// trailing whitespace does matter are left to the classifier: a line
// ending in a backslash (spaces after it decide whether it continues the
// line, and a blank line after it ends the continuation), and a removed
// and added line that read the same (the diff scanner drops a trailing CR,
// so that pair is a line-ending change).
func trailingSpaceOnly(u *Unit) bool {
	var removed, added, rawRemoved, rawAdded []string
	for _, h := range u.Hunks {
		prev := "" // the line before, on either side
		for _, l := range h.Lines {
			if l == "" || (l[0] != '+' && l[0] != '-') {
				if l != "" && l[0] == ' ' {
					prev = strings.TrimRight(l[1:], " \t\r\f\v")
				}
				continue
			}
			if strings.HasSuffix(prev, "\\") {
				return false
			}
			t := strings.TrimRight(l[1:], " \t\r\f\v")
			prev = t
			if strings.HasSuffix(t, "\\") {
				return false
			}
			if t == "" {
				continue
			}
			if l[0] == '+' {
				added, rawAdded = append(added, t), append(rawAdded, l[1:])
			} else {
				removed, rawRemoved = append(removed, t), append(rawRemoved, l[1:])
			}
		}
	}
	if !slices.Equal(removed, added) {
		return false
	}
	for i := range removed {
		if rawRemoved[i] == rawAdded[i] {
			return false
		}
	}
	return true
}

// lineEndingsChanged reports whether the file gained or lost carriage
// returns, which the diff's lines do not show. Without both sides'
// content it says no; trailingSpaceOnly catches the lines that read the
// same.
func lineEndingsChanged(u *Unit, f FileDiff, src *Source) bool {
	if src.Content == nil || src.BaseContent == nil || u.Status == StatusAdded || u.Status == StatusDeleted {
		return false
	}
	head, err := src.Content(u.File)
	if err != nil {
		return true
	}
	old := f.OldPath
	if old == "" {
		old = u.File
	}
	base, err := src.BaseContent(old)
	if err != nil {
		return true
	}
	return bytes.Count(head, []byte("\r")) != bytes.Count(base, []byte("\r"))
}

// multilineDelims are the string delimiters that can span lines, where
// trailing whitespace and blank lines are part of the value.
var multilineDelims = map[string][]string{
	".py": {`"""`, `'''`}, ".pyi": {`"""`, `'''`},
	".js": {"`"}, ".jsx": {"`"}, ".mjs": {"`"}, ".cjs": {"`"}, ".ts": {"`"}, ".tsx": {"`"}, ".mts": {"`"}, ".cts": {"`"},
}

// inMultilineString reports whether a changed line may sit inside a
// string that spans lines. Go files are tokenized, so only raw strings
// that cover a changed line count. Elsewhere any multi-line delimiter in
// the file (or, without content, the hunks) counts. It errs toward yes:
// that only costs a classifier call.
func inMultilineString(u *Unit, f FileDiff, src *Source) bool {
	ext := strings.ToLower(path.Ext(u.File))
	if ext == ".go" {
		return goRawStringCovers(u, f, src)
	}
	delims := multilineDelims[ext]
	if len(delims) == 0 {
		return false
	}
	var texts [][]byte
	if src.Content != nil && u.Status != StatusDeleted {
		b, err := src.Content(u.File)
		if err != nil {
			return true
		}
		texts = append(texts, b)
	}
	if src.BaseContent != nil && u.Status != StatusAdded {
		old := f.OldPath
		if old == "" {
			old = u.File
		}
		b, err := src.BaseContent(old)
		if err != nil {
			return true
		}
		texts = append(texts, b)
	}
	if len(texts) == 0 {
		for _, h := range u.Hunks {
			texts = append(texts, []byte(strings.Join(h.Lines, "\n")))
		}
	}
	for _, t := range texts {
		for _, d := range delims {
			if bytes.Contains(t, []byte(d)) {
				return true
			}
		}
	}
	return false
}

// goRawStringCovers reports whether a changed line of u falls on a Go raw
// string that spans lines: added lines against the head's strings,
// removed ones against the base's. Without content, a backtick anywhere
// in the hunks counts.
func goRawStringCovers(u *Unit, f FileDiff, src *Source) bool {
	var newLines, oldLines []int
	for _, h := range u.Hunks {
		o, n := h.OldStart, h.NewStart
		for _, l := range h.Lines {
			if l == "" {
				continue
			}
			switch l[0] {
			case ' ':
				o++
				n++
			case '-':
				oldLines = append(oldLines, o)
				o++
			case '+':
				newLines = append(newLines, n)
				n++
			}
		}
	}
	covers := func(content ContentFunc, file string, lines []int) (bool, bool) {
		if content == nil || len(lines) == 0 {
			return false, false
		}
		b, err := content(file)
		if err != nil {
			return false, false
		}
		for _, r := range goRawStrings(b) {
			for _, l := range lines {
				if l >= r[0] && l <= r[1] {
					return true, true
				}
			}
		}
		return false, true
	}
	old := f.OldPath
	if old == "" {
		old = u.File
	}
	inNew, knewNew := covers(src.Content, u.File, newLines)
	inOld, knewOld := covers(src.BaseContent, old, oldLines)
	if inNew || inOld {
		return true
	}
	if (len(newLines) == 0 || knewNew) && (len(oldLines) == 0 || knewOld) {
		return false
	}
	for _, h := range u.Hunks {
		for _, l := range h.Lines {
			if strings.Contains(l, "`") {
				return true
			}
		}
	}
	return false
}

// goRawStrings are the first and last lines of each raw string literal
// in src that spans lines.
func goRawStrings(src []byte) [][2]int {
	fset := token.NewFileSet()
	file := fset.AddFile("", -1, len(src))
	var s scanner.Scanner
	s.Init(file, src, func(token.Position, string) {}, 0)
	var out [][2]int
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			return out
		}
		if tok == token.STRING && strings.HasPrefix(lit, "`") {
			if n := strings.Count(lit, "\n"); n > 0 {
				start := fset.Position(pos).Line
				out = append(out, [2]int{start, start + n})
			}
		}
	}
}

// buildSpecialNames are files whose name alone gives them a role (a
// manifest, an entry point, a package marker); renaming one to or from
// such a name can change what is built or run.
var buildSpecialNames = []string{
	"go.mod", "go.work", "package.json", "dockerfile", "makefile", "gnumakefile", "__init__.py", "__main__.py",
	"conftest.py", "setup.py", "cargo.toml", "pyproject.toml", "build.gradle", "pom.xml", "cmakelists.txt",
}

// buildSpecialStems are base names that are special with any extension.
var buildSpecialStems = []string{"index", "main", "mod", "lib"}

// goOS and goArch are the GOOS and GOARCH values a Go file name suffix
// can constrain (go tool dist list, plus the aliases go/build accepts).
var (
	goOS = []string{"aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "js", "linux", "nacl",
		"netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows", "zos"}
	goArch = []string{"386", "amd64", "amd64p32", "arm", "armbe", "arm64", "arm64be", "loong64", "mips", "mipsle",
		"mips64", "mips64le", "mips64p32", "mips64p32le", "ppc", "ppc64", "ppc64le", "riscv", "riscv64", "s390",
		"s390x", "sparc", "sparc64", "wasm"}
)

// inertRename reports whether renaming oldPath to newPath with the content
// unchanged cannot change what is built or run: the extension stays, and
// neither name has a special role. A Go file must also stay in its
// directory (the directory is the package) and keep its build-constraint
// suffixes (_test, _GOOS, _GOARCH).
func inertRename(oldPath, newPath string) bool {
	if oldPath == "" || newPath == "" {
		return false
	}
	ob, nb := strings.ToLower(path.Base(oldPath)), strings.ToLower(path.Base(newPath))
	oext, next := path.Ext(ob), path.Ext(nb)
	if oext != next {
		return false
	}
	for _, b := range []string{ob, nb} {
		stem := strings.TrimSuffix(b, path.Ext(b))
		if slices.Contains(buildSpecialNames, b) || slices.Contains(buildSpecialStems, stem) || strings.HasPrefix(b, "dockerfile") || strings.HasPrefix(b, ".") {
			return false
		}
	}
	if oext == ".go" {
		if path.Dir(oldPath) != path.Dir(newPath) {
			return false
		}
		return slices.Equal(goNameConstraints(ob), goNameConstraints(nb))
	}
	return true
}

// goNameConstraints are the suffixes of a Go file name that go/build
// reads as constraints: _test, and _GOOS, _GOARCH or _GOOS_GOARCH before
// it.
func goNameConstraints(base string) []string {
	name := strings.TrimSuffix(base, ".go")
	var out []string
	if n, ok := strings.CutSuffix(name, "_test"); ok {
		out, name = append(out, "test"), n
	}
	parts := strings.Split(name, "_")
	if len(parts) < 2 {
		return out
	}
	last := parts[len(parts)-1]
	if slices.Contains(goArch, last) {
		out = append(out, "arch="+last)
		if len(parts) >= 3 && slices.Contains(goOS, parts[len(parts)-2]) {
			out = append(out, "os="+parts[len(parts)-2])
		}
	} else if slices.Contains(goOS, last) {
		out = append(out, "os="+last)
	}
	return out
}

// baseGenerated reports whether the file had a generated header at the
// merge base too, under its old name if the PR renamed it.
func baseGenerated(u *Unit, f FileDiff, src *Source) bool {
	if u.Status == StatusAdded || src.BaseContent == nil {
		return false
	}
	old := f.OldPath
	if old == "" {
		old = u.File
	}
	return hasGeneratedHeader(src.BaseContent, old)
}

// goGenerated is the Go convention (go help generate); the line must come
// before the package clause.
var goGenerated = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// commentOpeners start a comment line in the languages generators write.
var commentOpeners = []string{"<!--", "/*", "//", "--", "#", ";", "*"}

// hasGeneratedHeader reports whether the file starts with a "Code generated
// ... DO NOT EDIT" comment, or outside Go the markers protoc and .NET
// write ("Generated by the protocol buffer compiler.  DO NOT EDIT!",
// "<auto-generated>"). Only the header counts: the comments before the
// first line of code. A generator's own source holds the phrase in a
// string, which is not a comment, and hand-written code comes before it.
func hasGeneratedHeader(content ContentFunc, file string) bool {
	src, err := content(file)
	if err != nil {
		return false
	}
	head := src
	if len(head) > 2048 {
		head = head[:2048]
	}
	isGo := strings.HasSuffix(file, ".go")
	inBlock := false // inside a /* or <!-- comment opened on an earlier line
	for _, line := range bytes.Split(head, []byte("\n")) {
		raw := strings.TrimRight(string(line), " \t\r")
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if isGo {
			if goGenerated.MatchString(raw) {
				return true
			}
			switch {
			case inBlock:
				inBlock = !strings.Contains(s, "*/")
			case strings.HasPrefix(s, "//"):
			case strings.HasPrefix(s, "/*"):
				inBlock = !strings.Contains(s[2:], "*/")
			default:
				return false // the package clause or code: past the header
			}
			continue
		}
		text, ok := "", false
		if inBlock {
			text, ok = s, true
			if strings.HasPrefix(text, "*") && !strings.HasPrefix(text, "*/") {
				text = text[1:]
			}
		} else if strings.HasPrefix(s, "#!") || strings.HasPrefix(s, "<?") {
			continue // a shebang, or an XML declaration or PHP open tag
		} else {
			for _, o := range commentOpeners {
				if strings.HasPrefix(s, o) {
					text, ok = strings.TrimLeft(s[len(o):], "/#;-*!"), true
					inBlock = (o == "/*" && !strings.Contains(s[2:], "*/")) || (o == "<!--" && !strings.Contains(s[4:], "-->"))
					break
				}
			}
		}
		if !ok {
			return false
		}
		if inBlock && (strings.Contains(s, "*/") || strings.Contains(s, "-->")) {
			inBlock = false
		}
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(text), "*/"), "-->"))
		if (strings.HasPrefix(text, "Code generated") || strings.HasPrefix(text, "Generated by the protocol buffer compiler")) && strings.Contains(text, "DO NOT EDIT") {
			return true
		}
		if strings.HasPrefix(text, "<auto-generated") {
			return true
		}
	}
	return false
}

// fixtureDirs hold only what the tests read: neither Go (testdata) nor
// Maven (src/test/resources) builds them, so any file there is a fixture.
var fixtureDirs = []string{"testdata/", "src/test/resources/"}

// dataDirs hold fixtures next to test or helper code (a JS fixtures/
// with factory modules, tests/ itself), so only data files there are
// fixtures; code keeps its review.
var dataDirs = []string{"fixtures/", "__fixtures__/", "test-fixtures/", "test_fixtures/", "test/", "tests/", "__tests__/", "spec/"}

// dataExts are files a program reads rather than runs.
var dataExts = map[string]bool{
	".txt": true, ".json": true, ".jsonl": true, ".ndjson": true, ".yaml": true, ".yml": true, ".xml": true,
	".csv": true, ".tsv": true, ".html": true, ".htm": true, ".log": true, ".out": true, ".golden": true,
	".expected": true, ".toml": true, ".properties": true, ".ini": true, ".env": true, ".eml": true, ".har": true,
}

// testConfigs are files in a test directory that change how the tests
// build or run, not what they read.
var testConfigs = []string{"tsconfig*.json", "jest.config.*", "vitest.config.*", "pytest.ini", "tox.ini", "setup.cfg", "*.runsettings"}

// isFixture reports whether file is input or expected output the tests
// read, never built or shipped. Its diff is context for the tests that
// use it, not a change to review on its own.
func isFixture(file string) bool {
	lower := strings.ToLower(file)
	base := path.Base(lower)
	if strings.HasPrefix(base, ".") {
		return false // tool config: .eslintrc.json, .babelrc
	}
	if _, ok := MatchAny(notDocs, lower); ok {
		return false
	}
	if _, ok := MatchAny(testConfigs, lower); ok {
		return false
	}
	if _, ok := MatchAny(fixtureDirs, lower); ok {
		return true
	}
	_, ok := MatchAny(dataDirs, lower)
	return ok && dataExts[path.Ext(base)]
}

// notDocs are .txt files that a tool reads: dependency lists, build files
// and web server config. They change what gets installed, built or served.
var notDocs = []string{
	"*requirements*.txt", "requirements/", "constraints*.txt", "cmakelists.txt",
	"robots.txt", "runtime.txt",
}

func isDocs(file string) bool {
	lower := strings.ToLower(file)
	if strings.HasSuffix(lower, ".txt") {
		_, manifest := MatchAny(notDocs, lower)
		return !manifest
	}
	return strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".rst")
}

var (
	blankOrDotImport = regexp.MustCompile(`^[+-]\s*(import\s+)?[_.]\s+"`)
	importLine       = regexp.MustCompile(`^[+-]\s*(import\s*\(?|\)|(\w+\s+)?"[^"]*"|import\s+(\w+\s+)?"[^"]*")?\s*(//.*)?$`)
	packageLine      = regexp.MustCompile(`^[+-]\s*(package\s+\w+)?\s*(//.*)?$`)
)

var (
	importSpec  = regexp.MustCompile(`^[+-]\s*(?:import\s*\(?\s*)?(?:(\w+)\s+)?"([^"]*)"`)
	majorSuffix = regexp.MustCompile(`^v[0-9]+$`)
)

type goImport struct{ alias, path string }

// importSwap reports whether the changed import lines rebind a name to
// another package: a spec removed and one added under the same name, like
// "crypto/rand" -> rand "math/rand" or foo "x/v1" -> foo "x/v2". The code
// using the name still compiles, so the compiler proves nothing. Specs
// removed and re-added unchanged (reordering, regrouping) cancel out.
func importSwap(changed []string) bool {
	var removed, added []goImport
	for _, l := range changed {
		m := importSpec.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if l[0] == '-' {
			removed = append(removed, goImport{m[1], m[2]})
		} else {
			added = append(added, goImport{m[1], m[2]})
		}
	}
	for i := 0; i < len(removed); i++ {
		if j := slices.Index(added, removed[i]); j >= 0 {
			added = slices.Delete(added, j, j+1)
			removed = slices.Delete(removed, i, i+1)
			i--
		}
	}
	for _, r := range removed {
		for _, a := range added {
			if r.path != a.path && sharesName(importNames(r), importNames(a)) {
				return true
			}
		}
	}
	return false
}

// importNames is the name an import binds: its alias, or else the package
// name, which only the package clause gives for sure. The guesses cover
// the usual ways it differs from the last path element: a /vN major
// version suffix, gopkg.in's .vN, and go-/-go around the name. A wrong
// guess only costs a classifier call.
func importNames(im goImport) []string {
	if im.alias != "" {
		return []string{im.alias}
	}
	elems := strings.Split(im.path, "/")
	last := elems[len(elems)-1]
	if majorSuffix.MatchString(last) && len(elems) > 1 {
		last = elems[len(elems)-2]
	}
	names := []string{last}
	for _, part := range strings.FieldsFunc(last, func(r rune) bool { return r == '-' || r == '.' }) {
		if part != "go" && !majorSuffix.MatchString(part) {
			names = append(names, part)
		}
	}
	return names
}

func sharesName(a, b []string) bool {
	for _, n := range a {
		if slices.Contains(b, n) {
			return true
		}
	}
	return false
}

// goDirective is a changed line that sets what the file builds into: the
// package clause, a build constraint, a //go: directive or a cgo flag.
var goDirective = regexp.MustCompile(`^[+-]\s*(package\s|//go:|//\s*\+build|//line |(//\s*)?#cgo\b)`)

// cgoImport matches import "C", alone or in a group; the comment before it
// is C code (the cgo preamble).
var cgoImport = regexp.MustCompile(`(?m)^[ +-]?\s*(import\s+)?"C"\s*(//.*)?$`)

// goBoilerplate decides units whose changed lines are only import specs or
// the blank lines and comments around the package clause. Blank and dot
// imports run init code, and a name moved to another package changes what
// its uses call, so those go to the classifier; so does a changed package
// clause or build directive, and any change in a cgo file, whose comments
// can be C code.
func goBoilerplate(u *Unit, f FileDiff, src *Source) (Decision, bool) {
	var changed []string
	for _, h := range u.Hunks {
		for _, l := range h.Lines {
			if strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") {
				changed = append(changed, l)
				if goDirective.MatchString(l) {
					return Decision{}, false
				}
			}
		}
		if cgoImport.MatchString(strings.Join(h.Lines, "\n")) {
			return Decision{}, false
		}
	}
	if len(changed) == 0 || usesCgo(u, f, src) {
		return Decision{}, false
	}
	all := func(re *regexp.Regexp) bool {
		for _, l := range changed {
			if !re.MatchString(l) {
				return false
			}
		}
		return true
	}
	switch {
	case u.Symbol == "imports" && all(importLine):
		for _, l := range changed {
			if blankOrDotImport.MatchString(l) {
				return Decision{}, false
			}
		}
		if importSwap(changed) {
			return Decision{}, false
		}
		return Decision{Bucket: BucketNone, ChangeKind: "format", Reason: "import list only (the compiler rejects unused imports; uses are in other units)"}, true
	case u.Symbol == "" && all(packageLine):
		return Decision{Bucket: BucketNone, ChangeKind: "format", Reason: "comments / blank lines around the package clause only"}, true
	}
	return Decision{}, false
}

// usesCgo reports whether either side of the file imports "C".
func usesCgo(u *Unit, f FileDiff, src *Source) bool {
	old := f.OldPath
	if old == "" {
		old = u.File
	}
	for _, c := range []struct {
		content ContentFunc
		file    string
	}{{src.Content, u.File}, {src.BaseContent, old}} {
		if c.content == nil {
			continue
		}
		if b, err := c.content(c.file); err == nil && cgoImport.Match(b) {
			return true
		}
	}
	return false
}
