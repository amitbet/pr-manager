package triage

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/amitbet/pr-manager/llm"
)

const sampleDiff = `diff --git a/svc/retry.go b/svc/retry.go
index 1111111..2222222 100644
--- a/svc/retry.go
+++ b/svc/retry.go
@@ -1,9 +1,9 @@ package svc
 package svc
 
 // Retry calls fn.
 func Retry(fn func() error) error {
-	for i := 0; i < 3; i++ {
+	for i := 0; i < 5; i++ {
 		if err := fn(); err == nil {
 			return nil
 		}
 	}
diff --git a/go.sum b/go.sum
index 1111111..2222222 100644
--- a/go.sum
+++ b/go.sum
@@ -1 +1 @@
-a v1
+a v2
diff --git a/old/name.go b/new/name.go
similarity index 100%
rename from old/name.go
rename to new/name.go
diff --git a/db/migrations/001.sql b/db/migrations/001.sql
new file mode 100644
--- /dev/null
+++ b/db/migrations/001.sql
@@ -0,0 +1 @@
+CREATE TABLE t (id int);
diff --git a/logo.png b/logo.png
Binary files a/logo.png and b/logo.png differ
`

func TestParseDiff(t *testing.T) {
	files, err := ParseDiff(sampleDiff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 5 {
		t.Fatalf("got %d files", len(files))
	}
	if f := files[0]; f.Path != "svc/retry.go" || len(f.Hunks) != 1 || f.Hunks[0].NewStart != 1 || f.Hunks[0].NewLines != 9 {
		t.Errorf("retry.go: %+v", f)
	}
	if f := files[2]; f.Status != StatusRenamed || f.OldPath != "old/name.go" || f.Path != "new/name.go" || len(f.Hunks) != 0 {
		t.Errorf("rename: %+v", f)
	}
	if files[3].Status != StatusAdded || !files[4].Binary {
		t.Errorf("added/binary: %+v %+v", files[3], files[4])
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pat, file string
		want      bool
	}{
		{"go.sum", "sub/go.sum", true},
		{"*.pb.go", "api/v1/x.pb.go", true},
		{"vendor/", "vendor/a/b.go", true},
		{"vendor/", "x/vendor/a.go", true},
		{"vendor/", "vendorx/a.go", false},
		{"**/migrations/**", "db/migrations/001.sql", true},
		{"**/values*.yaml", "values.yaml", true},
		{"**/values*.yaml", "charts/app/values-prod.yaml", true},
		{".github/workflows/", ".github/workflows/ci.yml", true},
		{"Dockerfile", "svc/Dockerfile", true},
		{"*.tf", "infra/main.tf.bak", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.pat, c.file); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v", c.pat, c.file, got)
		}
	}
}

const goSrc = `package svc

import "fmt"

// Retry calls fn.
func Retry(fn func() error) error {
	for i := 0; i < 5; i++ {
		if err := fn(); err == nil {
			return nil
		}
	}
	return fmt.Errorf("gave up")
}

type T struct{}

func (t *T) M() {}
`

func TestBuildUnitsGroupsByDecl(t *testing.T) {
	files := []FileDiff{{Path: "svc/retry.go", Status: StatusModified, Hunks: []Hunk{
		{Header: "@@ -5,3 +5,3 @@", NewStart: 6, Lines: []string{"+x"}},  // Retry
		{Header: "@@ -9,3 +9,3 @@", NewStart: 10, Lines: []string{"+y"}}, // Retry
		{Header: "@@ -17 +17 @@", NewStart: 17, Lines: []string{"+z"}},   // (*T).M
	}}}
	units := BuildUnits(files, func(string) ([]byte, error) { return []byte(goSrc), nil }, 0)
	if len(units) != 2 {
		t.Fatalf("got %d units: %+v", len(units), units)
	}
	if units[0].Symbol != "Retry" || len(units[0].Hunks) != 2 || units[1].Symbol != "(*T).M" {
		t.Errorf("units: %s/%d %s", units[0].Symbol, len(units[0].Hunks), units[1].Symbol)
	}
}

func TestBuildUnitsAddedFile(t *testing.T) {
	h := Hunk{Header: "@@ -0,0 +1,17 @@", NewStart: 1}
	for _, l := range strings.Split(strings.TrimSuffix(goSrc, "\n"), "\n") {
		h.Lines = append(h.Lines, "+"+l)
	}
	files := []FileDiff{{Path: "svc/retry.go", Status: StatusAdded, Hunks: []Hunk{h}}}
	src := func(string) ([]byte, error) { return []byte(goSrc), nil }

	units := BuildUnits(files, src, 0)
	if len(units) != 1 || units[0].ID != "svc/retry.go" || units[0].Symbol != "" || units[0].Line != 1 {
		t.Fatalf("whole file: got %d units: %+v", len(units), units)
	}

	// Over the limit: split by declaration so no unit is truncated.
	units = BuildUnits(files, src, len(h.String())-1)
	if len(units) < 2 {
		t.Fatalf("over limit: got %d units", len(units))
	}
}

