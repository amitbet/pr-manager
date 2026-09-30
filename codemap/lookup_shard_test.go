package codemap

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShardReadErrorNotCached(t *testing.T) {
	dir := writeMap(t, map[string][]Record{
		"repos": {{ID: "svc", Level: "repo", Repo: "svc", Impact: 40}},
		"svc":   {{ID: "svc/a.go", Level: "file", Repo: "svc", Path: "a.go", Impact: 60}},
	}, Meta{Repos: map[string]RepoMeta{"svc": {}}, Levels: map[string]int{"critical": 75, "high": 55, "medium": 35}})
	p := filepath.Join(dir, "svc.jsonl")
	good, _ := os.ReadFile(p)
	if err := os.WriteFile(p, append(append([]byte{}, good...), "{bad\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s := m.shard("svc"); s.byID["svc/a.go"] == nil {
		t.Fatal("records read before the error should still be used")
	}
	if _, ok := m.shards["svc"]; ok {
		t.Fatal("a shard that failed to read must not be cached")
	}
	if err := os.WriteFile(p, append(good, `{"id":"svc/b.go","level":"file","repo":"svc","path":"b.go"}`+"\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := m.shard("svc"); s.byID["svc/b.go"] == nil {
		t.Fatal("shard should be re-read after a failure")
	}
	if _, ok := m.shards["svc"]; !ok {
		t.Fatal("a clean read should be cached")
	}
}

func TestLookupNoRepoRecord(t *testing.T) {
	dir := writeMap(t, map[string][]Record{"repos": {}},
		Meta{Repos: map[string]RepoMeta{"svc": {}}, Levels: map[string]int{"critical": 75, "high": 55, "medium": 35}})
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	res := m.Lookup(Query{Repo: "svc", Path: "x/y.go"})
	if res.Matched != "none" || res.ImpactLevel != "low" {
		t.Errorf("res = %+v", res)
	}
}
