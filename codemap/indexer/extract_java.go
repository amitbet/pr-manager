package indexer

import (
	"path"
	"sort"
	"strings"

	"github.com/amitbet/pr-manager/codemap/decls"
)

// The Java extractor works from the tree-sitter parse in codemap/decls
// (declarations, imports and a token stream), without a compiler or the
// build's classpath. Names resolve through the package,
// imports (single, wildcard and static) and the file's own types. Calls
// resolve when the receiver's type is known: a type name (static call),
// this, an unqualified call in the enclosing class, or a variable, field or
// parameter declared with a workspace type. Overriding methods get "impl"
// edges from the method they override, as Go interface methods do.

func isJavaTestLike(p string) bool {
	b := path.Base(p)
	return strings.Contains(p, "src/test/") || strings.Contains(p, "src/it/") || strings.Contains(p, "/testFixtures/") ||
		strings.HasSuffix(b, "Test.java") || strings.HasSuffix(b, "Tests.java") || strings.HasSuffix(b, "IT.java")
}

type javaType struct {
	key, fqn, file string
	d              *decls.JavaDecl
	methods        map[string]string // simple name -> node key
	supers         []*javaType
}

func (jt *javaType) method(name string, depth int) string {
	if k, ok := jt.methods[name]; ok {
		return k
	}
	if depth > 6 {
		return ""
	}
	for _, s := range jt.supers {
		if k := s.method(name, depth+1); k != "" {
			return k
		}
	}
	return ""
}