func TestPresort(t *testing.T) {
	files, _ := ParseDiff(sampleDiff)
	units := BuildUnits(files, nil, 0)
	p := &Presorter{Policy: DefaultPolicy()}
	rest := p.Presort(units, &Source{Files: files})
	got := map[string]Bucket{}
	for _, u := range units {
		got[u.File] = u.Decision.Bucket
	}
	want := map[string]Bucket{
		"go.sum": BucketNone, "new/name.go": BucketNone,
		"db/migrations/001.sql": BucketHuman, "logo.png": BucketHuman,
	}
	for f, b := range want {
		if got[f] != b {
			t.Errorf("%s: got %q want %q", f, got[f], b)
		}
	}
	if len(rest) != 1 || rest[0].File != "svc/retry.go" {
		t.Errorf("rest: %+v", rest)
	}
}

func TestDecodeDecisionAndThresholds(t *testing.T) {
	th := Thresholds{None: 0.9, Skim: 0.7}
	if d, err := decodeDecision(map[string]any{"bucket": "skim", "confidence": 0.9, "reason": "docs"}); err != nil || d.Bucket != BucketSkim {
		t.Errorf("skim bucket: %v, %v", d, err)
	}
	if _, err := decodeDecision(map[string]any{"bucket": "summary", "confidence": 0.9, "reason": "docs"}); err == nil {
		t.Error("old summary bucket accepted")
	}
	if _, err := decodeDecision(map[string]any{"bucket": "maybe", "confidence": 0.9}); err == nil {
		t.Error("invalid bucket accepted")
	}
	if _, err := decodeDecision(map[string]any{"bucket": "none", "confidence": 0.99}); err == nil {
		t.Error("none without reason accepted")
	}
	d, _ := decodeDecision(map[string]any{"bucket": "none", "confidence": 0.99, "reason": "comment", "risk_signals": []any{"api"}})
	if d.Bucket != BucketSkim {
		t.Errorf("none+risk: %s", d.Bucket)
	}
	cases := []struct {
		in        Bucket
		conf      float64
		truncated bool
		want      Bucket
	}{
		{BucketNone, 0.95, false, BucketNone},
		{BucketNone, 0.8, false, BucketSkim},
		{BucketNone, 0.95, true, BucketSkim},
		{BucketSkim, 0.6, false, BucketHuman},
		{BucketHuman, 0.1, false, BucketHuman},
	}
	for _, c := range cases {
		got := applyThresholds(Decision{Bucket: c.in, Confidence: c.conf}, th, c.truncated).Bucket
		if got != c.want {
			t.Errorf("%s@%.2f trunc=%v: got %s want %s", c.in, c.conf, c.truncated, got, c.want)
		}
	}
}

type fakeLLM struct {
	fn func(req llm.LLMRequest) (*llm.LLMResponse, error)
}

func (f *fakeLLM) Call(_ context.Context, req llm.LLMRequest) (*llm.LLMResponse, error) {
	return f.fn(req)
}
func (f *fakeLLM) ModelID() string { return "fake" }
func (f *fakeLLM) Name() string    { return "fake" }

func toolResp(name string, args map[string]any) *llm.LLMResponse {
	return &llm.LLMResponse{ToolCalls: []llm.ToolCall{{Name: name, Arguments: args}}}
}

func TestPipelineReviewFailureIsHuman(t *testing.T) {
	files, _ := ParseDiff(sampleDiff)
	boom := &fakeLLM{fn: func(llm.LLMRequest) (*llm.LLMResponse, error) { return nil, errors.New("boom") }}
	p := &Pipeline{
		Presorter:   &Presorter{Policy: DefaultPolicy()},
		Summarizer:  &Summarizer{LLM: boom, Critic: boom, Policy: DefaultPolicy()},
		Concurrency: 4,
	}
	for _, u := range p.Run(context.Background(), &Source{Files: files}) {
		if u.File == "svc/retry.go" && (u.Decision.Bucket != BucketHuman || u.Score.Pin != BucketHuman) {
			t.Errorf("failed review should pin human: %+v score=%+v", u.Decision, u.Score)
		}
	}

	// Without a reviewer the classifier places units, and its errors too
	// end in human.
	p.Classifier = &LLMClassifier{LLM: boom, Policy: DefaultPolicy()}
	p.Summarizer = nil
	for _, u := range p.Run(context.Background(), &Source{Files: files}) {
		if u.File == "svc/retry.go" && u.Decision.Bucket != BucketHuman {
			t.Errorf("error should escalate to human: %+v", u.Decision)
		}
	}
}

