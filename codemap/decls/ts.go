// Package decls finds declarations in TypeScript, Java, Python, C# and Rust
// files and operations in OpenAPI specs. The code-map indexer and pr-manager
// share it, so a hunk is resolved to the same names the map was built with.
package decls

import (
	"path"
	"strings"
)

type Tok struct {
	Kind byte // 'i' ident, 's' string, 't' template, 'p' punct, 'n' number (Python, Rust)
	Text string
	Line int
	// for templates: raw parts with ${expr} replaced by "\x00expr\x00"
}

// Lex tokenizes TS/JS source without JSX.
func Lex(src string) []Tok { return lex(src, false) }

// lex tokenizes TS/JS source. With jsx, an element in expression position is
// read as markup: its tag names and the code in its {expressions} become
// tokens, attribute names and text children do not.
func lex(src string, jsx bool) []Tok {
	l := &lexer{src: src, n: len(src), line: 1, jsx: jsx}
	l.code(false)
	return l.toks
}

type lexer struct {
	src       string
	i, n      int
	line      int
	jsx       bool
	toks      []Tok
	last      byte // the last significant token: 'i', 's', a punctuation byte, or 0
	lastIdent string
}

func (l *lexer) emit(kind byte, text string, line int) {
	l.toks = append(l.toks, Tok{Kind: kind, Text: text, Line: line})
}

func (l *lexer) regexAllowed() bool {
	switch l.last {
	case 0, '(', ',', '=', ':', '[', '!', '&', '|', '?', '{', '}', ';', '+', '-', '*', '%', '<', '>', '~', '^':
		return true
	case 'i':
		switch l.lastIdent {
		case "return", "typeof", "case", "do", "else", "in", "of", "new", "delete", "void", "throw", "yield", "await":
			return true
		}
	}
	return false
}

// jsxAllowed reports whether a '<' here can open a JSX element: where an
// expression starts, never after a value (a < b, f<T>()).
func (l *lexer) jsxAllowed() bool {
	switch l.last {
	case 0, '(', ',', '=', ':', '[', '!', '&', '|', '?', '{', ';':
		return true
	case '>': // =>
		k := len(l.toks)
		return k >= 2 && l.toks[k-2].Kind == 'p' && l.toks[k-2].Text == "="
	case 'i':
		switch l.lastIdent {
		case "return", "case", "default", "yield", "await", "else", "do":
			return true
		}
	}
	return false
}

// code lexes until the end of the source, or with untilBrace until the '}'
// matching an already consumed '{', which it consumes without emitting. It
// reports whether it found that brace (always true without untilBrace).
func (l *lexer) code(untilBrace bool) bool {
	src, n := l.src, l.n
	depth := 0
	for l.i < n {
		c := src[l.i]
		switch {
		case c == '\n':
			l.line++
			l.i++
		case c == ' ' || c == '\t' || c == '\r':
			l.i++
		case c == '/' && l.i+1 < n && src[l.i+1] == '/':
			for l.i < n && src[l.i] != '\n' {
				l.i++
			}
		case c == '/' && l.i+1 < n && src[l.i+1] == '*':
			l.blockComment()
		case c == '"' || c == '\'':
			start := l.line
			l.i++
			var b strings.Builder
			for l.i < n && src[l.i] != c && src[l.i] != '\n' {
				if src[l.i] == '\\' && l.i+1 < n {
					b.WriteByte(src[l.i+1])
					l.i += 2
					continue
				}
				b.WriteByte(src[l.i])
				l.i++
			}
			if l.i < n && src[l.i] == c {
				l.i++
			}
			l.emit('s', b.String(), start)
			l.last = 's'
		case c == '`':
			start := l.line
			l.i++
			s := l.template()
			l.emit('t', s, start)
			l.last = 's'
		case c == '<' && l.jsx && l.jsxAllowed() && l.tryJSX():
			l.last = 's'
		case c == '/' && (l.last == '<' || (l.i+1 < n && src[l.i+1] == '>')):
			// JSX closing tag or self-closing tag, not a regex
			l.emit('p', "/", l.line)
			l.last = '/'
			l.i++
		case c == '/' && l.regexAllowed():
			l.i++
			inClass := false
			for l.i < n && src[l.i] != '\n' {
				if src[l.i] == '\\' {
					l.i = min(l.i+2, n)
					continue
				}
				if src[l.i] == '[' {
					inClass = true
				} else if src[l.i] == ']' {
					inClass = false
				} else if src[l.i] == '/' && !inClass {
					l.i++
					break
				}
				l.i++
			}
			for l.i < n && isIdentByte(src[l.i]) {
				l.i++
			}
			l.last = 's'
		case isIdentStart(c):
			j := l.i
			for j < n && isIdentByte(src[j]) {
				j++
			}
			w := src[l.i:j]
			l.emit('i', w, l.line)
			l.last = 'i'
			l.lastIdent = w
			l.i = j
		case c >= '0' && c <= '9':
			j := l.i
			for j < n && (isIdentByte(src[j]) || src[j] == '.') {
				j++
			}
			l.emit('n', src[l.i:j], l.line)
			l.last = 's'
			l.i = j
		default:
			if c == '{' {
				depth++
			} else if c == '}' {
				if depth == 0 && untilBrace {
					l.i++
					l.last = '}'
					return true
				}
				depth--
			}
			l.emit('p', string(c), l.line)
			l.last = c
			l.i++
		}
	}
	return !untilBrace
}