func extractJava(repo, repoRoot string, tracked []string, g *Graph) {
	var paths []string
	for _, p := range tracked {
		if decls.IsJavaPath(p) && !isJavaTestLike(p) {
			paths = append(paths, p)
		}
	}
	files := map[string]*decls.JavaFile{}
	var order []string
	for i, f := range parseFiles(repoRoot, "java", paths, decls.ParseJava) {
		if f != nil {
			files[paths[i]] = f
			order = append(order, paths[i])
		}
	}
	if len(files) == 0 {
		return
	}
	sort.Strings(order)
	key := func(file, sym string) string { return "java:" + repo + "/" + file + ":" + sym }
	modKey := func(file string) string { return key(file, "<module>") }

	byFQN := map[string]*javaType{}
	pkgTypes := map[string][]*javaType{} // package -> top-level types
	nested := map[string][]*javaType{}   // outer fqn -> direct nested types
	fileTypes := map[string]map[string]*javaType{}
	for _, p := range order {
		f := files[p]
		dir := path.Dir(p)
		if dir == "." {
			dir = ""
		}
		g.Files = append(g.Files, FileInfo{Path: p, Lang: "java", Lines: f.Lines, Generated: f.Generated})
		g.Nodes = append(g.Nodes, &Node{Key: modKey(p), Kind: "module", Repo: repo, File: p, Dir: dir, Sym: "<module>", Start: 1, End: f.Lines, Internal: true})
		ft := map[string]*javaType{}
		fileTypes[p] = ft
		seen := map[string]bool{}
		for _, d := range f.Decls {
			if seen[d.Name] {
				continue // overloads share a node
			}
			seen[d.Name] = true
			n := &Node{Key: key(p, d.Name), Kind: "java-" + d.Kind, Repo: repo, File: p, Dir: dir, Sym: d.Name,
				Start: d.Line, End: max(d.EndLine, d.Line), Exported: d.Exported}
			if !d.IsType() {
				n.Cyclo, n.Nest = d.Cyclo, d.Nest
			}
			if f.Generated {
				n.Tags = append(n.Tags, "generated")
			}
			g.Nodes = append(g.Nodes, n)
			if d.IsType() {
				fqn := d.Name
				if f.Package != "" {
					fqn = f.Package + "." + d.Name
				}
				jt := &javaType{key: n.Key, fqn: fqn, file: p, d: d, methods: map[string]string{}}
				ft[d.Name] = jt
				byFQN[fqn] = jt
				if d.Owner == "" {
					pkgTypes[f.Package] = append(pkgTypes[f.Package], jt)
				} else {
					outer := strings.TrimSuffix(fqn, "."+lastSeg(d.Name))
					nested[outer] = append(nested[outer], jt)
				}
			} else if jt := ft[d.Owner]; jt != nil {
				jt.methods[lastSeg(d.Name)] = n.Key
			}
		}
	}

	// Type names visible in each file: wildcard imports, then the package,
	// then single-type imports, then the file's own types (later wins).
	binds := map[string]map[string]*javaType{}
	statics := map[string]map[string]string{}
	for _, p := range order {
		f := files[p]
		b := map[string]*javaType{}
		for _, im := range f.Imports {
			if im.Static || !im.Wildcard {
				continue
			}
			for _, jt := range append(append([]*javaType(nil), pkgTypes[im.Path]...), nested[im.Path]...) {
				b[lastSeg(jt.d.Name)] = jt
			}
		}
		for _, jt := range pkgTypes[f.Package] {
			b[jt.d.Name] = jt
		}
		st := map[string]string{}
		for _, im := range f.Imports {
			switch {
			case !im.Static:
				if !im.Wildcard {
					if jt := byFQN[im.Path]; jt != nil {
						b[lastSeg(im.Path)] = jt
					}
				}
			case im.Wildcard:
				if jt := byFQN[im.Path]; jt != nil {
					for m, k := range jt.methods {
						st[m] = k
					}
				}
			default:
				owner, member := im.Path, lastSeg(im.Path)
				owner = strings.TrimSuffix(owner, "."+member)
				if jt := byFQN[im.Path]; jt != nil {
					b[member] = jt
				} else if jt := byFQN[owner]; jt != nil {
					if k := jt.methods[member]; k != "" {
						st[member] = k
					} else {
						st[member] = jt.key // a constant
					}
				}
			}
		}
		for _, name := range bindOrder(fileTypes[p]) {
			b[lastSeg(name)] = fileTypes[p][name]
		}
		binds[p], statics[p] = b, st
	}
	resolveType := func(p, name string) *javaType {
		if jt := byFQN[name]; jt != nil {
			return jt
		}
		head, rest, _ := strings.Cut(name, ".")
		jt := binds[p][head]
		if jt != nil && rest != "" {
			jt = byFQN[jt.fqn+"."+rest]
		}
		return jt
	}
	for _, p := range order {
		for _, name := range bindOrder(fileTypes[p]) {
			jt := fileTypes[p][name]
			for _, s := range jt.d.Supers {
				if st := resolveType(p, s); st != nil && st != jt {
					jt.supers = append(jt.supers, st)
				}
			}
		}
	}

	edges := edgeAcc{}
	for _, p := range order {
		f := files[p]
		t := f.Toks
		b, st, ft := binds[p], statics[p], fileTypes[p]
		// The innermost declaration owns each token; types come before
		// their members, so later ranges overwrite.
		owner := make([]*decls.JavaDecl, len(t))
		for _, d := range f.Decls {
			for i := d.Start; i < d.End && i < len(t); i++ {
				owner[i] = d
			}
		}
		ownerKey := func(i int) string {
			if owner[i] == nil {
				return modKey(p)
			}
			return key(p, owner[i].Name)
		}
		// Enclosing types, innermost first.
		enclosing := func(i int) []*javaType {
			d := owner[i]
			if d == nil {
				return nil
			}
			name := d.Name
			if !d.IsType() {
				name = d.Owner
			}
			var out []*javaType
			for name != "" {
				if jt := ft[name]; jt != nil {
					out = append(out, jt)
				}
				i := strings.LastIndexByte(name, '.')
				if i < 0 {
					break
				}
				name = name[:i]
			}
			return out
		}
		scopeOf := func(i int) string {
			if owner[i] == nil {
				return ""
			}
			return owner[i].Name
		}
		// Variables, fields and parameters declared with a workspace type:
		// Foo x = / Foo x; / Foo x, / Foo x) / Foo<Bar> x.
		vars := map[string]map[string]*javaType{}
		for i, tk := range t {
			if tk.Kind != 'i' || (i > 0 && t[i-1].Kind == 'p' && t[i-1].Text == ".") {
				continue
			}
			jt := b[tk.Text]
			if jt == nil {
				continue
			}
			j := i + 1
			if j < len(t) && t[j].Kind == 'p' && t[j].Text == "<" {
				d := 0
				for ; j < len(t); j++ {
					if t[j].Kind == 'p' && t[j].Text == "<" {
						d++
					} else if t[j].Kind == 'p' && t[j].Text == ">" {
						d--
						if d == 0 {
							j++
							break
						}
					} else if t[j].Kind == 'p' && t[j].Text != "," && t[j].Text != "?" && t[j].Text != "." && t[j].Text != "[" && t[j].Text != "]" {
						break
					}
				}
			}
			if j+1 < len(t) && t[j].Kind == 'i' && t[j+1].Kind == 'p' && strings.Contains("=;,):", t[j+1].Text) {
				sc := scopeOf(i)
				if vars[sc] == nil {
					vars[sc] = map[string]*javaType{}
				}
				vars[sc][t[j].Text] = jt
			}
		}
		varType := func(i int, name string) *javaType {
			if d := owner[i]; d != nil && !d.IsType() {
				if jt := vars[d.Name][name]; jt != nil {
					return jt
				}
			}
			for _, et := range enclosing(i) {
				if jt := vars[et.d.Name][name]; jt != nil {
					return jt
				}
			}
			return vars[""][name]
		}
		member := func(i int) (string, bool) { // .name after t[i]
			if i+2 < len(t) && t[i+1].Kind == 'p' && t[i+1].Text == "." && t[i+2].Kind == 'i' {
				return t[i+2].Text, true
			}
			return "", false
		}
		for i, tk := range t {
			if tk.Kind != 'i' || (i > 0 && t[i-1].Kind == 'p' && t[i-1].Text == ".") {
				continue
			}
			from := ownerKey(i)
			call := i+1 < len(t) && t[i+1].Kind == 'p' && t[i+1].Text == "("
			switch {
			case tk.Text == "this" || tk.Text == "super":
				if m, ok := member(i); ok {
					for _, et := range enclosing(i) {
						if tk.Text == "super" {
							for _, s := range et.supers {
								if k := s.method(m, 0); k != "" {
									edges.add(from, k, "")
								}
							}
							break
						}
						if k := et.method(m, 0); k != "" {
							edges.add(from, k, "")
							break
						}
						if vt := vars[et.d.Name][m]; vt != nil {
							if m2, ok := member(i + 2); ok {
								if k := vt.method(m2, 0); k != "" {
									edges.add(from, k, "")
								}
							}
							break
						}
					}
				}
			case b[tk.Text] != nil:
				jt := b[tk.Text]
				// Outer.Inner
				for {
					m, ok := member(i)
					if !ok {
						break
					}
					nt := byFQN[jt.fqn+"."+m]
					if nt == nil {
						break
					}
					jt, i = nt, i+2
				}
				edges.add(from, jt.key, "")
				if i > 0 && t[i-1].Kind == 'i' && t[i-1].Text == "new" {
					if k := jt.methods[lastSeg(jt.d.Name)]; k != "" {
						edges.add(from, k, "")
					}
				}
				if m, ok := member(i); ok {
					if k := jt.method(m, 0); k != "" {
						edges.add(from, k, "")
					}
				}
			case call:
				found := false
				for _, et := range enclosing(i) {
					if k := et.method(tk.Text, 0); k != "" {
						edges.add(from, k, "")
						found = true
						break
					}
				}
				if !found && st[tk.Text] != "" {
					edges.add(from, st[tk.Text], "")
				}
			default:
				if vt := varType(i, tk.Text); vt != nil {
					if m, ok := member(i); ok {
						if k := vt.method(m, 0); k != "" {
							edges.add(from, k, "")
						}
					}
				} else if k := st[tk.Text]; k != "" {
					edges.add(from, k, "")
				}
			}
		}
	}
	// Overrides: callers of the overridden method can reach the override.
	for _, p := range order {
		for _, jt := range fileTypes[p] {
			for m, k := range jt.methods {
				for _, s := range jt.supers {
					if sk := s.method(m, 0); sk != "" && sk != k {
						edges.add(sk, k, "impl")
					}
				}
			}
		}
	}
	g.Edges = append(g.Edges, edges.list()...)
}

// bindOrder is m's keys (dotted type names) in the order to bind them by
// last segment, so that when two share one the top-level type (fewest
// segments), then the alphabetically first, is bound last and wins.
func bindOrder[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		di, dj := strings.Count(keys[i], "."), strings.Count(keys[j], ".")
		if di != dj {
			return di > dj
		}
		return keys[i] > keys[j]
	})
	return keys
}

func lastSeg(s string) string {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return s[i+1:]
	}
	return s
}
