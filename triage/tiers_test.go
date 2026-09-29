package triage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/codemap"
	"github.com/amitbet/pr-manager/llm"
)

// testMap writes a small code map for repo "svc":
//
//	svc/calm.go:Calm  impact 12 (lines 3-8)    svc/hot.go:Hot  impact 82 (lines 3-8)
//	svc/flaky.go      impact 20, reverted and fixed often (likelihood alone escalates)
//	svc/both.go       impact 60, fixed and reverted (impact and likelihood both high)
//	svc/calm3.go      impact 14, two recent fixes (likelihood blocks demotion)
func testMap(t *testing.T) *codemap.Map {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, v any, lines bool) {
		var b strings.Builder
		if lines {
			for _, r := range v.([]codemap.Record) {
				j, _ := json.Marshal(r)
				b.Write(j)
				b.WriteByte('\n')
			}
		} else {
			j, _ := json.Marshal(v)
			b.Write(j)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("meta.json", codemap.Meta{
		GeneratedAt: "test", Repos: map[string]codemap.RepoMeta{"svc": {Commit: "abc", Category: "core"}},
		Weights: codemap.Weights{Rank: 0.55, Rollback: 0.45},
		Levels:  map[string]int{"critical": 75, "high": 55, "medium": 35},
	}, false)
	write("repos.jsonl", []codemap.Record{{ID: "svc", Level: "repo", Repo: "svc", Impact: 30, Rank: 40, Rollback: 10}}, true)
	write("svc.jsonl", []codemap.Record{
		{ID: "svc/svc/", Level: "dir", Repo: "svc", Path: "svc", Impact: 20, Rank: 30, Rollback: 10, Hist: &codemap.History{Fixes: 8, RecentFixes: 7}},
		{ID: "svc/svc/flaky.go", Level: "file", Repo: "svc", Path: "svc/flaky.go", Impact: 20, Rank: 20, Rollback: 10,
			Hist: &codemap.History{Commits: 15, Fixes: 4, Reverts: 2, Authors: 6, Recent: 13, RecentFixes: 3}},
		{ID: "svc/svc/both.go", Level: "file", Repo: "svc", Path: "svc/both.go", Impact: 60, Rank: 70, Rollback: 45,
			Hist: &codemap.History{Commits: 9, Fixes: 2, Reverts: 1, Authors: 3, Recent: 8, RecentFixes: 2}},
		{ID: "svc/svc/calm3.go", Level: "file", Repo: "svc", Path: "svc/calm3.go", Impact: 14, Rank: 10, Rollback: 10,
			Hist: &codemap.History{Commits: 3, Fixes: 2, Authors: 1, Recent: 2, RecentFixes: 2}},
		{ID: "svc/svc/calm.go", Level: "file", Repo: "svc", Path: "svc/calm.go", Impact: 14, Rank: 10, Rollback: 10},
		{ID: "svc/svc/calm.go:Calm", Level: "symbol", Repo: "svc", Path: "svc/calm.go", Sym: "Calm", Lines: []int{3, 8}, Impact: 12, ImpactLevel: "low", Rank: 8, Rollback: 10},
		{ID: "svc/svc/hot.go", Level: "file", Repo: "svc", Path: "svc/hot.go", Impact: 80, Rank: 99, Rollback: 70},
		{ID: "svc/svc/hot.go:Hot", Level: "symbol", Repo: "svc", Path: "svc/hot.go", Sym: "Hot", Lines: []int{3, 8}, Impact: 82, ImpactLevel: "critical", Rank: 99, Rollback: 70,
			DepRepos: []string{"webapp"}, RollbackTags: []codemap.Tag{{ID: "db-write", Score: 65}}},
		{ID: "svc/svc/ui.ts", Level: "file", Repo: "svc", Path: "svc/ui.ts", Impact: 40, Rank: 50, Rollback: 10},
		{ID: "svc/svc/ui.ts:B", Level: "symbol", Repo: "svc", Path: "svc/ui.ts", Sym: "B", Lines: []int{90, 99}, Impact: 44, Rank: 60, Rollback: 10},
		{ID: "svc/api/v1/user/api.yaml", Level: "file", Repo: "svc", Path: "api/v1/user/api.yaml", Impact: 70, Rank: 90, Rollback: 55},
		{ID: "svc/api/v1/user/api.yaml:GetB", Level: "symbol", Repo: "svc", Path: "api/v1/user/api.yaml", Sym: "GetB", Lines: []int{400, 420}, Impact: 72, Rank: 91, Rollback: 55},
	}, true)
	m, err := codemap.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func fileDiff(path string) string {
	return `diff --git a/` + path + ` b/` + path + `
--- a/` + path + `
+++ b/` + path + `
@@ -4,3 +4,3 @@ func X() {
 a
-	b := 1
+	b := 2
 c
`
}

// scripted answers per file for the reviewer, which also places the unit.
type script struct {
	bucket, kind string
	issues       []any
}

func runScripted(t *testing.T, m *codemap.Map, repo, diff string, answers map[string]script) (map[string]*Unit, map[string]string) {
	t.Helper()
	files, err := ParseDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	pick := func(req llm.LLMRequest) script {
		for f, s := range answers {
			if strings.Contains(req.Messages[1].Content, "File: "+f) {
				return s
			}
		}
		t.Fatalf("no script for prompt %q", req.Messages[1].Content)
		return script{}
	}
	tools := map[string]string{}
	review := &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		s := pick(req)
		for f := range answers {
			if strings.Contains(req.Messages[1].Content, "File: "+f) {
				tools[f] = req.Tools[0].Name
				if strings.Contains(req.Messages[1].Content, "Code map: impact") {
					tools[f] += "+map"
				}
			}
		}
		return toolResp(req.Tools[0].Name, map[string]any{"bucket": s.bucket, "change_kind": s.kind, "confidence": 0.95, "reason": "r",
			"headline": "h", "summary": "s", "focus": []any{"x"}, "issues": s.issues}), nil
	}}
	p := &Pipeline{
		Presorter:  &Presorter{Policy: DefaultPolicy()},
		Summarizer: &Summarizer{LLM: review, Policy: DefaultPolicy()},
	}
	if m != nil {
		p.CodeMap = &CodeMap{Map: m, Repo: repo}
	}
	out := map[string]*Unit{}
	for _, u := range p.Run(context.Background(), &Source{Files: files}) {
		out[u.File] = u
	}
	return out, tools
}

