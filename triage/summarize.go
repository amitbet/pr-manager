package triage

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/amitbet/pr-manager/llm"
)

// analyzeSystem is the one prompt a unit is reviewed with: the reviewer
// places it in a bucket, describes it and looks for defects in one call,
// so the bucket comes from the model that read the most.
const analyzeSystem = `You review pull-request changes for a Go/Kubernetes codebase. For each change unit you decide who needs to look at it, say what it does, and review it for defects.

First, triage the unit. ` + bucketRules + untrustedData + `

Never call something unused or unreferenced unless you checked: its uses may be in the other changes of the PR shown after the diff, or elsewhere in the repository.
Give change_kind, risk_signals, confidence, and reason: one short sentence on why this bucket (for "none", why behavior cannot change).

Then write:
- headline: one line, at most 12 words, saying what changed (e.g. "Retry helper now caps attempts at 5"). No "This change...".
- summary: 1-3 sentences on what changed and why it matters at runtime; for "skim" and "none", why a line-by-line review can be skipped.
- focus: for "human", 1-4 short, specific things the reviewer should verify (e.g. "callers that relied on Framework being set before merge"). No generic advice like "check tests". Leave it empty for "none".
` + issuesInstructions

// issuesInstructions makes the analysis a code review, not just a summary.
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

// analyzeTool is the triage decision plus the review.
var analyzeTool = func() llm.ToolDefinition {
	props := map[string]any{
		"summary": map[string]any{"type": "string"},
		"focus":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "For human: 1-4 specific things to verify."},
		"issues":  issuesSchema,
	}
	for k, v := range triageTool.InputSchema["properties"].(map[string]any) {
		props[k] = v
	}
	required := append([]string{"summary", "issues"}, triageTool.InputSchema["required"].([]string)...)
	return llm.ToolDefinition{
		Name:        "submit_analysis",
		Description: "Submit the triage decision, summary and review for this change unit.",
		InputSchema: map[string]any{"type": "object", "properties": props, "required": required},
	}
}()

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
// code-map context, and note (see ruleNote).
func (s *Summarizer) prompt(u *Unit, note string) string {
	p, _ := unitDiff(u, s.Policy.MaxUnitChars)
	return p + u.ReviewContext + reviewContext(u) + lintContext(u) + "\n" + note
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

func (s *Summarizer) source() string { return s.LLM.Name() + "/" + s.LLM.ModelID() }

// ruleNote tells the reviewer a rule already placed the unit; the rule's
// bucket stands, and the review only adds notes and issues to it.
func ruleNote(u *Unit) string {
	if u.Decision.Source != "rule" {
		return ""
	}
	return "Triage rule: " + string(u.Decision.Bucket) + " (" + u.Decision.Reason + ")\n"
}

// Analyze triages and reviews one unit in one call: it sets the unit's
// decision (unless a rule made it), summary, focus and issues. A medium
// or worse issue that survives the critic pins the unit in afterReview.
func (s *Summarizer) Analyze(ctx context.Context, u *Unit) {
	args, _, err := llm.CallToolIn(ctx, s.LLM, s.workspace, []llm.ChatMessage{
		{Role: "system", Content: s.system(analyzeSystem)},
		{Role: "user", Content: s.prompt(u, ruleNote(u))},
	}, analyzeTool, reviewMaxTokens)
	if err != nil {
		s.reviewFailed(u, err)
		return
	}
	s.applyReview(ctx, u, args, s.prompt(u, ""))
}

// reviewFailed puts a unit the review could not answer for in human.
func (s *Summarizer) reviewFailed(u *Unit, err error) {
	if u.Decision.Source == "rule" {
		u.Decision.escalate(BucketHuman, "review failed: "+err.Error())
		return
	}
	u.Decision = Decision{Bucket: BucketHuman, Source: s.source(), Reason: "review failed: " + err.Error(), Failed: true}
}

// setIssues decodes issues. Claims that rest on code the reviewer did not
// see become things to check. A reply whose issues field is missing or not
// a list (CLI replies are free text, so "issues": null or "none" happen)
// was not a review, so it cannot count as "nothing found"; neither can a
// non-empty list none of whose entries decode.
func (s *Summarizer) setIssues(ctx context.Context, u *Unit, args map[string]any) {
	s.setIssuesIn(ctx, u, args, s.prompt(u, ""))
}

// setIssuesIn is setIssues with the text the critic re-reads, which for a
// grouped review is the whole group's prompt: an issue in one member is
// often only checkable against another.
func (s *Summarizer) setIssuesIn(ctx context.Context, u *Unit, args map[string]any, context string) {
	list, ok := args["issues"].([]any)
	if !ok {
		u.Issues, u.Reviewed = nil, false
		s.reviewFailed(u, errors.New("review returned no issues list"))
		return
	}
	var checks []string
	u.Issues, checks = decodeIssues(list)
	if len(list) > 0 && len(u.Issues)+len(checks) == 0 {
		u.Reviewed = false
		s.reviewFailed(u, errors.New("review returned no readable issues"))
		return
	}
	u.Issues = s.criticize(ctx, u, u.Issues, context)
	u.Reviewed = true
	for _, c := range checks {
		u.Focus = append(u.Focus, "unverified: "+c)
	}
}

// groupSystem is prepended for a grouped review. The members are related
// changes, so a defect may only be visible across two of them.
const groupSystem = `
You are reviewing several related change units in one pass. They were grouped because a reviewer should read them together: one may call, test or replace another, so a defect can be visible only across two of them.
Return one entry per unit, using the exact unit id given with its diff. Cover every unit, including ones you find nothing wrong with. Pick each unit's bucket on its own merits: a harmless unit next to a risky one is still harmless, and the other way round. Anchor each issue to the unit whose diff causes it.`

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

var groupAnalyzeTool = groupTool("submit_group_analysis", "Submit the triage decision, summary and review for every unit in this group.",
	analyzeTool.InputSchema["properties"].(map[string]any),
	analyzeTool.InputSchema["required"].([]string))

// groupPrompt lays out every member's diff and code-map context, then the
// rest of the PR once for the whole group.
func (s *Summarizer) groupPrompt(g *ReviewGroup) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Review these %d related change units.\n", len(g.Members))
	for _, u := range g.Members {
		p, _ := unitDiff(u, s.Policy.MaxUnitChars)
		fmt.Fprintf(&sb, "\n#### unit id: %s\n%s%s%s%s", u.ID, p, reviewContext(u), lintContext(u), ruleNote(u))
	}
	sb.WriteString(g.Context)
	return sb.String()
}

