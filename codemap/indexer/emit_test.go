package indexer

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/amitbet/pr-manager/codemap"
)

func readRecords(t *testing.T, p string) []codemap.Record {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var recs []codemap.Record
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		var r codemap.Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}
	return recs
}

// The repo record rolls up every directory, not just the top-level ones:
// the root must aggregate after its subdirs have absorbed theirs.
func TestRepoRecordIncludesNestedDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "deep")
	for _, f := range []string{"top.py", "a/one.py", "a/b/two.py", "a/b/c/three.py", "a/b/c/d/four.py"} {
		p := filepath.Join(root, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		src := "def f(x):\n    return x\n\n\ndef g(x):\n    return f(x)\n"
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, root, "init", "-q")
	out := t.TempDir()
	if err := cmdBuild([]string{"-C", root, "-output", out, "-cache", t.TempDir()}, true); err != nil {
		t.Fatal(err)
	}
	files, deep := 0, 0
	for _, r := range readRecords(t, filepath.Join(out, "deep.jsonl")) {
		if r.Level == "file" {
			files += r.Symbols
			if r.Path == "a/b/c/three.py" || r.Path == "a/b/c/d/four.py" {
				deep += r.Symbols
			}
		}
	}
	if deep == 0 {
		t.Fatal("no symbols extracted from the nested files")
	}
	repos := readRecords(t, filepath.Join(out, "repos.jsonl"))
	if len(repos) != 1 {
		t.Fatalf("got %d repo records", len(repos))
	}
	if got := repos[0].Symbols; got != files {
		t.Fatalf("repo record has %d symbols, files under it have %d", got, files)
	}
}
