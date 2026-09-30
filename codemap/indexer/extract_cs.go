package indexer

import (
	"path"
	"sort"
	"strings"

	"github.com/amitbet/pr-manager/codemap/decls"
)

// The C# extractor works from the tree-sitter parse in codemap/decls, like
// the Java one, without a compiler or the project's references. Type names resolve through
// the enclosing namespaces, using directives (global, static and aliases)
// and the file's own types. The parts of a partial type share one set of
// members. Calls and property uses resolve when the receiver's type is
// known: a type name (static member), this/base, an unqualified member of
// the enclosing type, or a variable, field, property or parameter declared
// with a workspace type (var x = new T() too). Extension methods resolve by
// name when their class is visible in the file. Overriding members get
// "impl" edges from the member they override.

func isCSTestLike(p string) bool {
	b := path.Base(p)
	if strings.HasSuffix(b, "Tests.cs") || strings.HasSuffix(b, "Test.cs") {
		return true
	}
	for _, s := range strings.Split(path.Dir(p), "/") {
		if s == "test" || s == "tests" || s == "Test" || strings.HasSuffix(s, "Tests") || strings.HasSuffix(s, ".Test") {
			return true
		}
	}
	return false
}

func isCSBuildOutput(p string) bool {
	for _, s := range strings.Split(path.Dir(p), "/") {
		if s == "bin" || s == "obj" || s == "packages" || s == "node_modules" {
			return true
		}
	}
	return false
}

type csType struct {
	key, fqn string
	d        *decls.CSDecl     // the first part of a partial type
	members  map[string]string // simple name -> node key
	props    map[string]bool
	supers   []*csType
}

func (ct *csType) member(name string, depth int) string {
	if k, ok := ct.members[name]; ok {
		return k
	}
	if depth > 6 {
		return ""
	}
	for _, s := range ct.supers {
		if k := s.member(name, depth+1); k != "" {
			return k
		}
	}
	return ""
}

type csPart struct {
	file string
	d    *decls.CSDecl
	ct   *csType
}

type csExt struct {
	key string
	cls *csType
}

