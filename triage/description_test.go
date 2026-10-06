package triage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/llm"
)

var descSeq = &Sequence{Version: SequenceVersion, Title: "Fetch",
	Participants: []SeqActor{{ID: "c", Label: "Client"}, {ID: "s", Label: "Server"}},
	Steps:        []SeqStep{{From: "c", To: "s", Text: "fetch", Changed: true}}}

func TestWriteDescription(t *testing.T) {
	pr := &PRInfo{Commits: []string{"Add retry"}}
	ov := &Overview{Why: "Flaky links.", How: []string{"Retry helper"}, Issues: []string{"No backoff"}}
	tmpl := "## What?\n<!-- what -->\n## Why?\n## Sequence\n"
	var prompt string
	l := &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		prompt = req.Messages[1].Content
		return toolResp("submit_description", map[string]any{"body": "## What?\nRetries.\n## Why?\nFlaky links.\n## Sequence\n" + SequencePlaceholder + "\n"}), nil
	}}
	body, err := WriteDescription(context.Background(), l, pr, ov, descSeq, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"<pr_template>\n## What?", "Why: Flaky links.", "- No backoff", "- Add retry"} {
		if !strings.Contains(prompt, s) {
			t.Errorf("prompt lacks %q:\n%s", s, prompt)
		}
	}
	if !strings.Contains(body, "## Sequence\n```mermaid\nsequenceDiagram") || strings.Contains(body, SequencePlaceholder) || strings.Count(body, "## Sequence") != 1 {
		t.Errorf("diagram goes where the placeholder was:\n%s", body)
	}
}

func TestPlainDescription(t *testing.T) {
	body := PlainDescription(&Overview{Why: "Flaky links.", How: []string{"Retry helper"}}, descSeq)
	if !strings.HasPrefix(body, "## Why\n\nFlaky links.\n\n## How\n\n- Retry helper\n\n## Sequence\n\n```mermaid\n") {
		t.Errorf("body:\n%s", body)
	}
	if got := PlainDescription(&Overview{Why: "W"}, nil); got != "## Why\n\nW" {
		t.Errorf("without a diagram: %q", got)
	}
	if got := withSequence("a\n"+SequencePlaceholder+"\nb", nil); got != "a\n\nb" {
		t.Errorf("placeholder without a diagram: %q", got)
	}
}

func TestFindPRTemplate(t *testing.T) {
	write := func(dir, p, s string) {
		p = filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0644); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	if got := FindPRTemplate(dir); got != "" {
		t.Errorf("none: %q", got)
	}
	write(dir, "docs/pull_request_template.md", "docs")
	write(dir, ".github/PULL_REQUEST_TEMPLATE.md", "github\n")
	if got := FindPRTemplate(dir); got != "github" {
		t.Errorf(".github first, any case: %q", got)
	}

	one := t.TempDir()
	write(one, ".github/PULL_REQUEST_TEMPLATE/feature.md", "feature")
	if got := FindPRTemplate(one); got != "feature" {
		t.Errorf("a directory of one: %q", got)
	}
	write(one, ".github/PULL_REQUEST_TEMPLATE/bug.md", "bug")
	if got := FindPRTemplate(one); got != "" {
		t.Errorf("a directory of several is skipped: %q", got)
	}
}
