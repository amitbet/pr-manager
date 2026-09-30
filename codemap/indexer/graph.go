package indexer

import (
	"sort"
	"time"

	"github.com/amitbet/pr-manager/codemap/githist"
)

// Raw per-repo graph produced by the extractors and cached under
// .cache/codemap/graphs. Ranking always runs over the union of all cached
// graphs, so re-extracting one repo is enough to refresh the whole map.

const extractorVersion = 13

// Node is one addressable unit of code: a Go declaration, a TS/JS top-level
// declaration or class member, a Java type or method, a Python function, class, method or
// module-level variable, a C# type or member, a Rust item or impl function,
// a type, function or member of a generic-parser language (shell,
// PowerShell, C, C++, PHP, Scala, Kotlin, Ruby, Swift, Dart), a module body
// (every language but Go), or an OpenAPI operation.
//
// Key formats:
//
//	go:<import path>:<sym>        sym = Func | (*T).M | (T).M | type T | var X | const X
//	ts:<repo>/<file>:<sym>        sym = top-level name | Class.member | <module>
//	java:<repo>/<file>:<sym>      sym = Type | Outer.Inner | Type.method | <module>
//	py:<repo>/<file>:<sym>        sym = func | Class | Class.method | VAR | <module>
//	cs:<repo>/<file>:<sym>        sym = Type | Outer.Inner | Type.Member | <module>
//	rs:<repo>/<file>:<sym>        sym = item | Type::fn | Trait::fn | inner::item | <module>
//	<lang>:<repo>/<file>:<sym>    lang = sh ps1 c cpp php scala kt rb swift dart;
//	                              sym = func | Type | Outer::Inner | Type.method (Type::method in C++, PHP) | <module>
//	api:<repo>/<spec file>:<op>   op  = operationId
type Node struct {
	Key      string   `json:"k"`
	Kind     string   `json:"t"`
	Repo     string   `json:"r"`
	File     string   `json:"f,omitempty"` // repo-relative; empty for stubs
	Dir      string   `json:"d"`           // repo-relative directory ("" = repo root)
	Sym      string   `json:"s"`
	Start    int      `json:"a,omitempty"`
	End      int      `json:"b,omitempty"`
	Exported bool     `json:"x,omitempty"`
	Tags     []string `json:"g,omitempty"` // extraction facts: parquet-struct, crd-type, generated, ...
	Internal bool     `json:"i,omitempty"` // participates in the graph but is not emitted (iface methods, module bodies)
	// Complexity of functions (and vars holding function literals).
	Cyclo int `json:"cy,omitempty"`
	Nest  int `json:"ne,omitempty"`
}

// Edge says From depends on To (From calls/references/implements-through To).
type Edge struct {
	From string `json:"f"`
	To   string `json:"t"`
	N    int    `json:"n"`
	Kind string `json:"k,omitempty"` // "" = reference, impl, contract
}

// FileInfo describes every file the map should know about, including files
// that carry no graph nodes (migrations, helm charts, specs).
type FileInfo struct {
	Path      string `json:"p"`
	Lang      string `json:"l"`
	Lines     int    `json:"n"`
	Generated bool   `json:"g,omitempty"`
}

type Graph struct {
	Version     int        `json:"version"`
	Repo        string     `json:"repo"`
	Commit      string     `json:"commit"`
	Dirty       bool       `json:"dirty"`
	Fingerprint string     `json:"fingerprint"`
	Modules     []Module   `json:"modules,omitempty"`
	Nodes       []*Node    `json:"nodes"`
	Edges       []Edge     `json:"edges"`
	Files       []FileInfo `json:"files"`
	Specs       []Spec     `json:"specs,omitempty"`
	Warnings    []string   `json:"warnings,omitempty"`
	// URLRefs are TS string literals that look like service URLs (/<service>/<path>).
	// They are linked to OpenAPI operations during ranking because the target
	// spec lives in another repo.
	URLRefs []URLRef `json:"urlrefs,omitempty"`
	// History is the repo's commits (trees only) up to HEAD within
	// historyDays; HeadTime is HEAD's commit time, the reference for
	// recency weights.
	History  []githist.Commit `json:"history,omitempty"`
	HeadTime time.Time        `json:"head_time"`
}

type Module struct {
	Path string `json:"path"` // module path from go.mod
	Dir  string `json:"dir"`  // repo-relative module root
}

type URLRef struct {
	From    string `json:"from"`
	Service string `json:"svc"`
	Path    string `json:"path"` // remainder after /<service>, "{}" for unknown segments
}

// edgeAcc accumulates edges with counts.
type edgeAcc map[[2]string]*Edge

func (a edgeAcc) add(from, to, kind string) {
	if from == "" || to == "" || from == to {
		return
	}
	k := [2]string{from, to}
	if e, ok := a[k]; ok {
		e.N++
		return
	}
	a[k] = &Edge{From: from, To: to, N: 1, Kind: kind}
}

// merge adds b's edges and counts to a.
func (a edgeAcc) merge(b edgeAcc) {
	for k, e := range b {
		if old, ok := a[k]; ok {
			old.N += e.N
		} else {
			a[k] = e
		}
	}
}

func (a edgeAcc) list() []Edge {
	out := make([]Edge, 0, len(a))
	for _, e := range a {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}
