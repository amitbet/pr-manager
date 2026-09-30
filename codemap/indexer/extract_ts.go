package indexer

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"

	"strings"

	"github.com/amitbet/pr-manager/codemap/cx"
	"github.com/amitbet/pr-manager/codemap/decls"
)

// The TS extractor is a lexer, not a type checker. It finds top-level
// declarations and class members, import bindings, identifier uses inside
// each declaration, and string/template literals. That is enough for
// module- and declaration-level linkage without a node toolchain. A member
// is reached through its class (the class depends on its members), through
// this.m inside the class, and through C.m where C names the class.

type tsResolver struct {
	repoRoot string
	baseURL  string // repo-relative
	paths    [][2]string
	files    map[string]*decls.TSFile
}

func loadTSConfig(repoRoot string) (baseURL string, paths [][2]string) {
	b, err := os.ReadFile(filepath.Join(repoRoot, "tsconfig.json"))
	if err != nil {
		return "", nil
	}
	// tsconfig allows comments and trailing commas.
	s := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(string(b), "")
	s = regexp.MustCompile(`,(\s*[}\]])`).ReplaceAllString(s, "$1")
	var cfg struct {
		CompilerOptions struct {
			BaseURL string              `json:"baseUrl"`
			Paths   map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
	}
	if json.Unmarshal([]byte(s), &cfg) != nil {
		return "", nil
	}
	baseURL = strings.TrimSuffix(path.Clean(cfg.CompilerOptions.BaseURL), "/")
	if baseURL == "." {
		baseURL = ""
	}
	for k, v := range cfg.CompilerOptions.Paths {
		if len(v) > 0 {
			paths = append(paths, [2]string{k, v[0]})
		}
	}
	// longest alias first
	sort.Slice(paths, func(i, j int) bool { return len(paths[i][0]) > len(paths[j][0]) })
	return baseURL, paths
}

var tsExts = []string{"", ".ts", ".tsx", ".d.ts", ".js", ".jsx", ".mjs", ".cjs", "/index.ts", "/index.tsx", "/index.js", "/index.jsx"}

// tsJSExtSubst maps an emitted JS extension to the TS sources it may be
// compiled from, in TypeScript's lookup order. Node ESM projects
// (moduleResolution node16/nodenext/bundler) import siblings by their
// output name: "./foo.js" names foo.ts.
var tsJSExtSubst = map[string][]string{
	".js":  {".ts", ".tsx", ".d.ts"},
	".jsx": {".tsx", ".d.ts"},
	".mjs": {".mts", ".d.mts"},
	".cjs": {".cts", ".d.cts"},
}

func (r *tsResolver) tryFile(p string) string {
	p = path.Clean(p)
	if _, ok := r.files[p]; ok {
		return p // a real .js file wins over its TS namesake
	}
	if ext := path.Ext(p); tsJSExtSubst[ext] != nil {
		stem := strings.TrimSuffix(p, ext)
		for _, e := range tsJSExtSubst[ext] {
			if _, ok := r.files[stem+e]; ok {
				return stem + e
			}
		}
	}
	for _, e := range tsExts {
		if _, ok := r.files[p+e]; ok {
			return p + e
		}
	}
	return ""
}

func (r *tsResolver) resolve(fromFile, spec string) string {
	if i := strings.IndexAny(spec, "?#"); i >= 0 {
		spec = spec[:i]
	}
	if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") {
		return r.tryFile(path.Join(path.Dir(fromFile), spec))
	}
	for _, pa := range r.paths {
		alias, target := pa[0], pa[1]
		if strings.HasSuffix(alias, "*") {
			pre := strings.TrimSuffix(alias, "*")
			if strings.HasPrefix(spec, pre) {
				rest := spec[len(pre):]
				tgt := strings.Replace(target, "*", rest, 1)
				if out := r.tryFile(path.Join(r.baseURL, tgt)); out != "" {
					return out
				}
			}
		} else if spec == alias {
			if out := r.tryFile(path.Join(r.baseURL, target)); out != "" {
				return out
			}
		}
	}
	if out := r.tryFile(path.Join(r.baseURL, spec)); out != "" {
		return out
	}
	if out := r.tryFile(path.Join("src", spec)); out != "" {
		return out
	}
	return ""
}

