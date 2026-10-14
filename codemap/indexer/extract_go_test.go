package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A file whose package clause fails to parse has f.Pos() == NoPos; the
// extractor must not panic looking up its token.File.
func TestGoExtractSurvivesBrokenPackageClause(t *testing.T) {
	root := t.TempDir()
	write := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, p), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/sample\n\ngo 1.21\n")
	write("keep.go", "package sample\nfunc Keep() {}\n")
	write("broken.go", "<<<<<<< HEAD\npackage sample\n")
	var g Graph
	mods := []Module{{Path: "example.com/sample", Dir: "."}}
	extractGo("sample", root, mods, mods, &g)
	var keys []string
	for _, n := range g.Nodes {
		keys = append(keys, n.Key)
	}
	if all := strings.Join(keys, " "); !strings.Contains(all, "sample:Keep") {
		t.Errorf("nodes = %s", all)
	}
}
