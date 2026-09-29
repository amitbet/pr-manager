package triage

import (
	"context"
	"fmt"
	"strings"

	"github.com/amitbet/pr-manager/llm"
)

const summarizeSystem = `You write short review summaries for pull-request changes that a triage step judged low-risk.

For the change unit, write:
- headline: one line, at most 12 words, saying what changed (e.g. "Retry helper now caps attempts at 5"). No "This change...".
- summary: 1-3 sentences on what changed and why it is safe to skip a line-by-line review.
You are also a second opinion. Changing behavior is what most changes are for; that alone is not a reason to set safe=false. Set safe=false only when the triage missed a concrete risk: a defect you report in issues, or a change to a contract other code relies on (API, wire or JSON shape, DB schema, persisted format) that the diff does not show is handled. Say what in escalate_reason.
` + issuesInstructions

var summaryTool = llm.ToolDefinition{
	Name:        "submit_summary",
	Description: "Submit the summary for this change unit.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"headline":        map[string]any{"type": "string", "description": "At most 12 words."},
			"summary":         map[string]any{"type": "string"},
			"safe":            map[string]any{"type": "boolean"},
			"escalate_reason": map[string]any{"type": "string", "description": "Required when safe=false."},
			"issues":          issuesSchema,
		},
		"required": []string{"headline", "summary", "safe", "issues"},
	},
}

const reviewNotesSystem = `You write review notes for pull-request changes that need a human reviewer.

For the change unit, write:
- headline: one line, at most 12 words, saying what changed. No "This change...".
- summary: 1-3 sentences on what changed and why it matters at runtime.
- focus: 1-4 short, specific things the reviewer should verify (e.g. "callers that relied on Framework being set before merge"). No generic advice like "check tests".
` + issuesInstructions

// issuesInstructions makes both prompts a code review, not just a summary.
// Every issue must carry its proof; decodeIssues enforces the caps in Go.
const issuesInstructions = `
Then review the unit for defects. Report a defect only when you can show it goes wrong:
- evidence: quote the line(s) that cause it, from the diff or from code you read.
- failure_scenario: the concrete input or state and the wrong result (crash, wrong value, leak, lost data). "May", "could" or "if X returns Y" when you have not seen that X returns Y is not a scenario.
- introduced_by_pr: false when the behavior already existed before this PR: code moved from removed lines in another unit, or logic the base already had. Such problems are capped at low.
- depends_on_unseen_code: true when the defect exists only if code you have not seen (a library, a caller, a file not shown) behaves a certain way. These become "check" items for the reviewer, not issues.
Look for:
- bugs, wrong conditions, off-by-one, nil/empty handling, swallowed or changed errors
- concurrency, resource leaks, retries/timeouts
- security problems
- breaking a contract other code relies on: API/wire/JSON shape, DB schema or queries, persisted formats, CRDs. If a code map line lists dependent repos, check the change is compatible with them.
Judge impact with the other changes of the PR shown after the diff: a fallback, caller or test elsewhere may limit the problem, or show that it is intended.
Severity: critical = outage, data loss or security hole; high = wrong behavior in production that you can show happening; medium = real risk worth a reviewer's time; low = minor problem with a concrete consequence.
Give the new-file line when there is one. No style, naming or "add tests" remarks. Most correct changes have no defects: an empty list is the right answer then, so don't look for something to report in every hunk.`

// toolsInstructions is added when the reviewer can read the repository.
const toolsInstructions = `
You can read the repository at the PR head: your working directory is its root, and you may read files and search the code. %s
Before reporting a defect that depends on code outside the diff (a library's types or errors, a caller, a config), read that code. Set depends_on_unseen_code only when you could not find it. Keep reading to what the review needs.`

var issuesSchema = map[string]any{
	"type":        "array",
	"description": "Defects found in the unit. Empty when the change looks correct.",
	"items": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"severity":               map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "critical"}},
			"line":                   map[string]any{"type": "integer", "description": "New-file line number, if the issue is on one."},
			"title":                  map[string]any{"type": "string", "description": "At most 12 words."},
			"detail":                 map[string]any{"type": "string", "description": "1-2 sentences: what goes wrong and when."},
			"evidence":               map[string]any{"type": "string", "description": "The code line(s) that cause it, quoted."},
			"failure_scenario":       map[string]any{"type": "string", "description": "Concrete input or state -> wrong result."},
			"introduced_by_pr":       map[string]any{"type": "boolean", "description": "False if the behavior existed before this PR (moved or unchanged logic)."},
			"depends_on_unseen_code": map[string]any{"type": "boolean", "description": "True if it is only a defect when code you have not seen behaves a certain way."},
		},
		"required": []string{"severity", "title", "evidence", "failure_scenario", "introduced_by_pr", "depends_on_unseen_code"},
	},
}

var reviewNotesTool = llm.ToolDefinition{
	Name:        "submit_review_notes",
	Description: "Submit review notes for this change unit.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"headline": map[string]any{"type": "string", "description": "At most 12 words."},
			"summary":  map[string]any{"type": "string"},
			"focus":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "1-4 specific things to verify."},
			"issues":   issuesSchema,
		},
		"required": []string{"headline", "summary", "focus", "issues"},
	},
}

