package decls

import (
	"path"
	"sort"
)

// Decl is a named line range: a TS top-level declaration, a Java or C# type
// or member, a Python function, class or method, a Rust item, a type or
// function of a generic-parser language (shell, PowerShell, C, C++, PHP,
// Scala, Kotlin, Ruby, Swift, Dart), or an OpenAPI operation.
// Names match the symbols in the code map.
type Decl struct {
	Name       string
	Start, End int
}

// IsTSPath reports whether the indexer parses p as TypeScript/JavaScript.
func IsTSPath(p string) bool {
	switch path.Ext(p) {
	case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
		return true
	}
	return false
}

// IsJavaPath, IsPyPath, IsCSPath and IsRustPath report whether the indexer
// parses p as Java, Python, C# or Rust.
func IsJavaPath(p string) bool { return path.Ext(p) == ".java" }
func IsPyPath(p string) bool   { return path.Ext(p) == ".py" }
func IsCSPath(p string) bool   { return path.Ext(p) == ".cs" }
func IsRustPath(p string) bool { return path.Ext(p) == ".rs" }

// HasDecls reports whether Names can name the code in p (Go is parsed by
// go/parser, not here).
func HasDecls(p string) bool {
	return IsTSPath(p) || IsJavaPath(p) || IsPyPath(p) || IsCSPath(p) || IsRustPath(p) || LangFor(p) != nil
}

// Names lists the declarations of a TS/JS, Java, Python, C#, Rust or
// generic-parser file, nil for anything else. Ranges nest (a class holds
// its methods, an impl its functions).
func Names(p string, src []byte) []Decl {
	switch {
	case IsTSPath(p):
		return TSDecls(p, string(src))
	case IsJavaPath(p):
		return JavaDecls(string(src))
	case IsPyPath(p):
		return PyDecls(string(src))
	case IsCSPath(p):
		return CSDecls(string(src))
	case IsRustPath(p):
		return RustDecls(string(src))
	case LangFor(p) != nil:
		return GenDecls(p, string(src))
	}
	return nil
}

// Innermost returns the smallest declaration holding lines lo..hi, or nil.
func Innermost(ds []Decl, lo, hi int) *Decl {
	var best *Decl
	for i := range ds {
		d := &ds[i]
		if d.Start <= lo && hi <= d.End && (best == nil || d.End-d.Start < best.End-best.Start) {
			best = d
		}
	}
	return best
}

// TSDecls lists the named top-level declarations and class members the
// indexer records, in file order; a class holds its members. Top-level
// overloads and merged declarations keep the first range. p picks JSX
// lexing (see IsJSXPath).
func TSDecls(p, src string) []Decl {
	f := ParseTS(p, src)
	seen := map[string]bool{}
	var out []Decl
	for _, d := range f.Decls {
		if d.Name == "<export>" || d.Name == "<destructure>" || seen[d.Name] {
			continue
		}
		seen[d.Name] = true
		out = append(out, Decl{Name: d.Name, Start: d.Line, End: max(d.EndLine, d.Line)})
	}
	return out
}

// SpecDecls lists an OpenAPI document's operations by operationId. It
// returns nil for YAML that is not an OpenAPI document.
func SpecDecls(src []byte) []Decl {
	sp, err := ParseSpec(src, "")
	if err != nil || sp == nil {
		return nil
	}
	out := make([]Decl, 0, len(sp.Ops))
	for _, op := range sp.Ops {
		out = append(out, Decl{Name: op.ID, Start: op.Start, End: op.End})
	}
	return out
}

// Overlapping returns the declarations intersecting [start, end].
func Overlapping(ds []Decl, start, end int) []Decl {
	var out []Decl
	for _, d := range ds {
		if d.Start <= end && d.End >= start {
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}
