package indexer

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/amitbet/pr-manager/codemap/cx"
)

// parseModule keeps declarations and complexity when the Go toolchain is absent
// or too new for the type checker compiled into this binary. It adds no edges.
func (x *goExtractor) parseModule(m Module) {
	root := filepath.Join(x.repoRoot, m.Dir)
	tracked := gitFileSet(x.repoRoot)
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if p != root {
				if strings.HasPrefix(entry.Name(), ".") || entry.Name() == "vendor" || entry.Name() == "testdata" || entry.Name() == "node_modules" {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		if tracked != nil && !tracked[rel(x.repoRoot, p)] {
			return nil // ignored or generated outside git: not the repo's code
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			x.warnings = append(x.warnings, fmt.Sprintf("parse %s: %v", p, err))
			return nil
		}
		file := rel(x.repoRoot, p)
		x.files[file] = &FileInfo{Path: file, Lang: "go", Lines: fset.File(f.Pos()).LineCount(), Generated: ast.IsGenerated(f)}
		moduleFile, _ := filepath.Rel(root, p)
		pkg := path.Join(m.Path, filepath.ToSlash(filepath.Dir(moduleFile)))
		dir := path.Dir(file)
		if dir == "." {
			dir = ""
		}
		add := func(sym, kind, name string, n ast.Node) *Node {
			key := "go:" + pkg + ":" + sym
			if old := x.nodes[key]; old != nil {
				return old
			}
			node := &Node{Key: key, Kind: kind, Repo: x.repo, File: file, Dir: dir, Sym: sym,
				Start: fset.Position(n.Pos()).Line, End: fset.Position(n.End()).Line, Exported: ast.IsExported(name)}
			if ast.IsGenerated(f) {
				node.Tags = []string{"generated"}
			}
			x.nodes[key] = node
			return node
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				sym, kind := d.Name.Name, "func"
				if d.Recv != nil && len(d.Recv.List) > 0 {
					sym = "(" + recvName(d.Recv.List[0].Type) + ")." + sym
					kind = "method"
				}
				if sym == "init" || sym == "_" {
					sym = fmt.Sprintf("%s#%d", sym, fset.Position(d.Pos()).Line)
				}
				n := add(sym, kind, d.Name.Name, d)
				stats := cx.Go(d)
				n.Cyclo, n.Nest = stats.Cyclo, stats.Nest
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						add("type "+spec.Name.Name, "type", spec.Name.Name, spec)
					case *ast.ValueSpec:
						for _, name := range spec.Names {
							add(d.Tok.String()+" "+name.Name, d.Tok.String(), name.Name, spec)
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		x.warnings = append(x.warnings, fmt.Sprintf("parse %s: %v", root, err))
	}
}

// trackedFiles is the repo's file list as discovery reads it (tracked plus
// untracked-but-not-ignored), so the fallback walk honours .gitignore. Nil
// when git can't list it; the walk then takes every file.
func gitFileSet(root string) map[string]bool {
	out, err := gitOut(root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, l := range strings.Split(out, "\x00") {
		if l != "" {
			set[l] = true
		}
	}
	return set
}