// AnalyzeGroup reviews a group in one call and writes the result back to
// each member, so everything downstream still sees per-unit decisions and
// notes. A group of one is an ordinary review. Members the reply leaves
// out are reviewed on their own rather than silently going unreviewed.
func (s *Summarizer) AnalyzeGroup(ctx context.Context, g *ReviewGroup) {
	if len(g.Members) == 1 {
		s.Analyze(ctx, g.Members[0])
		return
	}
	prompt := s.groupPrompt(g)
	args, _, err := llm.CallToolIn(ctx, s.LLM, s.workspace, []llm.ChatMessage{
		{Role: "system", Content: s.system(analyzeSystem) + groupSystem},
		{Role: "user", Content: prompt},
	}, groupAnalyzeTool, groupMaxTokens(len(g.Members)))
	if err != nil {
		// One failed call must not drop a whole group's review.
		for _, u := range g.Members {
			s.Analyze(ctx, u)
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
			s.Analyze(ctx, u)
			continue
		}
		s.applyReview(ctx, u, e, prompt)
	}
}

// groupMaxTokens scales the output cap with the number of units, since one
// reply now carries what several used to.
func groupMaxTokens(members int) int32 {
	n := int32(members) * reviewMaxTokens / 2
	return min(max(n, reviewMaxTokens), 4*reviewMaxTokens)
}

// applyReview writes one review answer onto a unit. The decision is the
// reviewer's unless a rule made it; an answer without a usable one leaves
// the unit in human with no confidence, which pins it there.
func (s *Summarizer) applyReview(ctx context.Context, u *Unit, args map[string]any, context string) {
	if u.Decision.Source != "rule" {
		d, err := decodeDecision(args)
		if err != nil {
			d = Decision{Bucket: BucketHuman, Reason: "review gave no usable triage: " + err.Error(), Failed: true}
		} else {
			_, truncated := unitDiff(u, s.Policy.MaxUnitChars)
			d = applyThresholds(d, s.Policy.Thresholds, truncated)
		}
		d.Source = s.source()
		u.Decision = d
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
	s.setIssuesIn(ctx, u, args, context)
}
