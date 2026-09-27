package triage

import (
	"context"
	"fmt"
	"strings"

	"github.com/amitbet/pr-manager/llm"
)

// Overview tells the reviewer what the PR as a whole does, before they
// read its units: why it was made, how it does it, and what it may break.
type Overview struct {
	Why    string   `json:"why"`
	How    []string `json:"how"`
	Issues []string `json:"issues,omitempty"`
}

const overviewSystem = `You write the overview a reviewer reads before reviewing a pull request unit by unit.

You get the PR title, its description, its commit messages, and the review notes of every change unit (headline, summary, issues found, things to check). Write:
- why: 1-2 sentences on the problem or goal behind the PR. Take it from the description and commit messages; when they don't say, infer it from the changes and say it is inferred.
- how: 1-4 short points on how the PR does it: the approach and the main changes, grouped by what they do, not file by file.
- issues: the potential problems the change can cause, at most 4, each one short sentence: behavior other code or users will notice, broken contracts (API, wire or JSON shape, schema, persisted formats), migrations or rollout order, and the worst issues found in review. Only problems the notes or the diff support; no generic advice like "add tests". Leave it empty when there are none.
Be concise and concrete; name the code (functions, flags, files) where it helps. Don't restate the title.`

var overviewTool = llm.ToolDefinition{
	Name:        "submit_overview",
	Description: "Submit the PR overview.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"why":    map[string]any{"type": "string", "description": "1-2 sentences."},
			"how":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "1-4 short points."},
			"issues": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "At most 4. Empty when there are none."},
		},
		"required": []string{"why", "how", "issues"},
	},
}

const (
	overviewBodyChars    = 4000
	overviewCommitsChars = 4000
	overviewUnitsChars   = 24000
)

// WriteOverview asks l for the overview of a PR from its description,
// commit messages and its units' review notes.
func WriteOverview(ctx context.Context, l llm.LLMTool, pr *PRInfo, units []*Unit) (*Overview, error) {
	args, _, err := llm.CallTool(ctx, l, []llm.ChatMessage{
		{Role: "system", Content: overviewSystem},
		{Role: "user", Content: overviewPrompt(pr, units)},
	}, overviewTool, reviewMaxTokens)
	if err != nil {
		return nil, fmt.Errorf("overview: %w", err)
	}
	ov := &Overview{How: stringList(args["how"]), Issues: stringList(args["issues"])}
	ov.Why, _ = args["why"].(string)
	ov.Why = strings.TrimSpace(ov.Why)
	if ov.Why == "" && len(ov.How) == 0 {
		return nil, fmt.Errorf("overview: empty reply")
	}
	return ov, nil
}

func stringList(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// overviewPrompt lists the units most important first, and cuts the list
// when it gets long; "no review" units only get their headline.
func overviewPrompt(pr *PRInfo, units []*Unit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PR title: %s\n", pr.Title)
	if body := strings.TrimSpace(pr.Body); body != "" {
		fmt.Fprintf(&b, "\nDescription:\n%s\n", clip(body, overviewBodyChars))
	} else {
		b.WriteString("\nDescription: (none)\n")
	}
	if len(pr.Commits) > 0 {
		b.WriteString("\nCommit messages:\n")
		n := 0
		for i, c := range pr.Commits {
			line := "- " + strings.ReplaceAll(strings.TrimSpace(c), "\n", "\n  ") + "\n"
			if n+len(line) > overviewCommitsChars {
				fmt.Fprintf(&b, "(%d more commits)\n", len(pr.Commits)-i)
				break
			}
			b.WriteString(line)
			n += len(line)
		}
	}

	sorted := append([]*Unit(nil), units...)
	SortByBucket(sorted)
	c := (&Report{Units: units}).Counts()
	fmt.Fprintf(&b, "\nChange units (%d: %d human review, %d skim, %d no review), most important first:\n", len(units), c[BucketHuman], c[BucketSkim], c[BucketNone])
	n := 0
	for i, u := range sorted {
		s := unitNote(u)
		if n+len(s) > overviewUnitsChars {
			fmt.Fprintf(&b, "(%d more units left out)\n", len(sorted)-i)
			break
		}
		b.WriteString(s)
		n += len(s)
	}
	return b.String()
}

func unitNote(u *Unit) string {
	var b strings.Builder
	where := u.File
	if u.Symbol != "" {
		where += " " + u.Symbol
	}
	h := u.Headline
	if h == "" {
		h = u.Decision.Headline
	}
	fmt.Fprintf(&b, "- [%s] %s: %s\n", u.Decision.Bucket, where, h)
	if u.Decision.Bucket == BucketNone {
		return b.String()
	}
	if u.Summary != "" {
		fmt.Fprintf(&b, "  %s\n", u.Summary)
	}
	for _, is := range u.Issues {
		fmt.Fprintf(&b, "  issue (%s): %s. %s\n", is.Severity, is.Title, is.Detail)
	}
	for _, f := range u.Focus {
		fmt.Fprintf(&b, "  check: %s\n", f)
	}
	return b.String()
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "\n(cut)"
}

// CommitMessages are the messages of the commits in base..head, oldest
// first, without merges. Errors give none: the overview does without.
func CommitMessages(ctx context.Context, dir, base, head string) []string {
	if dir == "" || base == "" || head == "" {
		return nil
	}
	out, err := GitCtx(ctx, dir, "log", "--no-merges", "--reverse", "--max-count=100", "--format=%B%x1e", base+".."+head)
	if err != nil {
		return nil
	}
	var msgs []string
	for _, m := range strings.Split(out, "\x1e") {
		if m = strings.TrimSpace(m); m != "" {
			msgs = append(msgs, m)
		}
	}
	return msgs
}
