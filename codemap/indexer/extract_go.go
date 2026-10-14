package indexer

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/amitbet/pr-manager/codemap/cx"
)

// goExtractor loads one Go module with full type information and records a
// node per top-level declaration plus an edge per resolved reference.
type goExtractor struct {
	repo     string
	repoRoot string
	// workspaceModules lists every go.mod module in the workspace. It decides
	// which interfaces are linked to implementations.
	workspaceModules []Module

	nodes    map[string]*Node
	edges    edgeAcc
	files    map[string]*FileInfo
	fieldOwn map[*types.Package]map[*types.Var]string
	warnings []string
}

func (x *goExtractor) isWorkspacePkg(path string) bool {
	for _, m := range x.workspaceModules {
		if path == m.Path || strings.HasPrefix(path, m.Path+"/") {
			return true
		}
	}
	return false
}

func (x *goExtractor) loadModule(modDir string) error {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports |
			packages.NeedModule,
		Dir:   modDir,
		Tests: false,
		// Services run on linux; loading for darwin would drop linux-only files.
		Env: append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOWORK=off"),
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return err
	}
	errCount := 0
	for _, p := range pkgs {
		for _, e := range p.Errors {
			if strings.Contains(e.Msg, "requires newer Go version") {
				return fmt.Errorf("%s: %s; upgrade pr-manager for full type information", p.PkgPath, e.Msg)
			}
		}
	}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if len(p.Errors) > 0 && isRoot(p, pkgs) {
			errCount += len(p.Errors)
			if len(x.warnings) < 20 {
				x.warnings = append(x.warnings, fmt.Sprintf("%s: %v", p.PkgPath, p.Errors[0]))
			}
		}
	})
	if errCount > 0 {
		fmt.Fprintf(os.Stderr, "  %s: %d type errors (partial info kept)\n", rel(x.repoRoot, modDir), errCount)
	}
	var roots []*packages.Package
	for _, p := range pkgs {
		if p.Types == nil || p.TypesInfo == nil || isMockPkg(p.PkgPath) {
			continue
		}
		roots = append(roots, p)
	}
	if len(roots) == 0 {
		return fmt.Errorf("no typed Go packages loaded from %s", modDir)
	}
	for _, p := range roots {
		x.walkPackage(p)
	}
	x.implEdges(roots)
	return nil
}

func isRoot(p *packages.Package, roots []*packages.Package) bool {
	for _, r := range roots {
		if r == p {
			return true
		}
	}
	return false
}

// Generated mocks implement every interface and are only used by tests.
// Including them would split interface rank between real and fake impls.
func isMockPkg(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		switch seg {
		case "mock", "mocks", "mock_gen", "fake", "fakes", "testutil", "testutils", "testhelpers":
			return true
		}
	}
	return false
}

// isCRDPackage reports whether the package defines Kubernetes API types
// (code-generator or kubebuilder markers). All exported types there are part
// of an object schema stored in customer clusters.
func isCRDPackage(p *packages.Package) bool {
	for _, f := range p.Syntax {
		for _, cg := range f.Comments {
			t := cg.Text()
			if strings.Contains(t, "+groupName=") || strings.Contains(t, "+k8s:deepcopy-gen=package") ||
				strings.Contains(t, "+kubebuilder:object:generate=true") || strings.Contains(t, "+kubebuilder:object:root=true") {
				return true
			}
		}
	}
	return false
}