func (l *lexer) blockComment() {
	l.i += 2
	for l.i+1 < l.n && !(l.src[l.i] == '*' && l.src[l.i+1] == '/') {
		if l.src[l.i] == '\n' {
			l.line++
		}
		l.i++
	}
	l.i = min(l.i+2, l.n) // an unclosed /* runs to the end
}

// template reads a template literal after its opening backtick. The code in
// each ${hole} is lexed in place, as $ { tokens }; the returned text has
// each hole replaced by "\x00expr\x00".
func (l *lexer) template() string {
	src, n := l.src, l.n
	var b strings.Builder
	for l.i < n {
		c := src[l.i]
		switch {
		case c == '\\' && l.i+1 < n:
			b.WriteByte(src[l.i+1])
			if src[l.i+1] == '\n' {
				l.line++
			}
			l.i += 2
		case c == '`':
			l.i++
			return b.String()
		case c == '$' && l.i+1 < n && src[l.i+1] == '{':
			// The hole's tokens are bracketed so they stay inside the
			// expression holding the template.
			l.emit('p', "$", l.line)
			l.emit('p', "{", l.line)
			l.i += 2
			start := l.i
			l.last = '{'
			l.code(true)
			l.emit('p', "}", l.line)
			end := max(start, l.i-1)
			b.WriteString("\x00" + strings.TrimSpace(src[start:end]) + "\x00")
		default:
			if c == '\n' {
				l.line++
			}
			b.WriteByte(c)
			l.i++
		}
	}
	return b.String()
}

// tryJSX reads a JSX element at '<'. When the markup does not hold together
// (a type argument, a comparison the heuristics let through) it restores the
// lexer and reports false, and '<' is lexed as punctuation.
func (l *lexer) tryJSX() bool {
	i, line, k := l.i, l.line, len(l.toks)
	if l.element() {
		return true
	}
	l.i, l.line, l.toks = i, line, l.toks[:k]
	return false
}

// element reads one element (or fragment) from its '<' to its end.
func (l *lexer) element() bool {
	l.i++ // <
	l.jsxSpace()
	if l.i < l.n && l.src[l.i] == '>' { // <>
		l.i++
		return l.children("")
	}
	line := l.line
	name := l.tagName()
	if name == "" {
		return false
	}
	l.jsxSpace()
	// <T,>(x) => x and <T extends U>(x) => x are generic arrows.
	if l.i < l.n && l.src[l.i] == ',' || strings.HasPrefix(l.src[l.i:], "extends ") {
		return false
	}
	l.tagTokens(name, line)
	if l.i < l.n && l.src[l.i] == '<' { // <Table<Row> ...>: type arguments
		if !l.typeArgs() {
			return false
		}
	}
	for {
		l.jsxSpace()
		if l.i >= l.n {
			return false
		}
		switch c := l.src[l.i]; {
		case c == '/':
			if l.i+1 < l.n && l.src[l.i+1] == '>' {
				l.i += 2
				return true
			}
			return false
		case c == '>':
			l.i++
			return l.children(name)
		case c == '{': // {...spread}
			if !l.container() {
				return false
			}
		case isIdentStart(c):
			for l.i < l.n && (isIdentByte(l.src[l.i]) || l.src[l.i] == '-' || l.src[l.i] == ':') {
				l.i++
			}
			l.jsxSpace()
			if l.i >= l.n || l.src[l.i] != '=' {
				continue // boolean attribute
			}
			l.i++
			l.jsxSpace()
			if l.i >= l.n {
				return false
			}
			switch q := l.src[l.i]; q {
			case '"', '\'':
				start := l.line
				j := l.i + 1
				for j < l.n && l.src[j] != q {
					if l.src[j] == '\n' {
						l.line++
					}
					j++
				}
				if j >= l.n {
					return false
				}
				l.emit('s', l.src[l.i+1:j], start)
				l.i = j + 1
			case '{':
				if !l.container() {
					return false
				}
			case '<':
				if !l.element() {
					return false
				}
			default:
				return false
			}
		default:
			return false
		}
	}
}

// typeArgs skips a component's <type arguments>.
func (l *lexer) typeArgs() bool {
	depth := 0
	for ; l.i < l.n; l.i++ {
		switch l.src[l.i] {
		case '<':
			depth++
		case '>':
			if depth--; depth == 0 {
				l.i++
				return true
			}
		case '\n':
			l.line++
		case '{', '}', ';':
			return false
		}
	}
	return false
}