func extractCS(repo, repoRoot string, tracked []string, g *Graph) {
	var paths []string
	for _, p := range tracked {
		if decls.IsCSPath(p) && !isCSTestLike(p) && !isCSBuildOutput(p) {
			paths = append(paths, p)
		}
	}
	files := map[string]*decls.CSFile{}
	var order []string
	for i, f := range parseFiles(repoRoot, "cs", paths, decls.ParseCS) {
		if f != nil {
			files[paths[i]] = f
			order = append(order, paths[i])
		}
	}
	if len(files) == 0 {
		return
	}
	sort.Strings(order)
	key := func(file, sym string) string { return "cs:" + repo + "/" + file + ":" + sym }
	modKey := func(file string) string { return key(file, "<module>") }

	byFQN := map[string]*csType{}
	nsTypes := map[string][]*csType{} // namespace -> top-level types
	fileTypes := map[string]map[string]*csType{}
	ext := map[string][]csExt{} // extension method name -> methods
	var parts []csPart
	for _, p := range order {
		f := files[p]
		dir := path.Dir(p)
		if dir == "." {
			dir = ""
		}
		g.Files = append(g.Files, FileInfo{Path: p, Lang: "cs", Lines: f.Lines, Generated: f.Generated})
		g.Nodes = append(g.Nodes, &Node{Key: modKey(p), Kind: "module", Repo: repo, File: p, Dir: dir, Sym: "<module>", Start: 1, End: f.Lines, Internal: true})
		ft := map[string]*csType{}
		fileTypes[p] = ft
		seen := map[string]bool{}
		for _, d := range f.Decls {
			if seen[d.Name] {
				continue // overloads share a node
			}
			seen[d.Name] = true
			n := &Node{Key: key(p, d.Name), Kind: "cs-" + d.Kind, Repo: repo, File: p, Dir: dir, Sym: d.Name,
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
				if d.Namespace != "" {
					fqn = d.Namespace + "." + d.Name
				}
				ct := byFQN[fqn]
				if ct == nil {
					ct = &csType{key: n.Key, fqn: fqn, d: d, members: map[string]string{}, props: map[string]bool{}}
					byFQN[fqn] = ct
					if d.Owner == "" {
						nsTypes[d.Namespace] = append(nsTypes[d.Namespace], ct)
					}
				}
				ft[d.Name] = ct
				parts = append(parts, csPart{p, d, ct})
			} else if ct := ft[d.Owner]; ct != nil {
				m := lastSeg(d.Name)
				if _, ok := ct.members[m]; !ok {
					ct.members[m] = n.Key
				}
				if d.Kind == "property" {
					ct.props[m] = true
				}
				if d.Ext {
					ext[m] = append(ext[m], csExt{n.Key, ct})
				}
			}
		}
	}

	// Type names visible in each file, later wins: using namespaces
	// (global ones from every file), the global namespace, the enclosing
	// namespaces from the outermost in, aliases, then the file's own types.
	var global []decls.CSUsing
	for _, p := range order {
		for _, u := range files[p].Usings {
			if u.Global {
				global = append(global, u)
			}
		}
	}
	binds := map[string]map[string]*csType{}
	statics := map[string]map[string]string{}
	for _, p := range order {
		f := files[p]
		b := map[string]*csType{}
		st := map[string]string{}
		usings := append(append([]decls.CSUsing(nil), global...), f.Usings...)
		for _, u := range usings {
			switch {
			case u.Alias != "":
			case u.Static:
				if ct := byFQN[u.Path]; ct != nil {
					for m, k := range ct.members {
						st[m] = k
					}
				}
			default:
				for _, ct := range nsTypes[u.Path] {
					b[ct.d.Name] = ct
				}
			}
		}
		for _, ct := range nsTypes[""] {
			b[ct.d.Name] = ct
		}
		for _, ns := range f.Namespaces {
			segs := strings.Split(ns, ".")
			for i := range segs {
				for _, ct := range nsTypes[strings.Join(segs[:i+1], ".")] {
					b[ct.d.Name] = ct
				}
			}
		}
		for _, u := range usings {
			if u.Alias != "" {
				if ct := byFQN[u.Path]; ct != nil {
					b[u.Alias] = ct
				}
			}
		}
		for _, name := range bindOrder(fileTypes[p]) {
			b[lastSeg(name)] = fileTypes[p][name]
		}
		binds[p], statics[p] = b, st
	}
	resolveType := func(p, name string) *csType {
		if ct := byFQN[name]; ct != nil {
			return ct
		}
		head, rest, _ := strings.Cut(name, ".")
		ct := binds[p][head]
		if ct != nil && rest != "" {
			ct = byFQN[ct.fqn+"."+rest]
		}
		return ct
	}
	for _, pt := range parts {
		for _, s := range pt.d.Supers {
			if st := resolveType(pt.file, s); st != nil && st != pt.ct && !containsType(pt.ct.supers, st) {
				pt.ct.supers = append(pt.ct.supers, st)
			}
		}
	}

	edges := edgeAcc{}
	for _, p := range order {
		f := files[p]
		t := f.Toks
		b, st, ft := binds[p], statics[p], fileTypes[p]
		isP := func(i int, s string) bool { return i >= 0 && i < len(t) && t[i].Kind == 'p' && t[i].Text == s }
		// The innermost declaration owns each token; types come before
		// their members, so later ranges overwrite.
		owner := make([]*decls.CSDecl, len(t))
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
		enclosing := func(i int) []*csType {
			d := owner[i]
			if d == nil {
				return nil
			}
			name := d.Name
			if !d.IsType() {
				name = d.Owner
			}
			var out []*csType
			for name != "" {
				if ct := ft[name]; ct != nil {
					out = append(out, ct)
				}
				i := strings.LastIndexByte(name, '.')
				if i < 0 {
					break
				}
				name = name[:i]
			}
			return out
		}
		// The scope a declaration at t[i] belongs to; a property's own name
		// belongs to its type.
		scopeOf := func(i int, name string) string {
			d := owner[i]
			if d == nil {
				return ""
			}
			if d.Kind == "property" && lastSeg(d.Name) == name {
				return d.Owner
			}
			return d.Name
		}
		// Variables, fields, properties and parameters declared with a
		// workspace type: T x = / T x; / T x, / T x) / T x { / T? x /
		// T<U> x / is T x && / var x = new T.
		vars := map[string]map[string]*csType{}
		declare := func(i int, name string, ct *csType) {
			sc := scopeOf(i, name)
			if vars[sc] == nil {
				vars[sc] = map[string]*csType{}
			}
			vars[sc][name] = ct
		}
		for i, tk := range t {
			if tk.Kind != 'i' || isP(i-1, ".") {
				continue
			}
			if tk.Text == "var" && i+4 < len(t) && t[i+1].Kind == 'i' && isP(i+2, "=") && t[i+3].Kind == 'i' && t[i+3].Text == "new" {
				if ct := b[t[i+4].Text]; ct != nil {
					declare(i, t[i+1].Text, ct)
				}
				continue
			}
			ct := b[tk.Text]
			if ct == nil {
				continue
			}
			j := i + 1
			if isP(j, "<") {
				d := 0
				for ; j < len(t); j++ {
					if isP(j, "<") {
						d++
					} else if isP(j, ">") {
						d--
						if d == 0 {
							j++
							break
						}
					} else if t[j].Kind == 'p' && !strings.Contains(",?.[]()", t[j].Text) {
						break
					}
				}
			}
			if isP(j, "?") {
				j++
			}
			if j+1 < len(t) && t[j].Kind == 'i' && (t[j+1].Kind == 'p' && strings.Contains("=;,):{&|", t[j+1].Text) || t[j+1].Text == "in") {
				declare(i, t[j].Text, ct)
			}
		}
		varType := func(i int, name string) *csType {
			if d := owner[i]; d != nil && !d.IsType() {
				if ct := vars[d.Name][name]; ct != nil {
					return ct
				}
			}
			for _, et := range enclosing(i) {
				if ct := vars[et.d.Name][name]; ct != nil {
					return ct
				}
			}
			return vars[""][name]
		}
		member := func(i int) (string, bool) { // .name or ?.name after t[i]
			if isP(i+1, "?") {
				i++
			}
			if isP(i+1, ".") && i+2 < len(t) && t[i+2].Kind == 'i' {
				return t[i+2].Text, true
			}
			return "", false
		}
		after := func(i int) int { // index of the name member(i) returned
			if isP(i+1, "?") {
				return i + 3
			}
			return i + 2
		}
		for i, tk := range t {
			if tk.Kind != 'i' {
				continue
			}
			from := ownerKey(i)
			call := isP(i+1, "(")
			if isP(i-1, ".") {
				// x.Ext(): an extension method whose class the file sees.
				if call {
					for _, e := range ext[tk.Text] {
						if b[lastSeg(e.cls.d.Name)] == e.cls {
							edges.add(from, e.key, "")
						}
					}
				}
				continue
			}
			switch {
			case tk.Text == "this" || tk.Text == "base":
				m, ok := member(i)
				if !ok {
					break
				}
				for _, et := range enclosing(i) {
					if tk.Text == "base" {
						for _, s := range et.supers {
							if k := s.member(m, 0); k != "" {
								edges.add(from, k, "")
							}
						}
						break
					}
					if k := et.member(m, 0); k != "" {
						edges.add(from, k, "")
					}
					if vt := vars[et.d.Name][m]; vt != nil {
						if m2, ok := member(after(i)); ok {
							if k := vt.member(m2, 0); k != "" {
								edges.add(from, k, "")
							}
						}
					}
					break
				}
			case b[tk.Text] != nil && varType(i, tk.Text) == nil:
				ct := b[tk.Text]
				// Outer.Inner
				for {
					m, ok := member(i)
					if !ok {
						break
					}
					nt := byFQN[ct.fqn+"."+m]
					if nt == nil {
						break
					}
					ct, i = nt, after(i)
				}
				edges.add(from, ct.key, "")
				if i > 0 && t[i-1].Kind == 'i' && t[i-1].Text == "new" {
					if k := ct.members[lastSeg(ct.d.Name)]; k != "" {
						edges.add(from, k, "")
					}
				}
				if m, ok := member(i); ok {
					if k := ct.member(m, 0); k != "" {
						edges.add(from, k, "")
					}
				}
			default:
				if vt := varType(i, tk.Text); vt != nil {
					if m, ok := member(i); ok {
						if k := vt.member(m, 0); k != "" {
							edges.add(from, k, "")
						}
					}
					break
				}
				// An unqualified call or property of the enclosing type,
				// then a using static member.
				found := false
				for _, et := range enclosing(i) {
					k := et.member(tk.Text, 0)
					if k != "" && (call || csIsProp(et, tk.Text, 0)) {
						edges.add(from, k, "")
						found = true
						break
					}
				}
				if !found && st[tk.Text] != "" {
					edges.add(from, st[tk.Text], "")
				}
			}
		}
	}
	// Overrides: callers of the overridden member can reach the override.
	done := map[*csType]bool{}
	for _, pt := range parts {
		ct := pt.ct
		if done[ct] {
			continue
		}
		done[ct] = true
		for m, k := range ct.members {
			for _, s := range ct.supers {
				if sk := s.member(m, 0); sk != "" && sk != k {
					edges.add(sk, k, "impl")
				}
			}
		}
	}
	g.Edges = append(g.Edges, edges.list()...)
}

// csIsProp reports whether name is a property of ct or its base types.
func csIsProp(ct *csType, name string, depth int) bool {
	if _, ok := ct.members[name]; ok {
		return ct.props[name]
	}
	if depth > 6 {
		return false
	}
	for _, s := range ct.supers {
		if csIsProp(s, name, depth+1) {
			return true
		}
	}
	return false
}

func containsType(s []*csType, ct *csType) bool {
	for _, x := range s {
		if x == ct {
			return true
		}
	}
	return false
}
