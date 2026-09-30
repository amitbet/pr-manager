package codemap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMap(t *testing.T, recs map[string][]Record, meta Meta) string {
	t.Helper()
	dir := t.TempDir()
	for name, rs := range recs {
		var b strings.Builder
		for _, r := range rs {
			j, _ := json.Marshal(r)
			b.Write(j)
			b.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(dir, name+".jsonl"), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	j, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(dir, "meta.json"), j, 0o644)
	return dir
}

func testMap(t *testing.T) *Map {
	ten := 10
	meta := Meta{
		Repos:   map[string]RepoMeta{"svc": {Category: "core"}},
		Weights: Weights{Rank: 0.55, Rollback: 0.45},
		Levels:  map[string]int{"critical": 75, "high": 55, "medium": 35},
		Rules: []PathRule{
			{ID: "db-migration", Score: 95, Floor: 70, Path: []string{"**/migrations/**"}},
			{ID: "test", Score: 0, Cap: &ten, Path: []string{"**/*_test.go"}},
		},
	}
	dir := writeMap(t, map[string][]Record{
		"repos": {{ID: "svc", Level: "repo", Repo: "svc", Impact: 40, Rank: 50, Rollback: 20}},
		"svc": {
			{ID: "svc/dao/", Level: "dir", Repo: "svc", Path: "dao", Impact: 60, Rank: 80, Rollback: 40},
			{ID: "svc/dao/q.go", Level: "file", Repo: "svc", Path: "dao/q.go", Impact: 65, Rank: 85, Rollback: 45},
			{ID: "svc/dao/q.go:(*Queries).Insert", Level: "symbol", Repo: "svc", Path: "dao/q.go", Sym: "(*Queries).Insert", Lines: []int{10, 20}, Impact: 80, Rollback: 65},
			{ID: "svc/dao/q.go:Helper", Level: "symbol", Repo: "svc", Path: "dao/q.go", Sym: "Helper", Lines: []int{22, 30}, Impact: 30},
		},
	}, meta)
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLookupFallback(t *testing.T) {
	m := testMap(t)
	cases := []struct {
		target, matched, basis string
		impact                 int
	}{
		{"svc/dao/q.go:Queries.Insert", "symbol", "svc/dao/q.go:(*Queries).Insert", 80},
		{"svc/dao/q.go:15-24", "symbol", "svc/dao/q.go:(*Queries).Insert", 80}, // spans two, highest impact first
		{"svc/dao/q.go:25", "symbol", "svc/dao/q.go:Helper", 30},
		{"svc/dao/q.go:5", "file", "svc/dao/q.go", 65},
		{"svc/dao/q.go:Missing", "file", "svc/dao/q.go", 65},
		{"svc/dao/new.go", "dir", "svc/dao/", 40},             // half the dir rank + dir rollback
		{"svc/dao/migrations/002.sql", "dir", "svc/dao/", 70}, // floor from path rule
		{"svc/dao/q_test.go", "dir", "svc/dao/", 10},          // cap from path rule
		{"svc/other/x.go", "repo", "svc", 23},
	}
	for _, c := range cases {
		r := m.Lookup(ParseTarget(c.target))
		if r.Matched != c.matched || len(r.Basis) == 0 || r.Basis[0].ID != c.basis || r.Impact != c.impact {
			id := ""
			if len(r.Basis) > 0 {
				id = r.Basis[0].ID
			}
			t.Errorf("%s: matched=%s basis=%s impact=%d, want %s %s %d (notes %v)", c.target, r.Matched, id, r.Impact, c.matched, c.basis, c.impact, r.Notes)
		}
	}
}

func TestParseDiffAndLookupDiff(t *testing.T) {
	diff := `diff --git a/dao/q.go b/dao/q.go
index 1..2 100644
--- a/dao/q.go
+++ b/dao/q.go
@@ -12,3 +12,4 @@ func (q *Queries) Insert() {
 a
+b
 c
diff --git a/dao/migrations/003.sql b/dao/migrations/003.sql
new file mode 100644
--- /dev/null
+++ b/dao/migrations/003.sql
@@ -0,0 +1,2 @@
+create table x();
+
`
	hunks, err := ParseDiff(strings.NewReader(diff))
	if err != nil || len(hunks) != 2 {
		t.Fatalf("hunks = %+v err=%v", hunks, err)
	}
	if hunks[0].OldStart != 12 || hunks[0].OldLines != 3 || hunks[1].OldPath != "" || hunks[1].NewPath != "dao/migrations/003.sql" {
		t.Fatalf("hunks = %+v", hunks)
	}
	rep := testMap(t).LookupDiff("svc", hunks)
	if rep.Impact != 80 || rep.ImpactLevel != "critical" {
		t.Fatalf("report impact = %d %s", rep.Impact, rep.ImpactLevel)
	}
	if rep.Hunks[1].Result.Impact != 70 {
		t.Errorf("new migration impact = %d", rep.Hunks[1].Result.Impact)
	}
}

func TestNormSym(t *testing.T) {
	for in, want := range map[string]string{"(*T).M": "T.M", "(T).M": "T.M", "T.M": "T.M", "type T": "type T", "F": "F"} {
		if got := NormSym(in); got != want {
			t.Errorf("NormSym(%q) = %q", in, got)
		}
	}
}

func TestLookupTSMemberFallsBackToClass(t *testing.T) {
	dir := writeMap(t, map[string][]Record{
		"repos": {{ID: "web", Level: "repo", Repo: "web", Impact: 40}},
		"web": {
			{ID: "web/src/svc.ts", Level: "file", Repo: "web", Path: "src/svc.ts", Impact: 30},
			{ID: "web/src/svc.ts:Svc", Level: "symbol", Repo: "web", Path: "src/svc.ts", Sym: "Svc", Lines: []int{1, 40}, Impact: 70},
			{ID: "web/src/svc.ts:Svc.hot", Level: "symbol", Repo: "web", Path: "src/svc.ts", Sym: "Svc.hot", Lines: []int{5, 9}, Impact: 80},
		},
	}, Meta{Repos: map[string]RepoMeta{"web": {}}, Levels: map[string]int{"critical": 75, "high": 55, "medium": 35}})
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for sym, want := range map[string]string{"Svc.hot": "web/src/svc.ts:Svc.hot", "Svc.cold": "web/src/svc.ts:Svc", "Other.x": "web/src/svc.ts"} {
		res := m.Lookup(Query{Repo: "web", Path: "src/svc.ts", Sym: sym})
		if len(res.Basis) == 0 || res.Basis[0].ID != want {
			t.Errorf("%s: basis = %+v, want %s", sym, res.Basis, want)
		}
	}
}

// A removed SQL "-- comment" reads as "--- comment" and must not be taken
// for the old-path header.
func TestParseDiffDashDashLines(t *testing.T) {
	raw := "diff --git a/q.sql b/q.sql\n--- a/q.sql\n+++ b/q.sql\n@@ -1,3 +1,2 @@\n select 1;\n--- old comment\n+++ new counter\n-x\n@@ -10 +9 @@\n-y\n+z\n" +
		"--- \"a/caf\\303\\251.sql\"\n+++ \"b/caf\\303\\251.sql\"\n@@ -1 +1 @@\n-a\n+b\n"
	hs, err := ParseDiff(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 3 {
		t.Fatalf("hunks = %+v", hs)
	}
	for _, h := range hs[:2] {
		if h.OldPath != "q.sql" || h.NewPath != "q.sql" {
			t.Errorf("hunk %+v: paths overwritten by a hunk line", h)
		}
	}
	if hs[1].OldStart != 10 || hs[2].OldPath != "café.sql" || hs[2].NewPath != "café.sql" {
		t.Errorf("hunks = %+v", hs)
	}
}