// sortedUnits runs SortByBucket over a result map.
func sortedUnits(m map[string]*Unit) []*Unit {
	var us []*Unit
	for _, u := range m {
		us = append(us, u)
	}
	sort.Slice(us, func(i, j int) bool { return us[i].File < us[j].File })
	SortByBucket(us)
	return us
}

func TestTierMoves(t *testing.T) {
	m := testMap(t)
	diff := fileDiff("svc/calm.go") + fileDiff("svc/calm2.go") + fileDiff("svc/hot.go") + fileDiff("svc/issue.go") + fileDiff("svc/test_like.go") + fileDiff("db/migrations/1.sql") +
		fileDiff("svc/flaky.go") + fileDiff("svc/both.go") + fileDiff("svc/calm3.go")
	medium := []any{map[string]any{"severity": "medium", "line": 5, "title": "b is never checked", "failure_scenario": "b=0 divides by zero"}}
	low := []any{map[string]any{"severity": "low", "title": "log text typo"}}
	units, _ := runScripted(t, m, "svc", diff, map[string]script{
		"svc/calm.go":         {bucket: "skim", kind: "refactor"},
		"svc/calm2.go":        {bucket: "human", kind: "behavior", issues: low},
		"svc/flaky.go":        {bucket: "skim", kind: "behavior"},
		"svc/both.go":         {bucket: "skim", kind: "behavior"},
		"svc/calm3.go":        {bucket: "skim", kind: "refactor"},
		"svc/hot.go":          {bucket: "skim", kind: "behavior"},
		"svc/issue.go":        {bucket: "skim", kind: "behavior", issues: medium},
		"svc/test_like.go":    {bucket: "human", kind: "test"},
		"db/migrations/1.sql": {},
	})
	check := func(file string, want Bucket, total int, why string) *Unit {
		t.Helper()
		u := units[file]
		if u == nil || u.Score == nil {
			t.Fatalf("no scored unit for %s", file)
		}
		if u.Decision.Bucket != want || u.Score.Total != total || !strings.Contains(u.Score.Why, why) {
			t.Errorf("%s: bucket=%s total=%d why=%q, want %s %d %q (score %+v)", file, u.Decision.Bucket, u.Score.Total, u.Score.Why, want, total, why, *u.Score)
		}
		return u
	}
	// Balanced: trust 0.3, human >= 40, skim >= 15.
	check("svc/both.go", BucketHuman, 41, "score 41 = 59 × 0.7 (clean review) → human (≥ 40 on balanced)") // √(60×58)
	check("svc/flaky.go", BucketSkim, 28, "40 × 0.7")                                                      // √(20×80): a clean review is trusted
	check("svc/hot.go", BucketSkim, 22, "→ skim")                                                          // impact 82 is under critical_impact 85
	// Both score none, but they change code: only a rule can skip that.
	check("svc/calm.go", BucketSkim, 5, "12 × kind 0.6 × 0.7 (clean review) → none (< 15 on balanced); raised to skim: the change is more than comments")
	check("svc/calm3.go", BucketSkim, 9, "no budget lifts this")
	check("svc/calm2.go", BucketSkim, 15, "review attention 15")                               // a low issue: half the trust, attention floor
	check("svc/test_like.go", BucketSkim, 7, "raised to skim: the classifier asked for human") // scores none
	if u := check("svc/issue.go", BucketHuman, 45, "any budget"); u.Score.Pin != BucketHuman || u.Issues[0].Line != 5 {
		t.Errorf("issue: %+v", *u.Score)
	}
	check("db/migrations/1.sql", BucketHuman, 0, "force_human")

	// Score orders a bucket.
	var human []string
	for _, u := range sortedUnits(units) {
		if u.Decision.Bucket == BucketHuman {
			human = append(human, u.File)
		}
	}
	if strings.Join(human, " ") != "svc/issue.go svc/both.go db/migrations/1.sql" {
		t.Errorf("human order = %v", human)
	}

	// Other budgets re-bucket without a re-run; pins hold, and floors hold
	// until a budget lets a clean review lift them. The code floor holds on
	// every budget: every unit here changes a code line.
	all := sortedUnits(units)
	bucketsAt := func(budget string) map[string]Bucket {
		t.Helper()
		if err := Rebucket(all, DefaultTierPolicy(), budget); err != nil {
			t.Fatal(err)
		}
		out := map[string]Bucket{}
		for _, u := range all {
			out[u.File] = u.Decision.Bucket
		}
		return out
	}
	most := bucketsAt("most")
	if most["svc/flaky.go"] != BucketHuman || most["svc/hot.go"] != BucketHuman || most["svc/calm.go"] != BucketSkim || most["svc/calm3.go"] != BucketSkim {
		t.Errorf("most: %v", most)
	}
	least := bucketsAt("least")
	if least["svc/both.go"] != BucketSkim || least["svc/issue.go"] != BucketHuman || least["db/migrations/1.sql"] != BucketHuman ||
		least["svc/flaky.go"] != BucketSkim || least["svc/test_like.go"] != BucketSkim || least["svc/calm2.go"] != BucketSkim { // a low issue keeps the floor
		t.Errorf("least: %v", least)
	}
	if u := units["svc/flaky.go"]; !strings.Contains(u.Score.Why, "a behavior change is never skipped lifted by the clean review; raised to skim: the change is more than comments") {
		t.Errorf("flaky on least: %q", u.Score.Why)
	}
	if err := Rebucket(all, DefaultTierPolicy(), "nope"); err == nil {
		t.Error("unknown budget: want an error")
	}
}