// children reads an element's children up to and including its closing tag,
// which must close name.
func (l *lexer) children(name string) bool {
	for l.i < l.n {
		switch c := l.src[l.i]; c {
		case '<':
			j := l.i + 1
			for j < l.n && (l.src[j] == ' ' || l.src[j] == '\t' || l.src[j] == '\n' || l.src[j] == '\r') {
				j++
			}
			if j < l.n && l.src[j] == '/' {
				l.i = j + 1
				l.jsxSpace()
				closing := l.tagName()
				l.jsxSpace()
				if closing != name || l.i >= l.n || l.src[l.i] != '>' {
					return false
				}
				l.i++
				return true
			}
			if !l.element() {
				return false
			}
		case '{':
			if !l.container() {
				return false
			}
		case '\n':
			l.line++
			l.i++
		default:
			l.i++
		}
	}
	return false
}

// container lexes a {expression} of an element as code, braces included.
func (l *lexer) container() bool {
	l.emit('p', "{", l.line)
	l.i++
	l.last = '{'
	if !l.code(true) {
		return false
	}
	l.emit('p', "}", l.line)
	return true
}

func (l *lexer) tagName() string {
	j := l.i
	if j >= l.n || !isIdentStart(l.src[j]) {
		return ""
	}
	for j < l.n && (isIdentByte(l.src[j]) || l.src[j] == '.' || l.src[j] == '-' || l.src[j] == ':') {
		j++
	}
	name := l.src[l.i:j]
	l.i = j
	return name
}

// tagTokens emits a component name (Foo, ns.Foo) as identifier tokens.
// Intrinsic elements (div, my-el, svg:rect) are not code.
func (l *lexer) tagTokens(name string, line int) {
	if strings.ContainsAny(name, "-:") || (name[0] >= 'a' && name[0] <= 'z' && !strings.Contains(name, ".")) {
		return
	}
	for k, part := range strings.Split(name, ".") {
		if k > 0 {
			l.emit('p', ".", line)
		}
		l.emit('i', part, line)
	}
}