func (x *goExtractor) walkPackage(p *packages.Package) {
	crdPkg := isCRDPackage(p)
	for _, f := range p.Syntax {
		// f.Pos() is the package clause, which is NoPos when the file failed
		// to parse; FileStart is always set.
		tf := p.Fset.File(f.FileStart)
		if tf == nil {
			continue
		}
		abs := tf.Name()
		if strings.HasSuffix(abs, "_test.go") || !strings.HasPrefix(abs, x.repoRoot) {
			continue // cgo-generated or outside repo
		}
		relFile := rel(x.repoRoot, abs)
		gen := ast.IsGenerated(f)
		fset := p.Fset
		x.files[relFile] = &FileInfo{Path: relFile, Lang: "go", Lines: tf.LineCount(), Generated: gen}
		dir := filepath.ToSlash(filepath.Dir(relFile))
		if dir == "." {
			dir = ""
		}
		mk := func(sym, kind string, start, end token.Pos, exported bool) *Node {
			n := &Node{
				Key: "go:" + p.PkgPath + ":" + sym, Kind: kind, Repo: x.repo,
				File: relFile, Dir: dir, Sym: sym,
				Start: fset.Position(start).Line, End: fset.Position(end).Line,
				Exported: exported,
			}
			if gen {
				n.Tags = append(n.Tags, "generated")
			}
			if old, ok := x.nodes[n.Key]; ok {
				// init() and blank vars repeat; keep the first, widen nothing.
				return old
			}
			x.nodes[n.Key] = n
			return n
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				start := d.Pos()
				if d.Doc != nil {
					start = d.Doc.Pos()
				}
				sym := d.Name.Name
				kind := "func"
				exported := d.Name.IsExported()
				if d.Recv != nil && len(d.Recv.List) > 0 {
					sym = fmt.Sprintf("(%s).%s", recvName(d.Recv.List[0].Type), d.Name.Name)
					kind = "method"
				}
				if sym == "init" || sym == "_" {
					sym = fmt.Sprintf("%s#%d", sym, fset.Position(d.Pos()).Line)
				}
				n := mk(sym, kind, start, d.End(), exported)
				st := cx.Go(d)
				n.Cyclo, n.Nest = st.Cyclo, st.Nest
				x.walkRefs(p, n.Key, d)
			case *ast.GenDecl:
				if d.Tok == token.IMPORT {
					continue
				}
				single := len(d.Specs) == 1
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						start, end := s.Pos(), s.End()
						if single {
							start, end = d.Pos(), d.End()
							if d.Doc != nil {
								start = d.Doc.Pos()
							}
						} else if s.Doc != nil {
							start = s.Doc.Pos()
						}
						n := mk("type "+s.Name.Name, "type", start, end, s.Name.IsExported())
						x.typeFacts(n, s, d)
						if crdPkg && n.Exported && !gen {
							if _, isIface := s.Type.(*ast.InterfaceType); !isIface {
								n.Tags = appendUniq(n.Tags, "crd-type")
							}
						}
						x.walkRefs(p, n.Key, s.Type)
						if s.TypeParams != nil {
							x.walkRefs(p, n.Key, s.TypeParams)
						}
						x.ifaceMethods(p, n, s)
					case *ast.ValueSpec:
						for _, name := range s.Names {
							if name.Name == "_" {
								continue
							}
							start, end := s.Pos(), s.End()
							if single && len(s.Names) == 1 {
								start, end = d.Pos(), d.End()
								if d.Doc != nil {
									start = d.Doc.Pos()
								}
							} else if s.Doc != nil {
								start = s.Doc.Pos()
							}
							kind := d.Tok.String() // var | const
							n := mk(kind+" "+name.Name, kind, start, end, name.IsExported())
							if s.Type != nil {
								x.walkRefs(p, n.Key, s.Type)
							}
							for _, v := range s.Values {
								x.walkRefs(p, n.Key, v)
								if _, ok := v.(*ast.FuncLit); ok {
									st := cx.Go(v)
									n.Cyclo, n.Nest = st.Cyclo, st.Nest
								}
							}
						}
					}
				}
			}
		}
	}
}

// typeFacts records contract signals visible on a type declaration.
func (x *goExtractor) typeFacts(n *Node, s *ast.TypeSpec, d *ast.GenDecl) {
	docs := []*ast.CommentGroup{d.Doc, s.Doc, s.Comment}
	for _, cg := range docs {
		if cg == nil {
			continue
		}
		t := cg.Text()
		if strings.Contains(t, "+kubebuilder:object:root") || strings.Contains(t, "+kubebuilder:resource") || strings.Contains(t, "+genclient") {
			n.Tags = appendUniq(n.Tags, "crd-type")
		}
	}
	st, ok := s.Type.(*ast.StructType)
	if !ok || st.Fields == nil {
		return
	}
	for _, f := range st.Fields.List {
		if f.Tag == nil {
			continue
		}
		tag := reflect.StructTag(strings.Trim(f.Tag.Value, "`"))
		if _, ok := tag.Lookup("parquet"); ok {
			n.Tags = appendUniq(n.Tags, "parquet-struct")
		}
		if _, ok := tag.Lookup("ch"); ok {
			n.Tags = appendUniq(n.Tags, "clickhouse-struct")
		}
		if v, ok := tag.Lookup("db"); ok && v != "-" {
			n.Tags = appendUniq(n.Tags, "db-struct")
		}
		if _, ok := tag.Lookup("json"); ok {
			n.Tags = appendUniq(n.Tags, "json-struct")
		}
	}
}

