package triage

import (
	"context"
	"strings"
	"testing"
)

// diffSource builds a one-file source from a raw unified diff.
func diffSource(t *testing.T, raw string) *Source {
	t.Helper()
	src, err := FromDiff(raw, "", "")
	if err != nil {
		t.Fatalf("FromDiff: %v", err)
	}
	return src
}

const secretDiff = `diff --git a/cfg.go b/cfg.go
--- a/cfg.go
+++ b/cfg.go
@@ -1,4 +1,7 @@
 package cfg
 
 const Region = "us-east-1"
+const Key = "AKIAIOSFODNN7EXAMPLE"
+const Placeholder = "password = \"changeme-please\""
+var note = "nothing to see"
`

func TestScanSecretsOnlyAddedLines(t *testing.T) {
	src := diffSource(t, secretDiff)
	fs := scanSecrets(src.Files)
	if len(fs) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(fs), fs)
	}
	if fs[0].Rule != "aws-access-key" || fs[0].Line != 4 || fs[0].Severity != lintError {
		t.Fatalf("unexpected finding: %+v", fs[0])
	}
}

func TestScanSecretsSkipsPlaceholders(t *testing.T) {
	for _, line := range []string{
		`password = "changeme-please"`,
		`api_key: "${VAULT_API_KEY}"`,
		`secret = "your-secret-here"`,
		`token = "<REDACTED-VALUE>"`,
	} {
		if fs := scanLine("a.yaml", 1, line); len(fs) > 0 {
			t.Errorf("%q: want no finding, got %+v", line, fs)
		}
	}
	real := `client_secret = "8f3aB2kQ91zXvR40pLmc"`
	if fs := scanLine("a.yaml", 1, real); len(fs) != 1 || fs[0].Severity != lintWarning {
		t.Errorf("%q: want one warning, got %+v", real, fs)
	}
}

func TestLinterRunAttachesToUnitsAndDropsUnchangedLines(t *testing.T) {
	src := diffSource(t, secretDiff)
	units := BuildUnits(src.Files, src.Content, 0)
	l := &Linter{Policy: LintPolicy{Enabled: true, Secrets: true, MaxPerUnit: 10}}
	l.Run(context.Background(), "", src, units)

	total := 0
	for _, u := range units {
		total += len(u.Lint)
	}
	if total != 1 {
		t.Fatalf("want 1 finding across units, got %d", total)
	}
	// A finding that does not sit on an added line never survives.
	kept := onAddedLines([]LintFinding{{Path: "cfg.go", Line: 3}}, addedLines(src.Files))
	if len(kept) != 0 {
		t.Fatalf("finding on an unchanged line was kept: %+v", kept)
	}
}

func TestLintContextTellsTheReviewerNotToRepeat(t *testing.T) {
	u := &Unit{Lint: []LintFinding{{Tool: "golangci-lint", Rule: "errcheck", Line: 12, Message: "Error return value is not checked"}}}
	got := lintContext(u)
	for _, want := range []string{"[golangci-lint errcheck] line 12", "do not repeat"} {
		if !strings.Contains(got, want) {
			t.Errorf("lintContext missing %q:\n%s", want, got)
		}
	}
	if lintContext(&Unit{}) != "" {
		t.Error("a unit with no findings should add nothing to the prompt")
	}
}

func TestLintPolicySet(t *testing.T) {
	var p LintPolicy
	for _, spec := range []string{"auto", ""} {
		p = LintPolicy{}
		if err := p.Set(spec); err != nil || !p.Enabled || p.Tools != nil {
			t.Errorf("Set(%q) = %+v, %v", spec, p, err)
		}
	}
	p = LintPolicy{Enabled: true}
	if err := p.Set("off"); err != nil || p.Enabled {
		t.Errorf(`Set("off") = %+v, %v`, p, err)
	}
	p = LintPolicy{}
	if err := p.Set("ruff, shellcheck"); err != nil || len(p.Tools) != 2 {
		t.Errorf(`Set("ruff, shellcheck") = %+v, %v`, p, err)
	}
	if err := p.Set("nope"); err == nil {
		t.Error("an unknown linter should be rejected, not ignored")
	}
}

func TestParseGolangCIAndESLint(t *testing.T) {
	gci := []byte(`level=info skipping
{"Issues":[{"FromLinter":"errcheck","Text":"Error return value of ` + "`w.Write`" + ` is not checked","Severity":"error","Pos":{"Filename":"/repo/a.go","Line":7,"Column":3}}]}`)
	fs, err := parseGolangCI(gci, "/repo")
	if err != nil || len(fs) != 1 || fs[0].Path != "a.go" || fs[0].Line != 7 || fs[0].Rule != "errcheck" {
		t.Fatalf("parseGolangCI = %+v, %v", fs, err)
	}
	es := []byte(`[{"filePath":"/repo/src/x.ts","messages":[{"ruleId":"no-unused-vars","severity":1,"message":"'y' is defined but never used","line":3,"column":9}]}]`)
	fs, err = parseESLint(es, "/repo")
	if err != nil || len(fs) != 1 || fs[0].Path != "src/x.ts" || fs[0].Severity != lintWarning {
		t.Fatalf("parseESLint = %+v, %v", fs, err)
	}
}

func TestPickSkipsToolsTheRepoIsNotConfiguredFor(t *testing.T) {
	dir := t.TempDir()
	l := &Linter{Policy: LintPolicy{Enabled: true}}
	for _, got := range l.pick(dir, []string{"a.go", "b.ts"}) {
		if len(got.config) > 0 {
			t.Errorf("auto picked %s with no config in the repo", got.name)
		}
	}
}
