package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/triage"
)

// scopeFixture is a Go file with a targeted function, Load, near the top
// and an unrelated one, Far, well below it.
func scopeFixture(t *testing.T) (string, *PRResult, []targetedIssue) {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("package p\n\nfunc Load(m map[string]int, k string) int {\n\tv := m[k]\n\treturn v\n}\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "\n// filler %d", i)
	}
	b.WriteString("\n\nfunc Far() int {\n\treturn 1\n}\n")
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	u := &triage.Unit{ID: "p.go:Load", File: "p.go", Symbol: "Load"}
	hunks := []triage.Hunk{{NewStart: 4, NewLines: 1, Lines: []string{"+\tv := m[k]"}}}
	r := &PRResult{Files: []resultFile{{FileDiff: triage.FileDiff{Path: "p.go"}, Units: []resultUnit{{Unit: u, Hunks: hunks}}}}}
	issues := []targetedIssue{{UnitID: u.ID, File: "p.go", Issue: triage.Issue{Title: "missing key", Line: 4, Severity: "medium"}}}
	return dir, r, issues
}

func hunkAt(line, added int, text ...string) triage.Hunk {
	h := triage.Hunk{NewStart: line, NewLines: added}
	for i := 0; i < added; i++ {
		l := "+\tx := 1"
		if i < len(text) {
			l = "+" + text[i]
		}
		h.Lines = append(h.Lines, l)
	}
	return h
}

func TestFixScope(t *testing.T) {
	dir, r, issues := scopeFixture(t)
	inside := triage.FileDiff{Path: "p.go", Hunks: []triage.Hunk{hunkAt(4, 3)}}
	if note := fixScope(dir, r, issues, []triage.FileDiff{inside}, ""); note != "" {
		t.Fatalf("a fix inside Load: %s", note)
	}

	far := triage.FileDiff{Path: "p.go", Hunks: []triage.Hunk{hunkAt(4, 1), hunkAt(69, 1)}}
	if note := fixScope(dir, r, issues, []triage.FileDiff{far}, ""); !strings.Contains(note, "p.go:69") {
		t.Fatalf("a change in Far: %q", note)
	}

	big := triage.FileDiff{Path: "p.go", Hunks: []triage.Hunk{hunkAt(4, 70)}}
	if note := fixScope(dir, r, issues, []triage.FileDiff{big}, ""); !strings.Contains(note, "adds 70 lines for 1 issue") {
		t.Fatalf("too big: %q", note)
	}

	api := triage.FileDiff{Path: "p.go", Hunks: []triage.Hunk{hunkAt(4, 2, "func LoadOr(m map[string]int, k string, d int) int {", `	fs.BoolVar(&strict, "strict-load", false, "")`)}}
	note := fixScope(dir, r, issues, []triage.FileDiff{api}, "")
	if !strings.Contains(note, "the exported LoadOr") || !strings.Contains(note, "the flag -strict-load") {
		t.Fatalf("new API: %q", note)
	}
	if note := fixScope(dir, r, issues, []triage.FileDiff{api}, "LoadOr: callers need a default; strict-load: asked for in the issue"); note != "" {
		t.Fatalf("named in extra: %q", note)
	}

	// A change the reader asked for is new code by request.
	change := []targetedIssue{changeTarget(&fixChange{Instructions: "add Far2", Files: []string{"p.go"}})}
	if note := fixScope(dir, r, change, []triage.FileDiff{far, big, api}, ""); note != "" {
		t.Fatalf("change: %q", note)
	}
}

func TestScopeBudget(t *testing.T) {
	_, r, issues := scopeFixture(t)
	if b := scopeBudget(r, issues); b != scopeBudgetFloor {
		t.Fatalf("one small issue: %d", b)
	}
	many := make([]targetedIssue, 40)
	for i := range many {
		many[i] = issues[0]
	}
	if b := scopeBudget(r, many); b != scopeBudgetCap {
		t.Fatalf("capped: %d", b)
	}
}