// ifaceMethods adds one internal node per explicit interface method so that
// calls through the interface can be routed to implementations.
func (x *goExtractor) ifaceMethods(p *packages.Package, typeNode *Node, s *ast.TypeSpec) {
	it, ok := s.Type.(*ast.InterfaceType)
	if !ok || it.Methods == nil {
		return
	}
	for _, m := range it.Methods.List {
		for _, name := range m.Names {
			k := "go:" + p.PkgPath + ":(" + s.Name.Name + ")." + name.Name
			fs := p.Fset
			x.nodes[k] = &Node{
				Key: k, Kind: "iface-method", Repo: x.repo, File: typeNode.File, Dir: typeNode.Dir,
				Sym: "(" + s.Name.Name + ")." + name.Name, Start: fs.Position(m.Pos()).Line, End: fs.Position(m.End()).Line,
				Exported: name.IsExported(), Internal: true,
			}
			x.edges.add(k, typeNode.Key, "")
		}
	}
}

func (x *goExtractor) walkRefs(p *packages.Package, from string, root ast.Node) {
	info := p.TypesInfo
	ast.Inspect(root, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		obj := info.Uses[id]
		if obj == nil {
			return true
		}
		if k := x.objKey(obj); k != "" {
			x.edges.add(from, k, "")
		}
		return true
	})
}

// objKey returns the node key for a package-level object, a method, or a
// struct field (mapped to its owning type). Locals and builtins return "".
func (x *goExtractor) objKey(obj types.Object) string {
	pkg := obj.Pkg()
	if pkg == nil {
		return ""
	}
	switch o := obj.(type) {
	case *types.Func:
		o = o.Origin()
		sig, _ := o.Type().(*types.Signature)
		if sig == nil || sig.Recv() == nil {
			if o.Parent() != pkg.Scope() {
				return ""
			}
			return "go:" + pkg.Path() + ":" + o.Name()
		}
		rt := sig.Recv().Type()
		star := ""
		if pt, ok := rt.(*types.Pointer); ok {
			star = "*"
			rt = pt.Elem()
		}
		switch t := rt.(type) {
		case *types.Named:
			return "go:" + t.Obj().Pkg().Path() + ":(" + star + t.Obj().Name() + ")." + o.Name()
		case *types.Alias:
			if nt, ok := types.Unalias(t).(*types.Named); ok {
				return "go:" + nt.Obj().Pkg().Path() + ":(" + star + nt.Obj().Name() + ")." + o.Name()
			}
		}
		return ""
	case *types.TypeName:
		if o.Parent() != pkg.Scope() {
			return ""
		}
		return "go:" + pkg.Path() + ":type " + o.Name()
	case *types.Const:
		if o.Parent() != pkg.Scope() {
			return ""
		}
		return "go:" + pkg.Path() + ":const " + o.Name()
	case *types.Var:
		o = o.Origin()
		if o.IsField() {
			return x.fieldOwner(o)
		}
		if o.Parent() != pkg.Scope() {
			return ""
		}
		return "go:" + pkg.Path() + ":var " + o.Name()
	}
	return ""
}

func (x *goExtractor) fieldOwner(v *types.Var) string {
	pkg := v.Pkg()
	m, ok := x.fieldOwn[pkg]
	if !ok {
		m = map[*types.Var]string{}
		sc := pkg.Scope()
		for _, name := range sc.Names() {
			tn, ok := sc.Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			st, ok := tn.Type().Underlying().(*types.Struct)
			if !ok {
				continue
			}
			k := "go:" + pkg.Path() + ":type " + name
			for i := 0; i < st.NumFields(); i++ {
				m[st.Field(i)] = k
			}
		}
		x.fieldOwn[pkg] = m
	}
	return m[v]
}