type Summarizer struct {
	LLM llm.LLMTool
	// Critic independently checks each issue. Nil uses LLM in a fresh call.
	Critic llm.LLMTool
	Policy Policy
	// Tools lets providers that support it (the CLIs) read the repository at
	// the PR head while reviewing. Pipeline.Run sets up the workspace.
	Tools bool
	// workspace is what the reviewer may read; nil without Tools.
	workspace *llm.Workspace
}

// prompt is the review prompt: the unit's diff, the rest of the PR, the
// code-map context, and what triage decided.
func (s *Summarizer) prompt(u *Unit, triage string) string {
	p, _ := unitDiff(u, s.Policy.MaxUnitChars)
	return p + u.ReviewContext + reviewContext(u) + lintContext(u) + "\n" + triage
}

// system adds the tools instructions. The review is always in English;
// other languages are translated from it (see Translate).
func (s *Summarizer) system(base string) string {
	ws := s.workspace
	if ws == nil {
		return base
	}
	extra := ""
	if len(ws.ReadDirs) > 0 {
		extra = "Library sources (Go module cache) are under " + strings.Join(ws.ReadDirs, ", ") + "; check go.mod for the versions."
	}
	return base + fmt.Sprintf(toolsInstructions, extra)
}

// Summarize fills u.Summary. For skim-bucket units it is also a second
// opinion: it escalates to human when the call fails. A medium or worse
// issue that survives the critic pins the unit in afterReview; safe=false
// without one only adds a thing to check, so a model that calls every
// behavior change unsafe cannot override the review budget.
// Human units get review notes instead.
func (s *Summarizer) Summarize(ctx context.Context, u *Unit) {
	if u.Decision.Bucket == BucketHuman {
		s.reviewNotes(ctx, u)
		return
	}
	prompt := s.prompt(u, "Triage said: "+string(u.Decision.Bucket)+" ("+u.Decision.Reason+")")
	args, _, err := llm.CallToolIn(ctx, s.LLM, s.workspace, []llm.ChatMessage{
		{Role: "system", Content: s.system(summarizeSystem)},
		{Role: "user", Content: prompt},
	}, summaryTool, reviewMaxTokens)
	if err != nil {
		u.Decision.escalate(BucketHuman, "summary failed: "+err.Error())
		return
	}
	u.Summary, _ = args["summary"].(string)
	u.Headline, _ = args["headline"].(string)
	s.setIssues(ctx, u, args)
	safe, ok := args["safe"].(bool)
	why, _ := args["escalate_reason"].(string)
	why = strings.TrimSpace(why)
	if (!ok || !safe) && why != "" && severityWeight[worstIssue(u.Issues).Severity] < severityWeight["medium"] {
		u.Focus = append(u.Focus, "summarizer: "+why)
	}
}

// reviewNotes writes a summary and a "what to check" list for a human
// unit. Failures leave the unit without notes; it is already human.
func (s *Summarizer) reviewNotes(ctx context.Context, u *Unit) {
	prompt := s.prompt(u, "Triage: human review ("+u.Decision.Reason+")")
	args, _, err := llm.CallToolIn(ctx, s.LLM, s.workspace, []llm.ChatMessage{
		{Role: "system", Content: s.system(reviewNotesSystem)},
		{Role: "user", Content: prompt},
	}, reviewNotesTool, reviewMaxTokens)
	if err != nil {
		return
	}
	u.Summary, _ = args["summary"].(string)
	u.Headline, _ = args["headline"].(string)
	if fs, ok := args["focus"].([]any); ok {
		for _, f := range fs {
			if str, ok := f.(string); ok && strings.TrimSpace(str) != "" {
				u.Focus = append(u.Focus, strings.TrimSpace(str))
			}
		}
	}
	s.setIssues(ctx, u, args)
}

// setIssues decodes issues. Claims that rest on code the reviewer did not
// see become things to check. A reply without an issues field was not a
// review, so it cannot count as "nothing found".
func (s *Summarizer) setIssues(ctx context.Context, u *Unit, args map[string]any) {
	s.setIssuesIn(ctx, u, args, s.prompt(u, ""))
}

// setIssuesIn is setIssues with the text the critic re-reads, which for a
// grouped review is the whole group's prompt: an issue in one member is
// often only checkable against another.
func (s *Summarizer) setIssuesIn(ctx context.Context, u *Unit, args map[string]any, context string) {
	v, ok := args["issues"]
	var checks []string
	u.Issues, checks = decodeIssues(v)
	u.Issues = s.criticize(ctx, u, u.Issues, context)
	u.Reviewed = ok
	for _, c := range checks {
		u.Focus = append(u.Focus, "unverified: "+c)
	}
}

// groupSystem is prepended for a grouped review. The members are related
// changes, so a defect may only be visible across two of them.
const groupSystem = `
You are reviewing several related change units in one pass. They were grouped because a reviewer should read them together: one may call, test or replace another, so a defect can be visible only across two of them.
Return one entry per unit, using the exact unit id given with its diff. Cover every unit, including ones you find nothing wrong with. Anchor each issue to the unit whose diff causes it.`