func isTSTestLike(p string) bool {
	b := path.Base(p)
	return strings.Contains(b, ".test.") || strings.Contains(b, ".Spec.") || strings.Contains(b, ".stories.") ||
		strings.Contains(p, "__tests__/") || strings.Contains(p, "__mocks__/") || strings.HasPrefix(p, "e2e/") ||
		strings.Contains(p, "/e2e/") || strings.HasPrefix(p, ".storybook/") || strings.Contains(p, "/test-utils/") ||
		strings.Contains(p, "/mocks/") || strings.HasPrefix(b, "setupTests")
}

// isJSTestLike adds the JS conventions that only make sense for source
// files: *.spec.* and test/ directories (helm keeps templates in tests/).
func isJSTestLike(p string) bool {
	return isTSTestLike(p) || strings.Contains(path.Base(p), ".spec.") ||
		strings.HasPrefix(p, "test/") || strings.HasPrefix(p, "tests/") || strings.Contains(p, "/test/") || strings.Contains(p, "/tests/")
}

// isJSToolConfig reports whether a root-level JS file configures a tool
// (eslint, webpack, babel, jest...) rather than holding application code.
func isJSToolConfig(p string) bool {
	b := strings.ToLower(path.Base(p))
	if strings.HasPrefix(b, ".") || strings.Contains(b, "config") || strings.Contains(b, "rc.") {
		return true
	}
	for _, pre := range []string{"webpack", "gulpfile", "gruntfile", "rollup", "karma", "jest", "babel", "vite", "prettier", "eslint"} {
		if strings.HasPrefix(b, pre) {
			return true
		}
	}
	return false
}

// svcURLRe finds service calls in URL literals: "/<service>/<path>", where
// <service> is the first absolute path segment (after an optional host) and
// names a repo in the workspace. Names that match no repo are dropped at rank
// time.
var svcURLRe = regexp.MustCompile(`(?:^|[^/\w.-]|//[^/\s'"]+)/([a-z0-9][a-z0-9._-]*)(/[^\s'"?#]*)?`)

