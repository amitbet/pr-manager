package indexer

import (
	"testing"

	"github.com/amitbet/pr-manager/codemap/decls"
)

func TestTSResolveJSExtensions(t *testing.T) {
	files := map[string]*decls.TSFile{}
	for _, p := range []string{
		"src/main.ts", "src/foo.ts", "src/comp.tsx", "src/esm.mts", "src/cjs.cts",
		"src/types.d.ts", "src/real.js", "src/real.ts", "src/dir/index.ts",
		"src/ui/index.tsx", "lib/alias.ts", "src/deep/bar.ts",
	} {
		files[p] = &decls.TSFile{Path: p}
	}
	r := &tsResolver{
		files:   files,
		baseURL: "src",
		paths:   [][2]string{{"@lib/*", "../lib/*"}, {"@deep", "deep/bar.js"}},
	}
	for _, c := range []struct{ spec, want string }{
		{"./foo.js", "src/foo.ts"},
		{"./foo", "src/foo.ts"},
		{"./comp.js", "src/comp.tsx"},
		{"./comp.jsx", "src/comp.tsx"},
		{"./esm.mjs", "src/esm.mts"},
		{"./cjs.cjs", "src/cjs.cts"},
		{"./types.js", "src/types.d.ts"},
		{"./real.js", "src/real.js"}, // a real .js file wins
		{"./dir/index.js", "src/dir/index.ts"},
		{"./dir", "src/dir/index.ts"},
		{"./ui/index.js", "src/ui/index.tsx"},
		{"./foo.js?raw", "src/foo.ts"},
		{"@lib/alias.js", "lib/alias.ts"},
		{"@deep", "src/deep/bar.ts"},
		{"deep/bar.js", "src/deep/bar.ts"}, // baseUrl
		{"./missing.js", ""},
		{"./foo.mjs", ""}, // .mjs never names a .ts
	} {
		if got := r.resolve("src/main.ts", c.spec); got != c.want {
			t.Errorf("resolve(%q) = %q, want %q", c.spec, got, c.want)
		}
	}
}