// groupTool builds the per-member tool schema from a single unit's
// properties, so grouped and ungrouped reviews stay in step.
func groupTool(name, description string, unit map[string]any, required []string) llm.ToolDefinition {
	props := map[string]any{"id": map[string]any{"type": "string", "description": "The unit id this entry reviews, copied exactly."}}
	for k, v := range unit {
		props[k] = v
	}
	return llm.ToolDefinition{
		Name:        name,
		Description: description,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"units": map[string]any{
					"type":        "array",
					"description": "One entry per change unit, in the order given.",
					"items": map[string]any{
						"type":       "object",
						"properties": props,
						"required":   append([]string{"id"}, required...),
					},
				},
			},
			"required": []string{"units"},
		},
	}
}

var (
	groupReviewNotesTool = groupTool("submit_group_review_notes", "Submit review notes for every unit in this group.",
		reviewNotesTool.InputSchema["properties"].(map[string]any),
		[]string{"headline", "summary", "focus", "issues"})
	groupSummaryTool = groupTool("submit_group_summary", "Submit the summary for every unit in this group.",
		summaryTool.InputSchema["properties"].(map[string]any),
		[]string{"headline", "summary", "safe", "issues"})
)

// groupPrompt lays out every member's diff and code-map context, then the
// rest of the PR once for the whole group.
func (s *Summarizer) groupPrompt(g *ReviewGroup) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Review these %d related change units.\n", len(g.Members))
	for _, u := range g.Members {
		p, _ := unitDiff(u, s.Policy.MaxUnitChars)
		fmt.Fprintf(&sb, "\n#### unit id: %s\n%s%s%sTriage said: %s (%s)\n",
			u.ID, p, reviewContext(u), lintContext(u), u.Decision.Bucket, u.Decision.Reason)
	}
	sb.WriteString(g.Context)
	return sb.String()
}

// SummarizeGroup reviews a group in one call and writes the result back to
// each member, so everything downstream still sees per-unit notes. A group
// of one is an ordinary review. Members the reply leaves out are reviewed
// on their own rather than silently going unreviewed.
func (s *Summarizer) SummarizeGroup(ctx context.Context, g *ReviewGroup) {
	human := g.Members[0].Decision.Bucket == BucketHuman
	mixed := false
	for _, u := range g.Members {
		// The two buckets ask different questions and answer with
		// different tools, so a mixed group cannot be one call.
		mixed = mixed || (u.Decision.Bucket == BucketHuman) != human
	}
	if mixed || len(g.Members) == 1 {
		for _, u := range g.Members {
			s.Summarize(ctx, u)
		}
		return
	}
	system, tool := summarizeSystem, groupSummaryTool
	if human {
		system, tool = reviewNotesSystem, groupReviewNotesTool
	}
	prompt := s.groupPrompt(g)
	args, _, err := llm.CallToolIn(ctx, s.LLM, s.workspace, []llm.ChatMessage{
		{Role: "system", Content: s.system(system) + groupSystem},
		{Role: "user", Content: prompt},
	}, tool, groupMaxTokens(len(g.Members)))
	if err != nil {
		// One failed call must not drop a whole group's review.
		for _, u := range g.Members {
			s.Summarize(ctx, u)
		}
		return
	}
	byID := map[string]map[string]any{}
	if list, ok := args["units"].([]any); ok {
		for _, it := range list {
			e, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if id, ok := e["id"].(string); ok {
				byID[id] = e
			}
		}
	}
	for _, u := range g.Members {
		e, ok := byID[u.ID]
		if !ok {
			s.Summarize(ctx, u)
			continue
		}
		s.applyReview(ctx, u, e, prompt, human)
	}
}

// groupMaxTokens scales the output cap with the number of units, since one
// reply now carries what several used to.
func groupMaxTokens(members int) int32 {
	n := int32(members) * reviewMaxTokens / 2
	return min(max(n, reviewMaxTokens), 4*reviewMaxTokens)
}

// applyReview writes one review entry onto a unit. It is the grouped
// equivalent of the tail of Summarize and reviewNotes.
func (s *Summarizer) applyReview(ctx context.Context, u *Unit, args map[string]any, context string, human bool) {
	u.Summary, _ = args["summary"].(string)
	u.Headline, _ = args["headline"].(string)
	if fs, ok := args["focus"].([]any); ok {
		for _, f := range fs {
			if str, ok := f.(string); ok && strings.TrimSpace(str) != "" {
				u.Focus = append(u.Focus, strings.TrimSpace(str))
			}
		}
	}
	s.setIssuesIn(ctx, u, args, context)
	if human {
		return
	}
	safe, ok := args["safe"].(bool)
	why, _ := args["escalate_reason"].(string)
	why = strings.TrimSpace(why)
	if (!ok || !safe) && why != "" && severityWeight[worstIssue(u.Issues).Severity] < severityWeight["medium"] {
		u.Focus = append(u.Focus, "summarizer: "+why)
	}
}