// jsxSpace skips whitespace and comments inside a tag.
func (l *lexer) jsxSpace() {
	for l.i < l.n {
		switch c := l.src[l.i]; {
		case c == '\n':
			l.line++
			l.i++
		case c == ' ' || c == '\t' || c == '\r':
			l.i++
		case c == '/' && l.i+1 < l.n && l.src[l.i+1] == '/':
			for l.i < l.n && l.src[l.i] != '\n' {
				l.i++
			}
		case c == '/' && l.i+1 < l.n && l.src[l.i+1] == '*':
			l.blockComment()
		default:
			return
		}
	}
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentByte(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

type TSDecl struct {
	Name     string
	Kind     string
	Exported bool
	IsDef    bool // export default
	Start    int  // token index
	End      int  // token index (exclusive)
	Line     int
	EndLine  int
	StrVal   string // for const X = "..." / `...`
	HasStr   bool
	AliasOf  string // export default Foo -> Foo
	Owner    string // a class member's class
}

type Import struct {
	Local    string
	Imported string // name, "default", or "*"
	Spec     string
	Require  bool // CommonJS: const x = require('...')
}

type Reexport struct {
	Exported string // name exposed by this file, or "*" for export *
	Imported string
	Spec     string
}

type TSFile struct {
	Path      string
	Toks      []Tok
	Decls     []*TSDecl
	Imports   []Import
	Reexports []Reexport
	LocalExp  map[string]string // export { a as b } -> b:a
	SideFx    []string          // import './x', dynamic import('./x')
	Lines     int
	Generated bool
}

var tsDeclKw = map[string]string{
	"function": "function", "class": "class", "interface": "interface", "type": "type",
	"enum": "enum", "const": "const", "let": "let", "var": "var", "namespace": "namespace",
	"module": "namespace",
}

// IsJSXPath reports whether p may hold JSX: everything but .ts, whose
// <T>x casts and generic arrows would read as markup. An unknown path ("")
// may.
func IsJSXPath(p string) bool {
	switch path.Ext(p) {
	case ".ts", ".mts", ".cts":
		return false
	}
	return true
}

func ParseTS(p, src string) *TSFile {
	f := &TSFile{Path: p, Toks: lex(src, IsJSXPath(p)), LocalExp: map[string]string{}, Lines: strings.Count(src, "\n") + 1}
	head := src
	if len(head) > 400 {
		head = head[:400]
	}
	f.Generated = strings.Contains(head, "Generated by orval") || strings.Contains(head, "Do not edit") || strings.Contains(head, "DO NOT EDIT") || strings.Contains(head, "auto-generated")
	t := f.Toks
	depth := 0
	var cur *TSDecl
	closeCur := func(at int) {
		if cur != nil {
			cur.End = at
			if at > 0 && at-1 < len(t) {
				cur.EndLine = tsEndLine(t[at-1])
			}
			f.Decls = append(f.Decls, cur)
			cur = nil
		}
	}
	// deco is the token index of the first decorator (@Component(...))
	// waiting for the declaration it decorates, or -1.
	deco := -1
	open := func(d *TSDecl) {
		if deco >= 0 {
			d.Start, d.Line = deco, t[deco].Line
			deco = -1
		}
		cur = d
	}
	stmtStart := true
	for i := 0; i < len(t); i++ {
		tk := t[i]
		if tk.Kind == 'p' {
			switch tk.Text {
			case "{", "(", "[":
				depth++
			case "}", ")", "]":
				depth--
				if depth < 0 {
					depth = 0
				}
				if depth == 0 && tk.Text == "}" {
					stmtStart = true
				}
			case ";":
				if depth == 0 {
					stmtStart = true
				}
			case "@":
				if depth == 0 && deco < 0 && (stmtStart || i == 0 || t[i-1].Line != tk.Line) {
					closeCur(i)
					deco = i
				}
			}
			continue
		}
		if depth != 0 || tk.Kind != 'i' {
			if tk.Kind != 'i' {
				stmtStart = false
			}
			continue
		}
		if deco >= 0 && i > 0 && (isP(t[i-1], "@") || isP(t[i-1], ".")) {
			continue // the decorator's name
		}
		// A top-level statement may start after ; or } or on a new line.
		newLine := i > 0 && tsEndLine(t[i-1]) != tk.Line
		if !stmtStart && !newLine && deco < 0 {
			continue
		}
		// A statement that is not a declaration ends the one before it:
		// after ; or }, or on a new line that cannot continue it.
		ends := stmtStart || deco >= 0 || (newLine && tsEndsStmt(t, i-1) && !tsContinues[tk.Text])
		stmtStart = false
		// Statements that may not be declarations: imports, CommonJS
		// require and exports.
		switch tk.Text {
		case "import":
			if i+1 < len(t) && t[i+1].Kind == 'p' && (t[i+1].Text == "(" || t[i+1].Text == ".") {
				if ends {
					closeCur(i)
				}
				continue // dynamic import or import.meta; handled in body scan
			}
			closeCur(i)
			deco = -1
			i = f.parseImport(i)
			stmtStart = true
			continue
		case "module", "exports":
			if d := f.cjsExport(i); d != nil {
				closeCur(i)
				open(d)
				continue
			}
		case "const", "let", "var":
			if j, ok := f.parseRequire(i); ok {
				closeCur(i)
				deco = -1
				i = j
				stmtStart = true
				continue
			}
		}
		switch tk.Text {
		case "export":
			j := i + 1
			if j < len(t) && t[j].Kind == 'p' && (t[j].Text == "{" || t[j].Text == "*") {
				closeCur(i)
				deco = -1
				i = f.parseExportList(i)
				stmtStart = true
				continue
			}
			if j < len(t) && t[j].Kind == 'i' && t[j].Text == "type" && j+1 < len(t) && t[j+1].Kind == 'p' && t[j+1].Text == "{" {
				closeCur(i)
				deco = -1
				i = f.parseExportList(j)
				stmtStart = true
				continue
			}
			closeCur(i)
			d := &TSDecl{Exported: true, Start: i, Line: tk.Line}
			if j < len(t) && t[j].Text == "default" {
				d.IsDef = true
				j++
			}
			for j < len(t) && t[j].Kind == 'i' && (t[j].Text == "declare" || t[j].Text == "async" || t[j].Text == "abstract") {
				j++
			}
			if j < len(t) && t[j].Kind == 'i' {
				if k, ok := tsDeclKw[t[j].Text]; ok {
					d.Kind = k
					if j+1 < len(t) && t[j+1].Kind == 'p' && t[j+1].Text == "*" {
						j++
					}
					if j+1 < len(t) && t[j+1].Kind == 'i' {
						d.Name = t[j+1].Text
					}
				} else if d.IsDef {
					d.Kind = "default"
					d.AliasOf = t[j].Text
				}
			}
			if d.IsDef {
				if d.Name == "" {
					d.Name = "default"
				}
				if d.Kind == "" {
					d.Kind = "default"
				}
			}
			if d.Name == "" {
				d.Name = "<export>"
			}
			f.stringInit(d, j)
			open(d)
			continue
		default:
			j := i
			for j < len(t) && t[j].Kind == 'i' && (t[j].Text == "declare" || t[j].Text == "async" || t[j].Text == "abstract") {
				j++
			}
			if j < len(t) && t[j].Kind == 'i' {
				if k, ok := tsDeclKw[t[j].Text]; ok {
					// `type` and `module` are also common identifiers.
					if (t[j].Text == "type" || t[j].Text == "module") && !(j+1 < len(t) && t[j+1].Kind == 'i') {
						if ends {
							closeCur(i)
							deco = -1
						}
						continue
					}
					closeCur(i)
					d := &TSDecl{Kind: k, Start: i, Line: tk.Line}
					if j+1 < len(t) && t[j+1].Kind == 'p' && t[j+1].Text == "*" {
						j++
					}
					if j+1 < len(t) && t[j+1].Kind == 'i' {
						d.Name = t[j+1].Text
					} else {
						d.Name = "<destructure>"
					}
					f.stringInit(d, j)
					open(d)
					continue
				}
			}
			if ends {
				closeCur(i)
				deco = -1
			}
		}
	}
	closeCur(len(t))
	// Local export lists mark declarations as exported.
	for _, local := range f.LocalExp {
		for _, d := range f.Decls {
			if d.Name == local {
				d.Exported = true
			}
		}
	}
	// Class members follow their class.
	var all []*TSDecl
	for _, d := range f.Decls {
		all = append(all, d)
		if d.Kind == "class" && d.Name != "<export>" {
			all = append(all, f.members(d)...)
		}
	}
	f.Decls = all
	return f
}

// tsEndLine is the last line of tk: a template literal spans lines.
func tsEndLine(tk Tok) int {
	if tk.Kind == 't' {
		return tk.Line + strings.Count(tk.Text, "\n")
	}
	return tk.Line
}

// tsContinues are words that continue the expression on the line before.
var tsContinues = map[string]bool{"as": true, "satisfies": true, "instanceof": true, "in": true, "of": true, "extends": true, "implements": true}

// tsOpenWords are words after which an expression has to go on.
var tsOpenWords = map[string]bool{"return": true, "new": true, "typeof": true, "await": true, "yield": true, "extends": true, "implements": true,
	"as": true, "satisfies": true, "in": true, "of": true, "instanceof": true, "case": true, "delete": true, "void": true, "throw": true, "keyof": true,
	"export": true, "default": true, "const": true, "let": true, "var": true, "type": true, "interface": true, "class": true, "function": true,
	"async": true, "declare": true, "abstract": true, "readonly": true, "enum": true, "namespace": true, "import": true, "from": true}

// tsEndsStmt reports whether a line break after t[k] can end a statement:
// t[k] ends a value (a name, a literal, a closing bracket, a type's >).
func tsEndsStmt(t []Tok, k int) bool {
	if k < 0 {
		return true
	}
	switch tk := t[k]; tk.Kind {
	case 'i':
		return !tsOpenWords[tk.Text]
	case 's', 't', 'n':
		return true
	case 'p':
		switch tk.Text {
		case ")", "]", "}", ";":
			return true
		case ">":
			return !(k > 0 && isP(t[k-1], "=")) // not =>
		}
	}
	return false
}

// tsMemberMods are the modifiers a class member may start with.
var tsMemberMods = map[string]bool{"public": true, "private": true, "protected": true, "static": true, "readonly": true, "abstract": true,
	"override": true, "declare": true, "async": true, "accessor": true}

// members lists the methods of class c (its constructor, methods, accessors
// and function-valued properties) as Class.name. Overloads and a getter with
// its setter share one declaration spanning all of them. Plain fields stay
// part of the class.
func (f *TSFile) members(c *TSDecl) []*TSDecl {
	t := f.Toks
	end := min(c.End, len(t))
	// The body is the first { outside parentheses (extends mixin({...})).
	open, paren := -1, 0
	for k := c.Start; k < end; k++ {
		switch {
		case isP(t[k], "(") || isP(t[k], "["):
			paren++
		case isP(t[k], ")") || isP(t[k], "]"):
			paren--
		case isP(t[k], "{") && paren == 0:
			open = k
		}
		if open >= 0 {
			break
		}
	}
	if open < 0 {
		return nil
	}
	var out []*TSDecl
	byName := map[string]*TSDecl{}
	k := open + 1
	for k < end && !isP(t[k], "}") {
		start := k
		// Decorators: @name, @a.b, @name(...).
		for k < end && isP(t[k], "@") {
			k++
			for k < end && (t[k].Kind == 'i' || isP(t[k], ".")) {
				k++
			}
			if k < end && isP(t[k], "(") {
				k = tsSkip(t, k, end)
			}
		}
		for k+1 < end && t[k].Kind == 'i' && tsMemberMods[t[k].Text] && !isP(t[k+1], "(") && !isP(t[k+1], "=") && !isP(t[k+1], ":") {
			k++
		}
		if k+1 < end && (isKw(t[k], "get") || isKw(t[k], "set")) && (t[k+1].Kind == 'i' || isP(t[k+1], "#")) {
			k++
		}
		if k < end && isP(t[k], "*") {
			k++
		}
		name := ""
		switch {
		case k+1 < end && isP(t[k], "#") && t[k+1].Kind == 'i':
			name = "#" + t[k+1].Text
			k += 2
		case k < end && (t[k].Kind == 'i' || t[k].Kind == 's'):
			name = t[k].Text
			k++
		}
		if k < end && (isP(t[k], "?") || isP(t[k], "!")) {
			k++
		}
		fn := false
		switch {
		case name == "":
		case k < end && (isP(t[k], "(") || isP(t[k], "<")):
			fn = true // method
		case k < end && (isP(t[k], "=") || isP(t[k], ":")):
			fn = tsFuncValued(t, k, end)
		}
		stop := max(tsMemberEnd(t, k, end), start+1)
		if fn {
			d := &TSDecl{Name: c.Name + "." + name, Kind: "method", Owner: c.Name, Start: start, End: stop,
				Line: t[start].Line, EndLine: tsEndLine(t[stop-1]), Exported: c.Exported && name[0] != '#' && !tsPrivate(t, start, k)}
			if old := byName[d.Name]; old != nil {
				old.End, old.EndLine = d.End, d.EndLine
			} else {
				byName[d.Name] = d
				out = append(out, d)
			}
		}
		k = stop
	}
	return out
}

// tsSkip returns the index after the bracket group opening at t[k].
func tsSkip(t []Tok, k, end int) int {
	depth := 0
	for ; k < end; k++ {
		switch {
		case isP(t[k], "(") || isP(t[k], "[") || isP(t[k], "{"):
			depth++
		case isP(t[k], ")") || isP(t[k], "]") || isP(t[k], "}"):
			depth--
			if depth == 0 {
				return k + 1
			}
		}
	}
	return end
}

// tsMemberEnd returns the index after the class member that continues at
// t[from] (past its decorators and modifiers): after its ;, after its
// body's }, or at a line break that ends it.
func tsMemberEnd(t []Tok, from, end int) int {
	depth := 0
	for k := from; k < end; k++ {
		tk := t[k]
		if depth == 0 && isP(tk, "}") {
			return k // the class body's }
		}
		if depth == 0 && k > from && tk.Line != tsEndLine(t[k-1]) && tsEndsStmt(t, k-1) && (tk.Kind == 'i' || tk.Kind == 's' || isP(tk, "@") || isP(tk, "#") || isP(tk, "*") || isP(tk, "[")) && !tsContinues[tk.Text] {
			return k
		}
		switch {
		case isP(tk, "(") || isP(tk, "["):
			depth++
		case isP(tk, ")") || isP(tk, "]"):
			depth--
		case isP(tk, "{"):
			depth++
		case isP(tk, "}"):
			depth--
			// A method body ends the member; an object or arrow body
			// ends it only with the line (x = () => {...}.bind(this)).
			if depth == 0 && tsIsMethodBody(t, k) {
				return k + 1
			}
		case isP(tk, ";") && depth == 0:
			return k + 1
		}
	}
	return end
}

// tsIsMethodBody reports whether the } at t[k] closes a method body: its {
// follows the parameter list's ) or a return type, not an = or =>.
func tsIsMethodBody(t []Tok, k int) bool {
	depth := 0
	for j := k; j >= 0; j-- {
		switch {
		case isP(t[j], "}"):
			depth++
		case isP(t[j], "{"):
			depth--
			if depth == 0 {
				for p := j - 1; p >= 0; p-- {
					switch {
					case isP(t[p], "="), isP(t[p], ">") && p > 0 && isP(t[p-1], "="):
						return false
					case isP(t[p], ")"):
						// Method params, unless an arrow's: x = (a) => {.
						return true
					case isP(t[p], ":"):
						return true // return type after params: f(): T {
					case isP(t[p], ";"), isP(t[p], "{"), isP(t[p], "}"):
						return true
					}
				}
				return true
			}
		}
	}
	return false
}

// tsFuncValued reports whether the property whose = or : is at t[k] holds
// a function: an arrow function or a function expression.
func tsFuncValued(t []Tok, k, end int) bool {
	if isP(t[k], ":") {
		// Skip the type annotation to the initializer.
		depth := 0
		for k++; k < end; k++ {
			switch {
			case isP(t[k], "(") || isP(t[k], "[") || isP(t[k], "{") || isP(t[k], "<"):
				depth++
			case isP(t[k], ")") || isP(t[k], "]") || isP(t[k], "}") || (isP(t[k], ">") && !isP(t[k-1], "=")):
				depth--
			case depth == 0 && (isP(t[k], ";") || t[k].Line != t[k-1].Line && tsEndsStmt(t, k-1)):
				return false
			}
			if depth == 0 && isP(t[k], "=") && !(k+1 < end && isP(t[k+1], ">")) {
				break
			}
		}
		if k >= end {
			return false
		}
	}
	k++ // past =
	if k < end && isKw(t[k], "async") {
		k++
	}
	switch {
	case k >= end:
		return false
	case isKw(t[k], "function"):
		return true
	case t[k].Kind == 'i':
		return k+2 < end && isP(t[k+1], "=") && isP(t[k+2], ">")
	case isP(t[k], "(") || isP(t[k], "<"):
		j := tsSkip(t, k, end)
		if isP(t[k], "<") { // <T>(x: T) => x
			for j = k; j < end && !isP(t[j], "("); j++ {
			}
			j = tsSkip(t, j, end)
		}
		if j < end && isP(t[j], ":") { // (x): T => x
			for j < end && !(isP(t[j], "=") && j+1 < end && isP(t[j+1], ">")) && t[j].Line == t[k].Line {
				j++
			}
		}
		return j+1 < end && isP(t[j], "=") && isP(t[j+1], ">")
	}
	return false
}

// tsPrivate reports whether the member's modifiers between from and to
// include private or protected.
func tsPrivate(t []Tok, from, to int) bool {
	for k := from; k < to && k < len(t); k++ {
		if isKw(t[k], "private") || isKw(t[k], "protected") {
			return true
		}
	}
	return false
}

// stringInit captures `const X = "..."` or a template literal initializer.
func (f *TSFile) stringInit(d *TSDecl, kwIdx int) {
	t := f.Toks
	if d.Kind != "const" && d.Kind != "let" && d.Kind != "var" {
		return
	}
	j := kwIdx + 2
	if j < len(t) && t[j].Kind == 'p' && t[j].Text == ":" {
		return // typed; skip
	}
	if j+1 < len(t) && t[j].Kind == 'p' && t[j].Text == "=" && (t[j+1].Kind == 's' || t[j+1].Kind == 't') {
		// only a bare literal (next token ends the statement or line)
		if j+2 >= len(t) || t[j+2].Line != t[j+1].Line || (t[j+2].Kind == 'p' && t[j+2].Text == ";") || (t[j+2].Kind == 'i' && t[j+2].Line != t[j+1].Line) {
			d.StrVal = t[j+1].Text
			d.HasStr = true
		}
	}
}

func (f *TSFile) parseImport(i int) int {
	t := f.Toks
	j := i + 1
	if j < len(t) && t[j].Kind == 's' { // import 'x'
		f.SideFx = append(f.SideFx, t[j].Text)
		return j
	}
	var binds []Import
	if j < len(t) && t[j].Kind == 'i' && t[j].Text == "type" && j+1 < len(t) && !(t[j+1].Kind == 'i' && t[j+1].Text == "from") {
		j++
	}
	for j < len(t) {
		tk := t[j]
		if tk.Kind == 'i' && tk.Text == "from" {
			if j+1 < len(t) && t[j+1].Kind == 's' {
				for _, b := range binds {
					b.Spec = t[j+1].Text
					f.Imports = append(f.Imports, b)
				}
				return j + 1
			}
			return j
		}
		switch {
		case tk.Kind == 'p' && tk.Text == "*":
			// * as ns
			if j+2 < len(t) && t[j+1].Text == "as" {
				binds = append(binds, Import{Local: t[j+2].Text, Imported: "*"})
				j += 3
				continue
			}
		case tk.Kind == 'p' && tk.Text == "{":
			j++
			for j < len(t) && !(t[j].Kind == 'p' && t[j].Text == "}") {
				if t[j].Kind == 'i' && t[j].Text == "type" && j+1 < len(t) && t[j+1].Kind == 'i' && t[j+1].Text != "as" {
					j++
				}
				if t[j].Kind == 'i' || t[j].Kind == 's' {
					name := t[j].Text
					local := name
					if j+2 < len(t) && t[j+1].Kind == 'i' && t[j+1].Text == "as" {
						local = t[j+2].Text
						j += 2
					}
					binds = append(binds, Import{Local: local, Imported: name})
				}
				j++
			}
		case tk.Kind == 'i':
			binds = append(binds, Import{Local: tk.Text, Imported: "default"})
		case tk.Kind == 'p' && tk.Text == ";":
			return j
		}
		j++
	}
	return j
}

func (f *TSFile) parseExportList(i int) int {
	t := f.Toks
	j := i + 1
	if j < len(t) && t[j].Kind == 'p' && t[j].Text == "*" {
		exp := "*"
		if j+2 < len(t) && t[j+1].Text == "as" {
			exp = t[j+2].Text
			j += 2
		}
		for j < len(t) && !(t[j].Kind == 'i' && t[j].Text == "from") {
			j++
		}
		if j+1 < len(t) && t[j+1].Kind == 's' {
			f.Reexports = append(f.Reexports, Reexport{Exported: exp, Imported: "*", Spec: t[j+1].Text})
			return j + 1
		}
		return j
	}
	type pair struct{ Local, exp string }
	var pairs []pair
	if j < len(t) && t[j].Text == "{" {
		j++
		for j < len(t) && !(t[j].Kind == 'p' && t[j].Text == "}") {
			if t[j].Kind == 'i' && t[j].Text == "type" && j+1 < len(t) && t[j+1].Kind == 'i' && t[j+1].Text != "as" {
				j++
			}
			if t[j].Kind == 'i' {
				local := t[j].Text
				exp := local
				if j+2 < len(t) && t[j+1].Kind == 'i' && t[j+1].Text == "as" {
					exp = t[j+2].Text
					j += 2
				}
				pairs = append(pairs, pair{local, exp})
			}
			j++
		}
		j++
	}
	if j < len(t) && t[j].Kind == 'i' && t[j].Text == "from" && j+1 < len(t) && t[j+1].Kind == 's' {
		for _, p := range pairs {
			f.Reexports = append(f.Reexports, Reexport{Exported: p.exp, Imported: p.Local, Spec: t[j+1].Text})
		}
		return j + 1
	}
	for _, p := range pairs {
		f.LocalExp[p.exp] = p.Local
	}
	return j - 1
}

// parseRequire records `const x = require('m')`, `const x = require('m').y`
// and `const { a, b: c } = require('m')` as imports. It returns the index of
// the statement's last token.
func (f *TSFile) parseRequire(i int) (int, bool) {
	t := f.Toks
	j := i + 1
	var binds []Import
	switch {
	case j < len(t) && t[j].Kind == 'i':
		binds = []Import{{Local: t[j].Text, Imported: "*"}}
		j++
	case j < len(t) && isP(t[j], "{"):
		for j++; j < len(t) && !isP(t[j], "}"); j++ {
			if t[j].Kind != 'i' {
				continue
			}
			b := Import{Local: t[j].Text, Imported: t[j].Text}
			if j+2 < len(t) && isP(t[j+1], ":") && t[j+2].Kind == 'i' {
				b.Local = t[j+2].Text
				j += 2
			} else if j+1 < len(t) && isP(t[j+1], ":") {
				return i, false // nested pattern
			}
			binds = append(binds, b)
		}
		j++
	default:
		return i, false
	}
	if !(j+4 < len(t) && isP(t[j], "=") && isKw(t[j+1], "require") && isP(t[j+2], "(") && t[j+3].Kind == 's' && isP(t[j+4], ")")) {
		return i, false
	}
	spec := t[j+3].Text
	end := j + 4
	if len(binds) == 1 && binds[0].Imported == "*" && end+2 < len(t) && isP(t[end+1], ".") && t[end+2].Kind == 'i' {
		binds[0].Imported = t[end+2].Text
		end += 2
	}
	for _, b := range binds {
		b.Spec, b.Require = spec, true
		f.Imports = append(f.Imports, b)
	}
	if end+1 < len(t) && isP(t[end+1], ";") {
		end++
	}
	return end, true
}

// cjsExport turns `module.exports = X`, `module.exports.name = X` and
// `exports.name = X` into declarations. Exporting a local name or an object
// of local names marks those as exported instead.
func (f *TSFile) cjsExport(i int) *TSDecl {
	t := f.Toks
	j := i
	switch {
	case isKw(t[j], "module") && j+2 < len(t) && isP(t[j+1], ".") && isKw(t[j+2], "exports"):
		j += 3
	case isKw(t[j], "exports"):
		j++
	default:
		return nil
	}
	name := ""
	switch {
	case j+2 < len(t) && isP(t[j], ".") && t[j+1].Kind == 'i' && isP(t[j+2], "="):
		name = t[j+1].Text
		j += 3
	case j < len(t) && isP(t[j], "=") && !(j+1 < len(t) && isP(t[j+1], "=")):
		j++
	default:
		return nil
	}
	if j >= len(t) {
		return nil
	}
	d := &TSDecl{Exported: true, Start: i, Line: t[i].Line, Name: name, Kind: "const"}
	switch {
	case isKw(t[j], "function") || isKw(t[j], "async"):
		d.Kind = "function"
	case isKw(t[j], "class"):
		d.Kind = "class"
	}
	alone := t[j].Kind == 'i' && d.Kind == "const" && (j+1 >= len(t) || t[j+1].Line != t[j].Line || isP(t[j+1], ";"))
	switch {
	case name != "" && alone:
		f.LocalExp[name] = t[j].Text
		d.Name = "<export>"
	case name != "":
	case alone:
		d.Name, d.Kind, d.IsDef, d.AliasOf = "default", "default", true, t[j].Text
	case isP(t[j], "{"):
		// module.exports = { a, b: c, ... }
		depth := 0
		for k := j; k < len(t); k++ {
			switch {
			case isP(t[k], "{"), isP(t[k], "("), isP(t[k], "["):
				depth++
			case isP(t[k], "}"), isP(t[k], ")"), isP(t[k], "]"):
				depth--
			case depth == 1 && t[k].Kind == 'i' && (isP(t[k-1], "{") || isP(t[k-1], ",")) && k+1 < len(t):
				switch {
				case isP(t[k+1], ",") || isP(t[k+1], "}"):
					f.LocalExp[t[k].Text] = t[k].Text
				case isP(t[k+1], ":") && k+3 < len(t) && t[k+2].Kind == 'i' && (isP(t[k+3], ",") || isP(t[k+3], "}")):
					f.LocalExp[t[k].Text] = t[k+2].Text
				}
			}
			if depth == 0 {
				break
			}
		}
		d.Name = "<export>"
	default:
		d.Name, d.IsDef = "default", true
		if d.Kind == "const" {
			d.Kind = "default"
		}
	}
	return d
}
