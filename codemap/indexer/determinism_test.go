package indexer

import (
	"os"
	"path/filepath"
	"testing"
)

// Two builds of the same tree write the same records: ties on rank and
// types sharing a last name segment must not resolve by map order.
func TestBuildDeterministic(t *testing.T) {
	root := filepath.Join(t.TempDir(), "det")
	files := map[string]string{
		"lib/core.py":    "def shared(x):\n    return x\n",
		"svc/a/one.py":   "from lib.core import shared\n\ndef one(x):\n    return shared(x)\n",
		"svc/b/two.py":   "from lib.core import shared\n\ndef two(x):\n    return shared(x)\n",
		"svc/c/three.py": "from lib.core import shared\n\ndef three(x):\n    return shared(x)\n",
		"j/Outer.java":   "package j;\npublic class Outer {\n  public static class Item { public void run() {} }\n  public static class Box { public static class Item { public void run() {} } }\n}\n",
		"j/Use.java":     "package j;\npublic class Use {\n  public void go(Item i) { i.run(); }\n}\n",
		"j/Item.java":    "package j;\npublic class Item { public void run() {} }\n",
	}
	for f, src := range files {
		p := filepath.Join(root, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, root, "init", "-q")
	build := func() (string, string) {
		out := t.TempDir()
		if err := cmdBuild([]string{"-C", root, "-output", out, "-cache", t.TempDir()}, true); err != nil {
			t.Fatal(err)
		}
		a, _ := os.ReadFile(filepath.Join(out, "det.jsonl"))
		b, _ := os.ReadFile(filepath.Join(out, "repos.jsonl"))
		return string(a), string(b)
	}
	s1, r1 := build()
	if s1 == "" {
		t.Fatal("no records written")
	}
	for range 4 {
		s2, r2 := build()
		if s1 != s2 || r1 != r2 {
			t.Fatal("two builds of the same tree differ")
		}
	}
}