func TestPipelineReviewFilterOnlyReviewsSelectedUnits(t *testing.T) {
	files, err := ParseDiff(fileDiff("a.go") + fileDiff("b.go"))
	if err != nil {
		t.Fatal(err)
	}
	var classified []string
	var mu sync.Mutex
	classify := &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		mu.Lock()
		classified = append(classified, req.Messages[1].Content)
		mu.Unlock()
		return toolResp("submit_triage", map[string]any{"bucket": "human", "change_kind": "behavior", "confidence": 0.9, "reason": "changed"}), nil
	}}
	calls := 0
	review := &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		calls++
		if !strings.Contains(req.Messages[1].Content, "File: a.go") {
			t.Errorf("reviewed another file: %s", req.Messages[1].Content)
		}
		return toolResp("submit_analysis", map[string]any{"bucket": "skim", "change_kind": "refactor", "confidence": 0.9, "reason": "r",
			"summary": "checked", "issues": []any{}}), nil
	}}
	p := &Pipeline{
		Presorter:    &Presorter{Policy: DefaultPolicy()},
		Classifier:   &LLMClassifier{LLM: classify, Policy: DefaultPolicy()},
		Summarizer:   &Summarizer{LLM: review, Policy: DefaultPolicy()},
		ReviewFilter: func(u *Unit) bool { return u.File == "a.go" },
	}
	for _, u := range p.Run(context.Background(), &Source{Files: files}) {
		if u.Reviewed != (u.File == "a.go") {
			t.Errorf("%s reviewed=%v", u.File, u.Reviewed)
		}
	}
	if calls != 1 {
		t.Errorf("review calls = %d", calls)
	}
	// The classifier places only what the review left out.
	if len(classified) != 1 || !strings.Contains(classified[0], "File: b.go") {
		t.Errorf("classified %d prompts, want b.go only: %v", len(classified), classified)
	}
}

func TestSplitHunkAcrossDecls(t *testing.T) {
	// One hunk touching Retry (line 7) and (*T).M (line 17).
	h := Hunk{OldStart: 6, NewStart: 6}
	for n := 6; n <= 17; n++ {
		switch n {
		case 7:
			h.Lines = append(h.Lines, "-\tfor i := 0; i < 3; i++ {", "+\tfor i := 0; i < 5; i++ {")
		case 17:
			h.Lines = append(h.Lines, "-func (t *T) M() {}", "+func (t *T) M() { println() }")
		default:
			h.Lines = append(h.Lines, " ctx")
		}
	}
	files := []FileDiff{{Path: "svc/retry.go", Status: StatusModified, Hunks: []Hunk{h}}}
	units := BuildUnits(files, func(string) ([]byte, error) { return []byte(goSrc), nil }, 0)
	if len(units) != 2 || units[0].Symbol != "Retry" || units[1].Symbol != "(*T).M" {
		for _, u := range units {
			t.Logf("%s: %s", u.Symbol, u.Diff())
		}
		t.Fatalf("got %d units", len(units))
	}
	if units[1].Line != 17 {
		t.Errorf("M line = %d", units[1].Line)
	}
	if strings.Contains(units[0].Diff(), "println") || !strings.Contains(units[1].Diff(), "println") {
		t.Errorf("lines in wrong unit")
	}
}