// TestBudgetsOnErrorCachePR checks the budget steps against the clean-reviewed
// behavior changes of a real PR (impact, likelihood from its run).
func TestBudgetsOnErrorCachePR(t *testing.T) {
	units := map[string][2]int{
		"fetchFromServer": {63, 53}, "cachedError": {79, 36}, "Client": {78, 36}, "NewClient": {70, 36},
		"isErrorReply": {59, 38}, "GetRecord": {54, 36}, "requestTimeout": {54, 36},
		"getRecord": {43, 40}, "loadMetadata": {39, 41},
	}
	tp := DefaultTierPolicy()
	want := map[string]map[Bucket]int{
		"most":     {BucketHuman: 9},
		"balanced": {BucketHuman: 1, BucketSkim: 8},
		"less":     {BucketSkim: 9},
		"least":    {BucketSkim: 4, BucketNone: 5},
	}
	for budget, counts := range want {
		got := map[Bucket]int{}
		for sym, s := range units {
			u := &Unit{ID: sym, Symbol: sym, Hunks: []Hunk{{}}, Reviewed: true,
				Impact:     &Impact{Score: s[0], Level: "high", Matched: "symbol"},
				Likelihood: &Likelihood{Score: s[1]},
				Decision:   Decision{Bucket: BucketHuman, ChangeKind: "behavior", Confidence: 0.9, Source: "m"},
			}
			tp.prior(u, 0)
			tp.afterReview(u, u.Decision.Bucket)
			if err := Rebucket([]*Unit{u}, tp, budget); err != nil {
				t.Fatal(err)
			}
			got[u.Decision.Bucket]++
			if budget == "balanced" && sym == "fetchFromServer" && u.Decision.Bucket != BucketHuman {
				t.Errorf("fetchFromServer: %s", u.Score.Why)
			}
		}
		if !reflect.DeepEqual(got, counts) {
			t.Errorf("%s: %v, want %v", budget, got, counts)
		}
	}
}

