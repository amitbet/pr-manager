package triage

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/amitbet/pr-manager/llm"
)

const criticSystem = `You are an independent code review critic. Assess one reported defect, not the review as a whole. The reviewer's issue is a claim, not evidence.

Check the changed code, the rest of the PR, and any relevant callers or contracts you can read. Decide whether this PR introduces the claimed wrong behavior in a concrete scenario. Reject claims based on unchanged or moved code, speculation about unseen behavior, intended behavior, or a fix elsewhere in the PR. Do not reject a real defect just because the reviewer described it poorly.

Return valid=false when the claim is false or lacks enough evidence to establish a defect. When valid=true, set severity independently:
- critical: an established outage, data loss, or security hole
- high: wrong production behavior in a demonstrated scenario
- medium: a real defect worth a reviewer's time
- low: a minor defect with a concrete consequence
Use the impact of the actual failure, not the reviewer's proposed severity. Give a short reason for the verdict.`

var criticTool = llm.ToolDefinition{
	Name:        "judge_issue",
	Description: "Validate one review issue and rate its severity.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"valid":    map[string]any{"type": "boolean"},
			"severity": map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "critical"}, "description": "Required when valid=true."},
			"reason":   map[string]any{"type": "string"},
		},
		"required": []string{"valid", "reason"},
	},
}

// criticize uses a fresh conversation for each issue. A failed or malformed
// verdict leaves the original issue in place rather than hiding a defect.
func (s *Summarizer) criticize(ctx context.Context, u *Unit, issues []Issue, context string) []Issue {
	critic := s.Critic
	if critic == nil {
		critic = s.LLM
	}
	if critic == nil || len(issues) == 0 {
		return issues
	}
	kept := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		claim, _ := json.Marshal(issue)
		args, _, err := llm.CallToolIn(ctx, critic, s.workspace, []llm.ChatMessage{
			{Role: "system", Content: s.system(criticSystem)},
			{Role: "user", Content: context + "\nReported issue to assess:\n" + string(claim)},
		}, criticTool, reviewMaxTokens)
		if err != nil {
			kept = append(kept, issue)
			continue
		}
		valid, ok := args["valid"].(bool)
		if !ok {
			kept = append(kept, issue)
			continue
		}
		if !valid {
			continue
		}
		severity, ok := args["severity"].(string)
		severity = strings.ToLower(strings.TrimSpace(severity))
		if !ok || severityWeight[severity] == 0 {
			kept = append(kept, issue)
			continue
		}
		issue.Severity = severity
		issue.Claimed = ""
		issue.Capped = ""
		kept = append(kept, issue)
	}
	return kept
}