func TestGoBoilerplate(t *testing.T) {
	mk := func(sym string, lines ...string) *Unit {
		return &Unit{File: "x.go", Symbol: sym, Hunks: []Hunk{{Lines: lines}}}
	}
	cases := []struct {
		u    *Unit
		want bool
	}{
		{mk("imports", "+import (", `+	"fmt"`, `+	prom "github.com/x/y"`, "+", "+)"), true},
		{mk("imports", `-	"os"`, `+	"strings" // for Cut`), true},
		{mk("imports", `+	_ "net/http/pprof"`), false},
		{mk("imports", `+import . "x"`), false},
		{mk("", "+package snapshot", "+"), true},
		{mk("", "+package snapshot", "+var x = 1"), false},
		{mk("Retry", `+	"fmt"`), false},
	}
	for i, c := range cases {
		if _, got := goBoilerplate(c.u); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

func TestHunkHeaderEmptyRange(t *testing.T) {
	h, err := parseHunkHeader("@@ -0,0 +1,3 @@")
	if err != nil || h.OldStart != 1 || h.OldLines != 0 || h.NewStart != 1 || h.NewLines != 3 {
		t.Errorf("added file: %+v %v", h, err)
	}
	h, _ = parseHunkHeader("@@ -10,2 +12,0 @@ func X()")
	if h.NewStart != 13 || h.OldStart != 10 {
		t.Errorf("pure deletion: %+v", h)
	}
}

func TestReviewerPlacesUnitsAndWritesNotes(t *testing.T) {
	files, _ := ParseDiff(sampleDiff)
	review := &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		if req.Tools[0].Name != "submit_analysis" {
			t.Errorf("got tool %s", req.Tools[0].Name)
		}
		return toolResp("submit_analysis", map[string]any{"bucket": "human", "change_kind": "behavior", "confidence": 0.95, "reason": "retry budget",
			"headline": "Retries 3 → 5", "summary": "More retries.", "focus": []any{"callers with tight deadlines", ""}, "issues": []any{}}), nil
	}}
	unused := &fakeLLM{fn: func(llm.LLMRequest) (*llm.LLMResponse, error) {
		t.Error("the classifier must not run when the reviewer places units")
		return nil, errors.New("unused")
	}}
	p := &Pipeline{
		Presorter:  &Presorter{Policy: DefaultPolicy()},
		Classifier: &LLMClassifier{LLM: unused, Policy: DefaultPolicy()},
		Summarizer: &Summarizer{LLM: review, Policy: DefaultPolicy()},
	}
	for _, u := range p.Run(context.Background(), &Source{Files: files}) {
		if u.File != "svc/retry.go" {
			continue
		}
		if u.Decision.Bucket != BucketHuman || u.Decision.Source != "fake/fake" || u.Summary != "More retries." || u.Headline != "Retries 3 → 5" || len(u.Focus) != 1 {
			t.Errorf("analysis not applied: %+v", u)
		}
		if u.Score == nil || u.Score.Classified != BucketHuman {
			t.Errorf("score should start from the reviewer's bucket: %+v", u.Score)
		}
		if len(u.Decision.Escalated) != 0 {
			t.Errorf("notes must not escalate: %v", u.Decision.Escalated)
		}
	}
}

func TestSplitByInnermostDecl(t *testing.T) {
	javaSrc := "package x;\n\nclass C {\n    int f = 1;\n\n    void a() {\n        run(1);\n    }\n\n    void b() {\n        run(2);\n    }\n}\n"
	pySrc := "class C:\n    X = 1\n\n    def a(self):\n        return 1\n\n    def b(self):\n        return 2\n"
	csSrc := "namespace X;\n\nclass C\n{\n    int f = 1;\n    public int P => f;\n    void A()\n    {\n        Run(1);\n    }\n}\n"
	rsSrc := "pub struct C {\n    f: u32,\n}\n\nimpl C {\n    const K: u32 = 1;\n    fn a(&self) -> u32 {\n        run(1)\n    }\n}\n"
	// hunk rewrites the given lines of src (1-based), from line `from` to `to`.
	hunk := func(src string, from, to int, changed ...int) Hunk {
		lines := strings.Split(src, "\n")
		h := Hunk{OldStart: from, NewStart: from, OldLines: to - from + 1, NewLines: to - from + 1}
		for n := from; n <= to; n++ {
			l := lines[n-1]
			if slices.Contains(changed, n) {
				h.Lines = append(h.Lines, "-"+l, "+"+l+" // changed")
			} else {
				h.Lines = append(h.Lines, " "+l)
			}
		}
		return h
	}
	for _, c := range []struct {
		path, src string
		h         Hunk
		want      []string
	}{
		{"src/main/java/x/C.java", javaSrc, hunk(javaSrc, 3, 12, 4, 7, 11), []string{"C", "C.a", "C.b"}},
		{"pkg/c.py", pySrc, hunk(pySrc, 1, 8, 2, 5, 8), []string{"C", "C.a", "C.b"}},
		{"src/X/C.cs", csSrc, hunk(csSrc, 3, 11, 5, 6, 9), []string{"C", "C.P", "C.A"}},
		{"src/c.rs", rsSrc, hunk(rsSrc, 1, 10, 2, 6, 8), []string{"C", "C::K", "C::a"}},
	} {
		content := func(string) ([]byte, error) { return []byte(c.src), nil }
		files := []FileDiff{{Path: c.path, Status: StatusModified, Hunks: []Hunk{c.h}}}
		units := BuildUnits(files, content, 0)
		var got []string
		for _, u := range units {
			got = append(got, u.Symbol)
			names, _ := resolveNames(u, c.path, content)
			if len(names) != 1 || names[0] != u.Symbol {
				t.Errorf("%s %s: map names = %v", c.path, u.Symbol, names)
			}
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: units = %v, want %v", c.path, got, c.want)
		}
	}
}