func TestNoMapNoMoves(t *testing.T) {
	answers := map[string]script{"svc/calm.go": {bucket: "skim", kind: "refactor"}}
	units, _ := runScripted(t, nil, "", fileDiff("svc/calm.go"), answers)
	if u := units["svc/calm.go"]; u.Decision.Bucket != BucketSkim || u.Impact != nil || !u.Reviewed || u.Likelihood == nil {
		t.Errorf("without a map: %+v impact=%v likelihood=%v", u.Decision, u.Impact, u.Likelihood)
	}
	// Repo missing from the map: impact unknown, still no demotion.
	units, _ = runScripted(t, testMap(t), "other", fileDiff("svc/calm.go"), answers)
	if u := units["svc/calm.go"]; u.Decision.Bucket != BucketSkim || u.Impact.Known() {
		t.Errorf("unknown repo: %+v impact=%+v", u.Decision, u.Impact)
	}
}

// Base-side sources. The map above says Hot is at lines 3-8; in this base
// it moved to 9-12 and Other now sits at 3-5, so the map's line numbers
// would pin line 4 on the wrong function.
const hotBase = `package svc

func Other() {
	println()
}

// Hot does the hot thing.
// It is hot.
func Hot() {
	x := 1
	_ = x
}
`

const uiBase = `import { x } from './x';

export const A = 1;

export function B() {
  return x;
}
`

const specBase = `openapi: 3.0.3
paths:
  /a:
    get:
      operationId: GetA
      responses: {}
  /b:
    get:
      operationId: GetB
      responses: {}
`

