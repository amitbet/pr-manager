package triage

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/llm"
)

func TestWriteOverview(t *testing.T) {
	pr := &PRInfo{Title: "Retry fetches", Body: "Fetch fails on flaky links.", Commits: []string{"Add retry helper\n\nCaps at 5.", "Use it in Fetch"}}
	units := []*Unit{
		{ID: "gen.go", File: "gen.go", Headline: "Regenerated", Summary: "not shown", Decision: Decision{Bucket: BucketNone}},
		{ID: "fetch.go", File: "fetch.go", Symbol: "Fetch", Headline: "Fetch retries", Summary: "Retries 5 times.",
			Decision: Decision{Bucket: BucketHuman}, Issues: []Issue{{Severity: "high", Title: "No backoff"}}, Focus: []string{"callers with deadlines"}},
	}
	var prompt string
	l := &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		prompt = req.Messages[1].Content
		return toolResp("submit_overview", map[string]any{"why": " Flaky links. ", "how": []any{"Retry helper", " "}, "issues": []any{}}), nil
	}}
	ov, err := WriteOverview(context.Background(), l, pr, units)
	if err != nil {
		t.Fatal(err)
	}
	if want := (&Overview{Why: "Flaky links.", How: []string{"Retry helper"}}); !reflect.DeepEqual(ov, want) {
		t.Errorf("overview = %+v, want %+v", ov, want)
	}
	for _, s := range []string{"Fetch fails on flaky links.", "- Add retry helper\n  \n  Caps at 5.", "- Use it in Fetch", "issue (high): No backoff", "check: callers with deadlines", "1 human review, 0 skim, 1 no review"} {
		if !strings.Contains(prompt, s) {
			t.Errorf("prompt lacks %q:\n%s", s, prompt)
		}
	}
	if strings.Index(prompt, "fetch.go") > strings.Index(prompt, "gen.go") || strings.Contains(prompt, "not shown") {
		t.Errorf("human units come first, no-review ones get only a headline:\n%s", prompt)
	}

	empty := &fakeLLM{fn: func(llm.LLMRequest) (*llm.LLMResponse, error) {
		return toolResp("submit_overview", map[string]any{"why": "", "how": []any{}, "issues": []any{}}), nil
	}}
	if _, err := WriteOverview(context.Background(), empty, pr, units); err == nil {
		t.Error("an empty overview should be an error")
	}
}

func TestCommitMessages(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := git("rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "first\n\nbody line")
	git("commit", "-q", "--allow-empty", "-m", "second")
	got := CommitMessages(context.Background(), dir, base, git("rev-parse", "HEAD"))
	if want := []string{"first\n\nbody line", "second"}; !reflect.DeepEqual(got, want) {
		t.Errorf("messages = %q, want %q", got, want)
	}
	if got := CommitMessages(context.Background(), dir, "nope", "HEAD"); got != nil {
		t.Errorf("a bad range gives none, got %q", got)
	}
}
