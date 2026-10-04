package triage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const sumBase = `package svc

func Sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

func Other() {}
`

const sumHead = `package svc

func Sum(xs []int) int {
	n := 0
	for _, x := range xs {
		if x < 0 && n > 0 {
			continue
		} else if x > 100 {
			switch {
			case x > 1000:
				return -1
			}
		}
		n += x
	}
	return n
}

func Other() {}
`

// TestLikelihoodChange builds a repo where alice wrote svc/sum.go and bob,
// new to it, makes Sum more complex without touching its test.
func TestLikelihoodChange(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	git := func(email string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=x", "GIT_AUTHOR_EMAIL="+email, "GIT_COMMITTER_NAME=x", "GIT_COMMITTER_EMAIL="+email)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(p, s string) {
		t.Helper()
		os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("a@x", "init", "-q", "-b", "main")
	write("svc/sum.go", sumBase)
	write("svc/sum_test.go", "package svc\n")
	git("a@x", "add", ".")
	git("a@x", "commit", "-q", "-m", "add sum")
	base := git("a@x", "rev-parse", "HEAD")
	write("svc/sum.go", sumHead)
	git("b@x", "commit", "-q", "-am", "fix: skip negatives")
	head := git("b@x", "rev-parse", "HEAD")

	src, err := FromGit(dir, base, head)
	if err != nil {
		t.Fatal(err)
	}
	src.Title = "fix: skip negatives"
	units := BuildUnits(src.Files, src.Content, 0)
	var u *Unit
	for _, x := range units {
		if x.Symbol == "Sum" {
			u = x
		}
	}
	if u == nil {
		t.Fatalf("no Sum unit in %v", units)
	}
	lc := newLikelihoodCtx(src, nil)
	lk := lc.assess(u, src.Files[0])
	m := lk.Metrics
	// Sum goes from cyclo 2 (1 + for) to 6 (1 + for, if, &&, else if, case)
	// and nests three deep.
	if m.Cyclo != 6 || m.CycloBase != 2 || m.Nest != 3 || len(m.Decls) != 1 || m.Decls[0] != "Sum" {
		t.Errorf("complexity = %+v", m)
	}
	if m.AuthorFileCommits != 0 || m.AuthorRepoCommits != 0 || !m.TestGap || !m.FixPR {
		t.Errorf("change metrics = %+v", m)
	}
	ids := map[string]int{}
	for _, f := range lk.Factors {
		ids[f.ID] = f.Points
	}
	for _, id := range []string{"complexity_added", "complexity", "new_to_file", "newcomer", "test_gap", "fix_pr"} {
		if ids[id] == 0 {
			t.Errorf("missing factor %s in %+v", id, lk.Factors)
		}
	}
	if ids["complexity_added"] != 8 { // +4 cyclo x 2
		t.Errorf("complexity_added = %d", ids["complexity_added"])
	}
	// 8 added complexity + 8 new to file + 6 newcomer + 6 test gap + 4 fix PR + 1 complexity
	if lk.Score != 33 {
		t.Errorf("likelihood = %d %s", lk.Score, lk.Level)
	}
	for _, n := range lk.Notes {
		if strings.Contains(n, "author experience unknown") {
			t.Errorf("author experience should be known: %v", lk.Notes)
		}
	}

	// The same change with its test updated and by the file's author: no
	// test gap, no experience factors.
	write("svc/sum_test.go", "package svc\n\n// covers negatives\n")
	git("a@x", "commit", "-q", "-am", "test sum")
	git("a@x", "reset", "-q", "--soft", base)
	git("a@x", "commit", "-q", "-m", "skip negatives")
	head2 := git("a@x", "rev-parse", "HEAD")
	src2, err := FromGit(dir, base, head2)
	if err != nil {
		t.Fatal(err)
	}
	lc2 := newLikelihoodCtx(src2, nil)
	for _, x := range BuildUnits(src2.Files, src2.Content, 0) {
		if x.Symbol != "Sum" {
			continue
		}
		lk2 := lc2.assess(x, fileOf(src2, x.File))
		if lk2.Metrics.TestGap || lk2.Metrics.AuthorFileCommits != 1 || lk2.Score >= lk.Score {
			t.Errorf("with test and owner: %+v score %d vs %d", lk2.Metrics, lk2.Score, lk.Score)
		}
	}
}

func TestLikelihoodSkipsRuleNone(t *testing.T) {
	lc := newLikelihoodCtx(&Source{}, nil)
	u := &Unit{File: "go.sum", Decision: Decision{Bucket: BucketNone, Source: "rule", Reason: "generated file (go.sum)"}}
	if lk := lc.assess(u, FileDiff{Path: "go.sum"}); lk.Score != 0 || len(lk.Notes) != 1 {
		t.Errorf("rule-none unit = %+v", lk)
	}
}

func TestLikelihoodComplete(t *testing.T) {
	m := testMap(t)
	src := "package svc\n\nfunc Calm() int {\n\treturn 1\n}\n"
	content := func(string) ([]byte, error) { return []byte(src), nil }
	for _, c := range []struct {
		name string
		file string
		cm   *CodeMap
		want bool
	}{
		{"in the map, parsed", "svc/calm.go", &CodeMap{Map: m, Repo: "svc"}, true},
		{"no code map", "svc/calm.go", nil, false},
		{"not in the map", "svc/other.go", &CodeMap{Map: m, Repo: "svc"}, false},
		{"no parser", "svc/calm.yaml", &CodeMap{Map: m, Repo: "svc"}, false},
	} {
		u := hunkUnit(c.file, "-x: 1", "+x: 2")
		f := FileDiff{Path: c.file, Status: StatusModified}
		s := &Source{Files: []FileDiff{f}, Content: content, BaseContent: content}
		if lk := newLikelihoodCtx(s, c.cm).assess(u, f); lk.Complete != c.want {
			t.Errorf("%s: complete = %v, want %v (notes %v)", c.name, lk.Complete, c.want, lk.Notes)
		}
	}
}