func baseFiles(files map[string]string) ContentFunc {
	return func(p string) ([]byte, error) {
		if s, ok := files[p]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
}

func TestAssessResolvesNamesOnBase(t *testing.T) {
	c := &CodeMap{Map: testMap(t), Repo: "svc"}
	base := baseFiles(map[string]string{"svc/hot.go": hotBase, "svc/ui.ts": uiBase, "api/v1/user/api.yaml": specBase})
	mod := func(p string) FileDiff { return FileDiff{Path: p, Status: StatusModified} }
	del := func(oldLine int) []Hunk { // one modified line on the base side
		return []Hunk{{OldStart: oldLine, OldLines: 1, NewStart: oldLine, NewLines: 1, Lines: []string{"-old", "+new"}}}
	}
	ins := func(before int) []Hunk { // pure insertion before a base line
		return []Hunk{{OldStart: before - 1, OldLines: 1, NewStart: before - 1, NewLines: 2, Lines: []string{" ctx", "+new"}}}
	}
	cases := []struct {
		name    string
		u       *Unit
		f       FileDiff
		base    ContentFunc
		matched string
		basis   string
	}{
		{"head name", &Unit{File: "svc/hot.go", Symbol: "Hot", Hunks: del(10)}, mod("svc/hot.go"), base, "symbol", "svc/svc/hot.go:Hot"},
		{"unparsed head, resolved on base despite stale map lines", &Unit{File: "svc/hot.go", Symbol: "func X() {", Hunks: del(10)}, mod("svc/hot.go"), base, "symbol", "svc/svc/hot.go:Hot"},
		{"renamed declaration uses the base name", &Unit{File: "svc/hot.go", Symbol: "HotRenamed", Hunks: del(9)}, mod("svc/hot.go"), base, "symbol", "svc/svc/hot.go:Hot"},
		{"map line 4 is Other on base, which is not in the map", &Unit{File: "svc/hot.go", Symbol: "func X() {", Hunks: del(4)}, mod("svc/hot.go"), base, "file", "svc/svc/hot.go"},
		{"new declaration after Hot", &Unit{File: "svc/hot.go", Symbol: "NewHelper", Hunks: ins(13)}, mod("svc/hot.go"), base, "file", "svc/svc/hot.go"},
		{"insertion inside Hot", &Unit{File: "svc/hot.go", Symbol: "func X() {", Hunks: ins(11)}, mod("svc/hot.go"), base, "symbol", "svc/svc/hot.go:Hot"},
		{"TS declaration", &Unit{File: "svc/ui.ts", Hunks: del(6)}, mod("svc/ui.ts"), base, "symbol", "svc/svc/ui.ts:B"},
		{"TS import block", &Unit{File: "svc/ui.ts", Hunks: del(1)}, mod("svc/ui.ts"), base, "file", "svc/svc/ui.ts"},
		{"spec operation", &Unit{File: "api/v1/user/api.yaml", Hunks: del(9)}, mod("api/v1/user/api.yaml"), base, "symbol", "svc/api/v1/user/api.yaml:GetB"},
		{"no base: map line numbers", &Unit{File: "svc/hot.go", Symbol: "func X() {", Hunks: del(4)}, mod("svc/hot.go"), nil, "symbol", "svc/svc/hot.go:Hot"},
		{"SQL: file record", &Unit{File: "svc/q.sql", Hunks: del(2)}, mod("svc/q.sql"), base, "dir", "svc/svc/"},
		{"new file", &Unit{File: "svc/hot2.go", Symbol: "Hot"}, FileDiff{Path: "svc/hot2.go", Status: StatusAdded}, base, "dir", "svc/svc/"},
		{"new file a later map already indexes", &Unit{File: "svc/hot.go", Symbol: "Hot"}, FileDiff{Path: "svc/hot.go", Status: StatusAdded}, base, "dir", "svc/svc/"},
	}
	for _, tc := range cases {
		d := c.Assess(tc.u, tc.f, tc.base)
		if d.Matched != tc.matched || d.Basis != tc.basis {
			t.Errorf("%s: matched=%s basis=%s notes=%v, want %s %s", tc.name, d.Matched, d.Basis, d.Notes, tc.matched, tc.basis)
		}
	}
	if d := c.Assess(&Unit{File: "svc/hot.go", Symbol: "func X() {", Hunks: del(4)}, mod("svc/hot.go"), nil); !strings.Contains(strings.Join(d.Notes, " "), "line numbers") {
		t.Errorf("line fallback should say so: %v", d.Notes)
	}
	if !strings.Contains(mapContext(c.Assess(&Unit{File: "svc/hot.go", Symbol: "Hot"}, mod("svc/hot.go"), base)), "webapp") {
		t.Errorf("map context should name dependent repos")
	}
}

func TestParsePolicyTiers(t *testing.T) {
	p, err := ParsePolicy([]byte("tiers:\n  review_budget: less\n  budgets:\n    less: { human: 60 }\n    custom: { trust: 1, human: 90, skim: 50 }\n  kind_weights: { test: 0.5 }\n  critical_impact: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := DefaultTierPolicy()
	b, name, _ := p.Tiers.Budget("")
	if name != "less" || b.Human != 60 || b.Skim != d.Budgets["less"].Skim || b.Trust != d.Budgets["less"].Trust {
		t.Errorf("budget %s = %+v", name, b)
	}
	if p.Tiers.KindWeights["test"] != 0.5 || p.Tiers.KindWeights["refactor"] != d.KindWeights["refactor"] || p.Tiers.CriticalImpact != 0 || p.Tiers.Budgets["custom"].Human != 90 || p.Tiers.Budgets["custom"].Skim != 50 {
		t.Errorf("tiers = %+v", p.Tiers)
	}
	if bs := p.Tiers.OrderedBudgets(); len(bs) != 6 || bs[0].Name != "most" || bs[5].Name != "custom" {
		t.Errorf("ordered = %+v", bs)
	}
	for _, bad := range []string{"tiers: { review_budget: nope }", "tiers: { budgets: { more: { skim: 99 } } }", "tiers: { budgets: { more: { trust: 2 } } }"} {
		if _, err := ParsePolicy([]byte(bad)); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

func TestDecodeIssues(t *testing.T) {
	got, checks := decodeIssues([]any{
		map[string]any{"severity": "HIGH", "line": 3.0, "title": "nil deref", "failure_scenario": "nil config panics"},
		map[string]any{"severity": "weird", "detail": "no title", "failure_scenario": "x"},
		map[string]any{"severity": "low"},
		"junk",
	})
	if len(got) != 2 || got[0].Severity != "high" || got[0].Line != 3 || got[1].Severity != "medium" || got[1].Title != "no title" || len(checks) != 0 {
		t.Errorf("issues = %+v", got)
	}
	if a := attentionScore(got); a != 80 {
		t.Errorf("attention = %d", a)
	}
}

func TestDecodeIssuesEnforcesEvidenceRules(t *testing.T) {
	got, checks := decodeIssues([]any{
		// Guess about a library: a check for the reviewer, not an issue.
		map[string]any{"severity": "high", "title": "Pointer errors bypass detection", "detail": "if the server returns *Error",
			"failure_scenario": "x", "introduced_by_pr": true, "depends_on_unseen_code": true},
		// Moved code: capped at low.
		map[string]any{"severity": "medium", "title": "Reconnect after cancel", "failure_scenario": "expired ctx redials",
			"introduced_by_pr": false, "depends_on_unseen_code": false},
		// No scenario: capped at low.
		map[string]any{"severity": "critical", "title": "Looks racy", "introduced_by_pr": true},
		// Shown, new and concrete: kept.
		map[string]any{"severity": "medium", "title": "Transient errors cached", "failure_scenario": "timeout pins unit for 5m", "introduced_by_pr": true},
	})
	if len(checks) != 1 || !strings.HasPrefix(checks[0], "Pointer errors bypass detection: ") {
		t.Errorf("checks = %q", checks)
	}
	if len(got) != 3 {
		t.Fatalf("issues = %+v", got)
	}
	if is := got[0]; is.Severity != "low" || is.Claimed != "medium" || !is.PreExisting || is.Capped != "pre-existing behavior" {
		t.Errorf("pre-existing = %+v", is)
	}
	if is := got[1]; is.Severity != "low" || is.Claimed != "critical" || is.Capped != "no failure scenario" {
		t.Errorf("no scenario = %+v", is)
	}
	if is := got[2]; is.Severity != "medium" || is.Claimed != "" {
		t.Errorf("kept = %+v", is)
	}
}

func TestScoreFindings(t *testing.T) {
	c := EvalCase{Name: "c", Findings: map[string]FindingLabels{"a.go": {
		MustFind: []FindingRule{
			{Match: "transient.*cach", MinSeverity: "medium"},
			{Match: "leak"},
			{Match: "overflow"},
		},
		MustNotFind: []FindingRule{{Match: "pointer"}, {Match: "reconnect", MinSeverity: "medium"}},
	}}}
	for key, fl := range c.Findings {
		for _, rules := range [][]FindingRule{fl.MustFind, fl.MustNotFind} {
			for i := range rules {
				rules[i].re = regexp.MustCompile("(?is)" + rules[i].Match)
			}
		}
		c.Findings[key] = fl
	}
	units := []*Unit{
		{ID: "a.go:F", File: "a.go", Issues: []Issue{
			{Severity: "medium", Title: "Transient errors cached for 5m"},
			{Severity: "high", Title: "Pointer errors bypass"},
			{Severity: "low", Title: "Reconnect after cancel"}, // allowed at low
		}},
		{ID: "a.go:G", File: "a.go", Issues: []Issue{{Severity: "low", Title: "fd leak"}, {Severity: "low", Title: "overflow"}}},
		{ID: "b.go", File: "b.go", Issues: []Issue{{Severity: "high", Title: "pointer"}}}, // unlabeled file
	}
	var r EvalResult
	r.scoreFindings(c, units)
	if r.MustFind != 3 || r.Found != 3 || len(r.Missed) != 0 || len(r.WrongSeverity) != 0 {
		t.Errorf("must_find: %+v", r)
	}
	if len(r.Broken) != 1 || !strings.Contains(r.Broken[0], "Pointer errors bypass") {
		t.Errorf("broken = %q", r.Broken)
	}
	if r.Issues != 5 || r.TruePos != 3 || r.FalsePos != 1 {
		t.Errorf("issues %d tp %d fp %d", r.Issues, r.TruePos, r.FalsePos)
	}

	units[0].Issues[0].Severity = "low"
	units[1].Issues = nil
	r = EvalResult{}
	r.scoreFindings(c, units)
	if r.Found != 0 || len(r.WrongSeverity) != 1 || len(r.Missed) != 2 {
		t.Errorf("second run: %+v", r)
	}
}

// A test file scores no likelihood, so its score is 0: a test edit the
// reviewer sent to human must stay there on every budget, even after a
// clean review, while one it sent to skim is left to the score.
func TestTestFileKeepsHuman(t *testing.T) {
	answers := map[string]script{
		"svc/calm_test.go":  {bucket: "human", kind: "test"},
		"svc/calm2_test.go": {bucket: "skim", kind: "test"},
	}
	diff := fileDiff("svc/calm_test.go") + fileDiff("svc/calm2_test.go")
	units, _ := runScripted(t, testMap(t), "svc", diff, answers)
	u, other := units["svc/calm_test.go"], units["svc/calm2_test.go"]
	if u == nil || other == nil || !u.Reviewed || !u.Impact.Known() || u.Likelihood == nil || u.Likelihood.Score != 0 {
		t.Fatalf("setup: %+v", u)
	}
	for _, b := range BudgetNames {
		if err := Rebucket([]*Unit{u, other}, DefaultTierPolicy(), b); err != nil {
			t.Fatal(err)
		}
		if u.Decision.Bucket != BucketHuman {
			t.Errorf("%s: human test edit placed %s: %s", b, u.Decision.Bucket, u.Score.Why)
		}
		if other.Score.Pin != "" {
			t.Errorf("%s: a skim test edit should not be pinned: %s", b, other.Score.Why)
		}
	}
}
