package triage

import (
	"slices"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/llm"
)

var testRelated = []RelatedCode{{
	Change: "github.com/acme/api#41", Title: "Rename user.name", Note: "the server half",
	Dir: "/cache/related-code/acme__api__41", Clone: "/cache/repos/acme/api",
	Head: "h1h1h1h1h1h1h1h1", Base: "b0b0b0b0b0b0b0b0", Files: []string{"api/user.go"},
}}

// A reviewer's workspace reads the related PRs' code, and its prompt says
// where it is and to look there; a library directory is still named as
// one, and only it.
func TestRelatedWorkspace(t *testing.T) {
	ws := &llm.Workspace{Dir: "/w"}
	AddRelated(ws, testRelated)
	if !slices.Equal(ws.ReadDirs, []string{"/cache/related-code/acme__api__41", "/cache/repos/acme/api"}) {
		t.Fatalf("read dirs %v", ws.ReadDirs)
	}
	s := (&Summarizer{workspace: ws}).system("BASE")
	for _, want := range []string{
		"github.com/acme/api#41 \"Rename user.name\" (related because: the server half)",
		"its code at its head: /cache/related-code/acme__api__41",
		"/cache/repos/acme/api (git diff b0b0b0b0b0b0 h1h1h1h1h1h1 is its change)",
		"search for it in their code before calling it missing", "it changes: api/user.go",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("no %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "Library sources") {
		t.Errorf("related code named a library:\n%s", s)
	}
	AddRelated(ws, nil)
	if len(ws.Notes) != 1 {
		t.Fatalf("notes %v", ws.Notes)
	}
}

// The overview and the sequence, which read no code, get the note
// without the directories, as data.
func TestRelatedPrompts(t *testing.T) {
	pr := &PRInfo{Title: "Use full_name", Related: testRelated}
	for name, p := range map[string]string{"overview": overviewPrompt(pr, nil), "sequence": sequencePrompt(pr, nil)} {
		if !strings.Contains(p, "<related_prs>\nRelated PRs:") || !strings.Contains(p, "github.com/acme/api#41") {
			t.Errorf("%s: no related PRs:\n%s", name, p)
		}
		if strings.Contains(p, "/cache/") {
			t.Errorf("%s: directories it can't read:\n%s", name, p)
		}
	}
	if p := overviewPrompt(&PRInfo{Title: "x"}, nil); strings.Contains(p, "related_prs") {
		t.Errorf("related PRs without any:\n%s", p)
	}
}
