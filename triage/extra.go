package triage

import (
	"context"
	"fmt"
	"strings"

	"github.com/amitbet/pr-manager/llm"
)

// Extra reviewers are review skills or prompts the reader runs on a PR
// on top of the built-in review, each on a provider and model of its own,
// when they ask for it. Their findings are pooled with the review's:
// placed on the unit nearest their line, checked by the critic, compared
// by Dedupe, and counted like the review's own issues. Issue.Source names
// the reviewer that raised each one.

// ExtraFinding is one finding an extra reviewer reported, before it is
// placed on a unit.
type ExtraFinding struct {
	File  string
	Issue Issue
}

// ExtraReviewTool is how an extra reviewer reports what it found.
var ExtraReviewTool = llm.ToolDefinition{
	Name:        "submit_findings",
	Description: "Report every problem the review found in this pull request's changes.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"findings": map[string]any{
				"type":        "array",
				"description": "One entry per problem. Empty when the review found none.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file":             map[string]any{"type": "string", "description": "Path relative to the repository root."},
						"line":             map[string]any{"type": "integer", "description": "New-file line the problem is on; 0 if none."},
						"severity":         map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "critical"}},
						"title":            map[string]any{"type": "string", "description": "At most 12 words."},
						"detail":           map[string]any{"type": "string", "description": "1-3 sentences."},
						"evidence":         map[string]any{"type": "string", "description": "The code that causes it, quoted."},
						"failure_scenario": map[string]any{"type": "string", "description": "The concrete input or state, and the wrong result."},
						"introduced_by_pr": map[string]any{"type": "boolean", "description": "false when the behavior was there before this PR."},
					},
					"required": []string{"file", "severity", "title", "failure_scenario"},
				},
			},
		},
		"required": []string{"findings"},
	},
}

// ExtraReviewPrompt asks for a review of base..HEAD in the working
// directory with skill (a skill name or slash command, as the reader's
// CLI knows it) or, without one, with instructions, and for the findings
// in ExtraReviewTool's shape.
func ExtraReviewPrompt(skill, instructions, base string, files []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review the changes of a pull request. Your working directory is the repository at the PR head; the change is `git diff %s` there", base)
	b.WriteString(", in these files:\n")
	for _, f := range files {
		b.WriteString("- " + f + "\n")
	}
	b.WriteByte('\n')
	if skill != "" {
		fmt.Fprintf(&b, "Run the review with `%s`: invoke that skill or command as you would when asked to, on this change, and let it finish.\n", skill)
	}
	if instructions = strings.TrimSpace(instructions); instructions != "" {
		b.WriteString("\n" + instructions + "\n")
	}
	b.WriteString(`
Then report every finding it made with ` + ExtraReviewTool.Name + `, one entry each, in its own words where they fit: the file, the new-file line, the severity, a short title, the detail, the code it quotes, and the failure scenario. Leave out what is not a problem in this change (praise, summaries, notes on code the PR does not touch). Report none rather than invent one.

Do not change, commit or push anything, and post nothing to GitHub: this is a read-only review.`)
	return b.String()
}

// DecodeFindings reads ExtraReviewTool's arguments, with decodeIssues'
// rules: an unknown severity is medium, and a pre-existing problem or one
// without a failure scenario is capped at low.
func DecodeFindings(args map[string]any) ([]ExtraFinding, error) {
	list, ok := args["findings"].([]any)
	if !ok {
		return nil, fmt.Errorf("no findings list in the answer")
	}
	var out []ExtraFinding
	for _, x := range list {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		file, _ := m["file"].(string)
		issues, _ := decodeIssues([]any{m})
		for _, is := range issues {
			out = append(out, ExtraFinding{File: strings.TrimPrefix(strings.TrimSpace(file), "./"), Issue: is})
		}
	}
	return out, nil
}

// PlaceFindings drops the issues an earlier run of reviewer source left
// on units, then puts each finding on the unit of its file whose changed
// lines are nearest its line, marked with source. A title the unit has
// already gets the reviewer's name, since issues are linked by title (see
// IssueRef). It returns the issues placed per unit and how many findings
// had no unit to go on: their file is not part of the change.
func PlaceFindings(units []*Unit, source string, findings []ExtraFinding) (map[*Unit][]Issue, int) {
	byFile := map[string][]*Unit{}
	for _, u := range units {
		kept := u.Issues[:0:0]
		for _, is := range u.Issues {
			if is.Source != source {
				kept = append(kept, is)
			}
		}
		u.Issues = kept
		if len(u.Hunks) > 0 {
			byFile[u.File] = append(byFile[u.File], u)
		}
	}
	placed := map[*Unit][]Issue{}
	lost := 0
	for _, f := range findings {
		us := byFile[f.File]
		if len(us) == 0 {
			lost++
			continue
		}
		best, dist := us[0], -1
		if f.Issue.Line > 0 {
			for _, u := range us {
				if d := lineDistance(u, f.Issue.Line); dist < 0 || d < dist {
					best, dist = u, d
				}
			}
		}
		is := f.Issue
		is.Source = source
		taken := func(title string) bool {
			if findIssue(best, title) >= 0 {
				return true
			}
			for _, p := range placed[best] {
				if p.Title == title {
					return true
				}
			}
			return false
		}
		if taken(is.Title) {
			is.Title += " (" + source + ")"
		}
		for n := 2; taken(is.Title); n++ {
			is.Title = fmt.Sprintf("%s (%s %d)", f.Issue.Title, source, n)
		}
		placed[best] = append(placed[best], is)
	}
	return placed, lost
}

// AddExtraIssues checks each unit's placed issues with the critic, in ws
// when it is set (the critic only reads it), and adds what is kept to the
// unit. Units are checked side by side, at most limit at a time (0: no
// cap). Placing the units again is ApplyIssues, once the PR's findings
// have been compared (Dedupe).
func (s *Summarizer) AddExtraIssues(ctx context.Context, ws *llm.Workspace, placed map[*Unit][]Issue, limit int) {
	s.workspace = ws
	if limit > 0 {
		s.calls = make(chan struct{}, limit)
	}
	type job struct {
		u      *Unit
		issues []Issue
	}
	var jobs []job
	for u, is := range placed {
		jobs = append(jobs, job{u, is})
	}
	kept := make([][]Issue, len(jobs))
	done := make(chan struct{})
	for i, j := range jobs {
		go func() {
			defer func() { done <- struct{}{} }()
			kept[i] = s.criticize(ctx, j.u, j.issues, s.prompt(j.u, ""))
		}()
	}
	for range jobs {
		<-done
	}
	for i, j := range jobs {
		j.u.Issues = append(j.u.Issues, kept[i]...)
	}
}

// ApplyIssues re-places u after issues were added to it: a medium or
// worse one that counts pins it to human, as afterReview does for the
// review's own, and the rest is ApplyDismissals.
func (tp TierPolicy) ApplyIssues(u *Unit) {
	s := u.Score
	if s == nil {
		return
	}
	if w := worstIssue(u.Issues); severityWeight[w.Severity] >= severityWeight["medium"] && !s.PinIssue && s.Pin != BucketHuman {
		s.PinBefore, s.PinBeforeWhy = s.Pin, s.PinWhy
		s.Pin, s.PinIssue = BucketHuman, true
	}
	tp.ApplyDismissals(u)
}

// ReviewWorkspace is the reviewer's workspace for src (see
// reviewWorkspace), for an extra review run after the triage.
func ReviewWorkspace(src *Source) (*llm.Workspace, func(), error) {
	return reviewWorkspace(src, nil)
}