func extractTS(repo, repoRoot string, tracked []string, g *Graph) {
	r := &tsResolver{repoRoot: repoRoot, files: map[string]*decls.TSFile{}}
	r.baseURL, r.paths = loadTSConfig(repoRoot)
	var paths []string
	for _, p := range tracked {
		if !decls.IsTSPath(p) || isJSTestLike(p) || strings.HasPrefix(p, "public/") || strings.Contains(p, "node_modules/") ||
			strings.HasSuffix(p, ".min.js") || strings.HasPrefix(p, "dist/") || strings.Contains(p, "/dist/") || strings.Contains(p, "vendor/") {
			continue
		}
		if ext := path.Ext(p); strings.Count(p, "/") == 0 && ext != ".ts" && ext != ".tsx" && isJSToolConfig(p) {
			continue // root config files (eslint, webpack)
		}
		paths = append(paths, p)
	}
	// No parse cache: the TS lexer is faster than decoding one.
	var order []string
	for i, f := range parseFiles(repoRoot, "", paths, decls.ParseTS) {
		if f != nil {
			r.files[paths[i]] = f
			order = append(order, paths[i])
		}
	}
	sort.Strings(order)
	key := func(file, sym string) string { return "ts:" + repo + "/" + file + ":" + sym }
	modKey := func(file string) string { return key(file, "<module>") }

	edges := edgeAcc{}
	// exportIndex: file -> exported name -> node key
	exportIndex := map[string]map[string]string{}
	constVals := map[string]string{} // node key -> string value
	members := map[string]bool{}     // node keys of class members
	for _, p := range order {
		f := r.files[p]
		dir := path.Dir(p)
		if dir == "." {
			dir = ""
		}
		g.Files = append(g.Files, FileInfo{Path: p, Lang: "ts", Lines: f.Lines, Generated: f.Generated})
		mk := &Node{Key: modKey(p), Kind: "module", Repo: repo, File: p, Dir: dir, Sym: "<module>", Start: 1, End: f.Lines, Internal: true}
		g.Nodes = append(g.Nodes, mk)
		ex := map[string]string{}
		exportIndex[p] = ex
		seen := map[string]bool{}
		for _, d := range f.Decls {
			if d.Name == "<export>" || d.Name == "<destructure>" {
				continue
			}
			sym := d.Name
			if seen[sym] {
				continue // overloads / declaration merging
			}
			seen[sym] = true
			n := &Node{Key: key(p, sym), Kind: "ts-" + d.Kind, Repo: repo, File: p, Dir: dir, Sym: sym, Start: d.Line, End: max(d.EndLine, d.Line), Exported: d.Exported}
			if d.Kind == "function" || d.Kind == "const" || d.Kind == "let" || d.Kind == "var" || d.Kind == "method" {
				end := min(d.End, len(f.Toks))
				st := cx.TS(f.Toks[min(max(0, d.Start), end):end])
				n.Cyclo, n.Nest = st.Cyclo, st.Nest
			}
			if f.Generated {
				n.Tags = append(n.Tags, "generated")
			}
			g.Nodes = append(g.Nodes, n)
			if d.Owner != "" {
				members[n.Key] = true
				edges.add(key(p, d.Owner), n.Key, "")
				continue // members are not exported by name
			}
			if d.Exported {
				if d.IsDef {
					ex["default"] = n.Key
				}
				ex[sym] = n.Key
			}
			if d.HasStr {
				constVals[n.Key] = d.StrVal
			}
		}
		for exp, local := range f.LocalExp {
			if k, ok := ex[local]; ok {
				ex[exp] = k
			} else if seen[local] {
				ex[exp] = key(p, local)
			}
		}
		// Default export of an identifier: `export default Foo`.
		for _, d := range f.Decls {
			if d.IsDef && d.AliasOf != "" && seen[d.AliasOf] {
				ex["default"] = key(p, d.AliasOf)
			}
		}
	}

	var resolveExport func(file, name string, depth int) string
	resolveExport = func(file, name string, depth int) string {
		if depth > 8 {
			return ""
		}
		if k, ok := exportIndex[file][name]; ok {
			return k
		}
		f := r.files[file]
		if f == nil {
			return ""
		}
		for _, re := range f.Reexports {
			tgt := r.resolve(file, re.Spec)
			if tgt == "" {
				continue
			}
			if re.Exported == name && re.Imported != "*" {
				if k := resolveExport(tgt, re.Imported, depth+1); k != "" {
					return k
				}
				return modKey(tgt)
			}
			if re.Exported == name && re.Imported == "*" { // export * as ns
				return modKey(tgt)
			}
			if re.Exported == "*" && name != "default" {
				if k := resolveExport(tgt, name, depth+1); k != "" {
					return k
				}
			}
		}
		return ""
	}

	// Barrel files: each re-export depends on its origin, so barrels get rank
	// only as a pass-through.
	for _, p := range order {
		f := r.files[p]
		for _, re := range f.Reexports {
			tgt := r.resolve(p, re.Spec)
			if tgt == "" {
				continue
			}
			edges.add(modKey(p), modKey(tgt), "")
		}
		for _, s := range f.SideFx {
			if tgt := r.resolve(p, s); tgt != "" {
				edges.add(modKey(p), modKey(tgt), "")
			}
		}
	}

	// Resolve each file's names on its own; the indexes above are read-only
	// from here.
	fileEdges := make([]edgeAcc, len(order))
	fileURLs := make([][]URLRef, len(order))
	parallel(len(order), func(fi int) {
		p, f := order[fi], r.files[order[fi]]
		edges := edgeAcc{}
		fileEdges[fi] = edges
		// binding: local name -> target key (or module key for namespace imports)
		binds := map[string]string{}
		nsBinds := map[string]string{} // ns local -> target file
		for _, im := range f.Imports {
			tgt := r.resolve(p, im.Spec)
			if tgt == "" {
				continue
			}
			if im.Imported == "*" {
				nsBinds[im.Local] = tgt
				binds[im.Local] = modKey(tgt)
				if im.Require {
					// module.exports = X makes the require value X.
					if k := resolveExport(tgt, "default", 0); k != "" {
						binds[im.Local] = k
					}
				}
				continue
			}
			k := resolveExport(tgt, im.Imported, 0)
			if k == "" {
				k = modKey(tgt)
			}
			binds[im.Local] = k
		}
		localDecl := map[string]string{}
		for _, d := range f.Decls {
			if d.Name != "<export>" && d.Name != "<destructure>" && d.Owner == "" {
				if _, ok := localDecl[d.Name]; !ok {
					localDecl[d.Name] = key(p, d.Name)
				}
			}
		}
		// Assign every token to its enclosing decl or the module body
		// (members follow their class, so they win), and class bodies'
		// tokens to their class.
		owner := make([]string, len(f.Toks))
		class := make([]string, len(f.Toks))
		mod := modKey(p)
		for i := range owner {
			owner[i] = mod
		}
		for _, d := range f.Decls {
			k := mod
			if d.Name != "<export>" && d.Name != "<destructure>" {
				k = key(p, d.Name)
			}
			for i := d.Start; i < d.End && i < len(owner); i++ {
				owner[i] = k
				if d.Kind == "class" {
					class[i] = k
				}
			}
		}
		t := f.Toks
		for i, tk := range t {
			from := owner[i]
			if tk.Kind == 'i' {
				if i > 0 && t[i-1].Kind == 'p' && t[i-1].Text == "." {
					// Property access: a member when the receiver is this
					// in its class, or names the class (C.m).
					if i > 1 && t[i-2].Kind == 'i' && !(i > 2 && isTSDot(t[i-3])) {
						recv := ""
						if t[i-2].Text == "this" {
							recv = class[i]
						} else if k, ok := binds[t[i-2].Text]; ok {
							recv = k
						} else if k, ok := localDecl[t[i-2].Text]; ok {
							recv = k
						}
						if recv != "" && members[recv+"."+tk.Text] {
							edges.add(from, recv+"."+tk.Text, "")
						}
					}
					continue // not a binding use
				}
				if tgtFile, ok := nsBinds[tk.Text]; ok && i+2 < len(t) && t[i+1].Text == "." && t[i+2].Kind == 'i' {
					if k := resolveExport(tgtFile, t[i+2].Text, 0); k != "" {
						edges.add(from, k, "")
						continue
					}
				}
				if k, ok := binds[tk.Text]; ok {
					edges.add(from, k, "")
				} else if k, ok := localDecl[tk.Text]; ok {
					edges.add(from, k, "")
				}
				// dynamic import('./x'), require('./x')
				if (tk.Text == "import" || tk.Text == "require") && i+2 < len(t) && t[i+1].Text == "(" && t[i+2].Kind == 's' {
					if tgt := r.resolve(p, t[i+2].Text); tgt != "" {
						k := exportIndex[tgt]["default"]
						if k == "" {
							k = modKey(tgt)
						}
						edges.add(from, k, "")
					}
				}
			}
		}
		// URL literals, with ${CONST} substituted where the const is known.
		for i, tk := range t {
			if tk.Kind != 's' && tk.Kind != 't' {
				continue
			}
			val := expandTemplate(tk.Text, func(expr string) (string, bool) {
				k, ok := binds[expr]
				if !ok {
					k, ok = localDecl[expr]
				}
				if !ok {
					return "", false
				}
				v, ok := constVals[k]
				return v, ok
			}, constVals, binds, localDecl, 0)
			for _, m := range svcURLRe.FindAllStringSubmatch(val, -1) {
				if m[2] == "" || m[2] == "/" {
					continue
				}
				fileURLs[fi] = append(fileURLs[fi], URLRef{From: owner[i], Service: m[1], Path: m[2]})
			}
		}
	})
	for fi := range order {
		edges.merge(fileEdges[fi])
		g.URLRefs = append(g.URLRefs, fileURLs[fi]...)
	}
	g.Edges = append(g.Edges, edges.list()...)
}

func isTSDot(t decls.Tok) bool { return t.Kind == 'p' && t.Text == "." }

// expandTemplate replaces \x00expr\x00 placeholders with known constant
// values (recursively) or "{}" when unknown.
func expandTemplate(s string, lookup func(string) (string, bool), constVals map[string]string, binds, local map[string]string, depth int) string {
	if !strings.Contains(s, "\x00") || depth > 4 {
		return strings.ReplaceAll(s, "\x00", "")
	}
	var b strings.Builder
	parts := strings.Split(s, "\x00")
	for i, part := range parts {
		if i%2 == 0 {
			b.WriteString(part)
			continue
		}
		if v, ok := lookup(part); ok {
			b.WriteString(expandTemplate(v, lookup, constVals, binds, local, depth+1))
		} else {
			b.WriteString("{}")
		}
	}
	return b.String()
}