// implEdges connects interface methods to concrete implementations found in
// this module. Interfaces can come from any workspace module, so a service
// implementing a shared-library or generated-client interface is linked too.
func (x *goExtractor) implEdges(roots []*packages.Package) {
	type iface struct {
		named *types.Named
		it    *types.Interface
	}
	var ifaces []iface
	seenPkg := map[*types.Package]bool{}
	var collect func(tp *types.Package)
	collect = func(tp *types.Package) {
		if tp == nil || seenPkg[tp] || !x.isWorkspacePkg(tp.Path()) || isMockPkg(tp.Path()) {
			return
		}
		seenPkg[tp] = true
		sc := tp.Scope()
		for _, name := range sc.Names() {
			tn, ok := sc.Lookup(name).(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			nt, ok := tn.Type().(*types.Named)
			if !ok || nt.TypeParams().Len() > 0 {
				continue
			}
			it, ok := nt.Underlying().(*types.Interface)
			if !ok || it.NumMethods() == 0 {
				continue
			}
			ifaces = append(ifaces, iface{nt, it})
		}
		for _, imp := range tp.Imports() {
			collect(imp)
		}
	}
	for _, p := range roots {
		collect(p.Types)
	}
	// Index concrete named types by method name.
	byMethod := map[string][]*types.Named{}
	for _, p := range roots {
		sc := p.Types.Scope()
		for _, name := range sc.Names() {
			tn, ok := sc.Lookup(name).(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			nt, ok := tn.Type().(*types.Named)
			if !ok || nt.TypeParams().Len() > 0 {
				continue
			}
			if _, isIface := nt.Underlying().(*types.Interface); isIface || strings.HasPrefix(name, "Mock") || strings.HasPrefix(name, "Fake") || strings.HasPrefix(name, "Unimplemented") {
				continue
			}
			ms := types.NewMethodSet(types.NewPointer(nt))
			for i := 0; i < ms.Len(); i++ {
				byMethod[ms.At(i).Obj().Name()] = append(byMethod[ms.At(i).Obj().Name()], nt)
			}
		}
	}
	for _, ifc := range ifaces {
		first := ifc.it.Method(0).Name()
		for _, nt := range byMethod[first] {
			var recv types.Type
			switch {
			case types.Implements(nt, ifc.it):
				recv = nt
			case types.Implements(types.NewPointer(nt), ifc.it):
				recv = types.NewPointer(nt)
			default:
				continue
			}
			ms := types.NewMethodSet(recv)
			for i := 0; i < ifc.it.NumMethods(); i++ {
				im := ifc.it.Method(i)
				sel := ms.Lookup(im.Pkg(), im.Name())
				if sel == nil {
					continue
				}
				// Embedded interface methods resolve to the embedded
				// interface's node, which is where callers point too.
				from := x.objKey(im)
				if to := x.objKey(sel.Obj()); to != "" {
					x.edges.add(from, to, "impl")
				}
			}
		}
	}
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return "*" + recvName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	case *ast.ParenExpr:
		return recvName(t.X)
	}
	return "?"
}

func extractGo(repo, repoRoot string, mods []Module, allMods []Module, g *Graph) {
	x := &goExtractor{
		repo: repo, repoRoot: repoRoot, workspaceModules: allMods,
		nodes: map[string]*Node{}, edges: edgeAcc{}, files: map[string]*FileInfo{},
		fieldOwn: map[*types.Package]map[*types.Var]string{},
	}
	for _, m := range mods {
		dir := filepath.Join(repoRoot, m.Dir)
		if err := x.loadModule(dir); err != nil {
			x.warnings = append(x.warnings, fmt.Sprintf("load %s: %v; using declarations only, without typed call edges", m.Path, err))
			x.parseModule(m)
		}
	}
	keys := make([]string, 0, len(x.nodes))
	for k := range x.nodes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		g.Nodes = append(g.Nodes, x.nodes[k])
	}
	g.Edges = append(g.Edges, x.edges.list()...)
	for _, f := range x.files {
		g.Files = append(g.Files, *f)
	}
	g.Warnings = append(g.Warnings, x.warnings...)
}
