package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/codemap/githist"
)

func TestHistorySample(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q", "-b", "main")
	gitTest(t, dir, "config", "user.name", "Test")
	gitTest(t, dir, "config", "user.email", "test@example.com")
	commit := func(file, body, msg string) string {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gitTest(t, dir, "add", "-A")
		gitTest(t, dir, "commit", "-q", "-m", msg)
		return gitTest(t, dir, "rev-parse", "HEAD")
	}
	commit("a.go", "package p\n", "start")
	bug := commit("a.go", "package p\n\nfunc F(m map[string]int) int {\n\treturn m[\"k\"] / len(m)\n}\n", "add F")
	clean := commit("b.go", "package p\n\nfunc G() int { return 1 }\n", "add G")
	commit("a.go", "package p\n\nfunc F(m map[string]int) int {\n\tif len(m) == 0 {\n\t\treturn 0\n\t}\n\treturn m[\"k\"] / len(m)\n}\n", "fix a crash in F on an empty map")

	incidents, cleanSet, err := historySample(dir, 5, githist.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	// The crash fix changed no line of F's: it only added the guard, so
	// nothing is blamed yet.
	if len(incidents) != 0 {
		t.Fatalf("incidents from added lines only: %+v", incidents)
	}
	commit("a.go", "package p\n\nfunc F(m map[string]int) int {\n\tif len(m) == 0 {\n\t\treturn 0\n\t}\n\treturn m[\"k\"] / max(len(m), 1)\n}\n", "fix division by zero in F")
	incidents, cleanSet, err = historySample(dir, 5, githist.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	ins := incidents[bug]
	if len(ins) != 1 || ins[0].file != "a.go" || ins[0].line != 4 {
		t.Fatalf("incidents: %+v", incidents)
	}
	found := false
	for _, sha := range cleanSet {
		if sha == bug {
			t.Fatal("an incident commit in the clean set")
		}
		found = found || sha == clean
	}
	if !found {
		t.Fatalf("clean set: %v", cleanSet)
	}

	var b bytes.Buffer
	tallies := map[string]*historyTally{}
	for _, n := range []string{"most", "more", "balanced", "less", "least"} {
		tallies[n] = &historyTally{human: 3, skim: 1, none: 1, cleanHuman: 2, cleanCode: 10}
	}
	printHistory(&b, tallies)
	if !strings.Contains(b.String(), "balanced") || !strings.Contains(b.String(), "1 (20%)") || !strings.Contains(b.String(), "2 of 10 (20%)") {
		t.Fatal(b.String())
	}
}
