package triage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/amitbet/pr-manager/llm"
)

type Decision struct {
	Bucket      Bucket   `json:"bucket"`
	ChangeKind  string   `json:"change_kind,omitempty"`
	RiskSignals []string `json:"risk_signals,omitempty"`
	Confidence  float64  `json:"confidence"`
	Reason      string   `json:"reason"`
	// Headline is a one-line description of the change (at most ~12 words).
	Headline string `json:"headline,omitempty"`
	// Source is "rule", "openjev", or "<provider>/<model>".
	Source string `json:"source"`
	// Escalated records why the classifier's bucket was raised (low
	// confidence, a truncated diff) or the reviewer disagreed with it.
	Escalated []string `json:"escalated,omitempty"`
	// Failed marks a decision that stands in for an error, so it is never
	// kept for a later run.
	Failed bool `json:"-"`
}

func (d *Decision) escalate(to Bucket, why string) {
	if to.rank() > d.Bucket.rank() {
		d.Bucket = to
		d.Escalated = append(d.Escalated, why)
	}
}

// failed is the decision for any unit whose classification errored.
func failed(source string, err error) Decision {
	return Decision{Bucket: BucketHuman, Source: source, Reason: "classification failed: " + err.Error(), Failed: true}
}

type Classifier interface {
	// Classify must always return a usable decision; errors become "human".
	Classify(ctx context.Context, u *Unit) Decision
}

// BatchClassifier classifies several units in one call, which saves the
// per-call cost (a CLI start, the system prompt) that dominates a small
// model's answer. It returns one decision per unit, in order.
type BatchClassifier interface {
	Classifier
	ClassifyBatch(ctx context.Context, units []*Unit) []Decision
}

// BatchPolicy controls how many units share one classify call.
type BatchPolicy struct {
	// MaxUnits caps the units in one call (1: one call per unit).
	MaxUnits int `yaml:"max_units"`
	// MaxChars caps the diff text in one call. A unit at least this big
	// is classified alone.
	MaxChars int `yaml:"max_chars"`
}

const (
	DefaultBatchUnits = 6
	DefaultBatchChars = 16000
)

func DefaultBatchPolicy() BatchPolicy {
	return BatchPolicy{MaxUnits: DefaultBatchUnits, MaxChars: DefaultBatchChars}
}

// classifyBatches packs units into classify calls in their order, which
// keeps a file's units, and a package's files, together.
func classifyBatches(units []*Unit, p BatchPolicy) [][]*Unit {
	var out [][]*Unit
	var cur []*Unit
	chars := 0
	for _, u := range units {
		n := len(u.Diff())
		if len(cur) > 0 && (len(cur) >= p.MaxUnits || chars+n > p.MaxChars) {
			out, cur, chars = append(out, cur), nil, 0
		}
		cur, chars = append(cur, u), chars+n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func batchID(b []*Unit) string {
	ids := make([]string, len(b))
	for i, u := range b {
		ids[i] = u.ID
	}
	return strings.Join(ids, " + ")
}

// DecisionStore keeps classifier decisions between runs. Keys come from
// decisionKey.
type DecisionStore interface {
	Load(key string) (Decision, bool)
	Save(key string, d Decision)
}

// decisionKey is what a unit's decision depends on: the prompts, the
// classifier and policy (salt), and the unit's file, declaration and diff.
// The list of other units in the PR is left out, so adding a unit to a PR
// does not send every other unit back to the model.
func decisionKey(salt string, u *Unit) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00", PromptVersion, salt, u.File, u.Status, u.Symbol)
	h.Write([]byte(u.Diff()))
	return hex.EncodeToString(h.Sum(nil))
}

// applyThresholds moves low-confidence answers up a bucket and forbids
// "none" for units whose diff was truncated.
func applyThresholds(d Decision, th Thresholds, truncated bool) Decision {
	switch d.Bucket {
	case BucketNone:
		if d.Confidence < th.None {
			d.escalate(BucketSkim, fmt.Sprintf("none confidence %.2f < %.2f", d.Confidence, th.None))
		}
	case BucketSkim:
		if d.Confidence < th.Skim {
			d.escalate(BucketHuman, fmt.Sprintf("skim confidence %.2f < %.2f", d.Confidence, th.Skim))
		}
	}
	if truncated && d.Bucket == BucketNone {
		d.escalate(BucketSkim, "diff truncated, model did not see the whole change")
	}
	return d
}

// unitPrompt is unitDiff plus the IDs of the other units.
func unitPrompt(u *Unit, maxChars int) (string, bool) {
	s, truncated := unitDiff(u, maxChars)
	s += relatedLine(u.Related)
	return s, truncated
}

func relatedLine(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return "\nOther units changed in the same PR (not shown): " + strings.Join(ids, ", ") + "\n"
}

// unitDiff is the unit's file, declaration and diff.
func unitDiff(u *Unit, maxChars int) (string, bool) {
	diff := u.Diff()
	truncated := false
	if maxChars > 0 && len(diff) > maxChars {
		diff = diff[:maxChars] + "\n... [truncated]"
		truncated = true
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "File: %s (%s)\n", u.File, u.Status)
	if u.Symbol != "" {
		fmt.Fprintf(&sb, "Declaration: %s\n", u.Symbol)
	}
	sb.WriteString("Diff:\n```diff\n")
	sb.WriteString(diff)
	sb.WriteString("\n```\n")
	return sb.String(), truncated
}

// Output token caps. For reasoning models they cover reasoning as well as
// the tool call, so they leave room for both.
const (
	classifyMaxTokens = 4096
	reviewMaxTokens   = 8192
)

// PromptVersion changes whenever the classify or analyze prompts do, so
// cached results from older prompts aren't reused.
const PromptVersion = "13"

// bucketRules defines the buckets. The classifier and the reviewer place
// units by the same rules.
const bucketRules = `Buckets are defined by consequence, not size:
- "human": the change can alter runtime behavior in a way a reviewer must judge. Logic, error handling, retries/timeouts, concurrency, public API or wire formats, security, persistence, resource limits, anything touching money or customer data. A one-line change to a retry count is "human".
- "skim": behavior changes are low-risk and a short written summary is enough for the reviewer. Logging text, metrics names, test-only changes, internal refactors with an obvious equivalence, new code behind an unused path.
- "none": the change cannot alter behavior. You MUST name the concrete reason (comment-only, import reordering, pure rename of an unexported identifier with all uses updated, dead code removal with no references). "Looks trivial" is not a reason.

Adding, removing or retagging struct fields is "human": it can change JSON/proto/wire output and what consumers receive.

Test files (*_test.go) cannot affect production behavior. New tests are "skim". Deleted tests, removed assertions, or expectations changed to match new behavior are "human": they can hide a regression. Comment-only or formatting-only edits in tests are "none".

When unsure, pick the higher bucket. Report confidence as the probability that your bucket is correct.`

// classifySystem is used only without a reviewer; with one, the review
// call places the unit (see analyzeSystem).
const classifySystem = `You triage pull-request changes for a Go/Kubernetes codebase. For each change unit decide who needs to look at it.

` + bucketRules + `

You only see one unit. Never argue that something is unused or unreferenced: its uses may be in the other units of the PR, which are listed after the diff.` + untrustedData

var triageTool = llm.ToolDefinition{
	Name:        "submit_triage",
	Description: "Submit the triage decision for this change unit.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"bucket": map[string]any{"type": "string", "enum": []string{"human", "skim", "none"}},
			"change_kind": map[string]any{"type": "string", "enum": []string{
				"behavior", "refactor", "rename", "config", "test", "docs", "generated", "format",
			}},
			"risk_signals": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "Only things that make this change risky, e.g. 'error handling changed', 'concurrency', 'public API'. " +
					"Not descriptive tags: never 'test-only', 'comment-only', 'no runtime effect'. Must be empty when bucket is none.",
			},
			"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"headline":   map[string]any{"type": "string", "description": "What changed, at most 12 words, e.g. 'Retry loop now tries 10 times with 100ms sleeps'. No 'This change...'."},
			"reason":     map[string]any{"type": "string", "description": "One short sentence (under 25 words) on why this bucket. For 'none', why behavior cannot change."},
		},
		"required": []string{"bucket", "change_kind", "headline", "confidence", "reason"},
	},
}

const classifyBatchNote = `

This call holds several units of the PR, each under its own "## Unit U<n>" heading. Decide each one on its own merits, as if it were the only one shown: a harmless unit next to a risky one is still harmless, and the other way round. Submit exactly one decision per unit, with its label (U1, U2, ...) in "unit".`

var triageBatchTool = func() llm.ToolDefinition {
	props := map[string]any{"unit": map[string]any{"type": "string", "description": "The unit's label, e.g. U1."}}
	for k, v := range triageTool.InputSchema["properties"].(map[string]any) {
		props[k] = v
	}
	required := append([]string{"unit"}, triageTool.InputSchema["required"].([]string)...)
	return llm.ToolDefinition{
		Name:        "submit_triage_batch",
		Description: "Submit the triage decision for every change unit in this call.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"decisions": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "object", "properties": props, "required": required},
				},
			},
			"required": []string{"decisions"},
		},
	}
}()

// LLMClassifier classifies with a tool-calling chat model.
type LLMClassifier struct {
	LLM    llm.LLMTool
	Policy Policy
}

func (c *LLMClassifier) source() string { return c.LLM.Name() + "/" + c.LLM.ModelID() }

func (c *LLMClassifier) Classify(ctx context.Context, u *Unit) Decision {
	prompt, truncated := unitPrompt(u, c.Policy.MaxUnitChars)
	args, _, err := llm.CallTool(ctx, c.LLM, []llm.ChatMessage{
		{Role: "system", Content: classifySystem},
		{Role: "user", Content: prompt},
	}, triageTool, classifyMaxTokens)
	if err != nil {
		return failed(c.source(), err)
	}
	d, err := decodeDecision(args)
	if err != nil {
		return failed(c.source(), err)
	}
	d.Source = c.source()
	return applyThresholds(d, c.Policy.Thresholds, truncated)
}

// ClassifyBatch classifies units in one call. Units the answer leaves out
// or gets wrong, and every unit when the call fails, are classified again
// one by one, so a bad batch costs time but never a decision.
func (c *LLMClassifier) ClassifyBatch(ctx context.Context, units []*Unit) []Decision {
	out := make([]Decision, len(units))
	if len(units) == 1 {
		out[0] = c.Classify(ctx, units[0])
		return out
	}
	in := map[string]bool{}
	for _, u := range units {
		in[u.ID] = true
	}
	var sb strings.Builder
	var others []string
	seen := map[string]bool{}
	truncated := make([]bool, len(units))
	for i, u := range units {
		s, t := unitDiff(u, c.Policy.MaxUnitChars)
		truncated[i] = t
		fmt.Fprintf(&sb, "## Unit U%d\n%s\n", i+1, s)
		for _, id := range u.Related {
			if !in[id] && !seen[id] && len(others) < maxRelated {
				seen[id] = true
				others = append(others, id)
			}
		}
	}
	sb.WriteString(relatedLine(others))
	done := make([]bool, len(units))
	args, _, err := llm.CallTool(ctx, c.LLM, []llm.ChatMessage{
		{Role: "system", Content: classifySystem + classifyBatchNote},
		{Role: "user", Content: sb.String()},
	}, triageBatchTool, classifyMaxTokens+1024*int32(len(units)))
	if err == nil {
		list, _ := args["decisions"].([]any)
		for _, x := range list {
			m, _ := x.(map[string]any)
			label, _ := m["unit"].(string)
			i, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(label), "U"))
			if err != nil || i < 1 || i > len(units) || done[i-1] {
				continue
			}
			d, err := decodeDecision(m)
			if err != nil {
				continue
			}
			d.Source = c.source()
			out[i-1], done[i-1] = applyThresholds(d, c.Policy.Thresholds, truncated[i-1]), true
		}
	}
	// One at a time: the batch holds one concurrency slot, so the retries
	// must not multiply the calls in flight.
	for i, u := range units {
		if !done[i] {
			out[i] = c.Classify(ctx, u)
		}
	}
	return out
}

// decodeDecision validates tool arguments in Go; providers (OpenAI in
// particular) do not reliably enforce the schema.
func decodeDecision(args map[string]any) (Decision, error) {
	var d Decision
	b, _ := args["bucket"].(string)
	d.Bucket = Bucket(strings.ToLower(strings.TrimSpace(b)))
	if !d.Bucket.Valid() {
		return d, fmt.Errorf("invalid bucket %q", b)
	}
	conf, ok := args["confidence"].(float64)
	if !ok || conf < 0 || conf > 1 {
		return d, fmt.Errorf("invalid confidence %v", args["confidence"])
	}
	d.Confidence = conf
	d.ChangeKind, _ = args["change_kind"].(string)
	d.Reason, _ = args["reason"].(string)
	d.Headline, _ = args["headline"].(string)
	if d.Bucket == BucketNone && strings.TrimSpace(d.Reason) == "" {
		return d, fmt.Errorf("bucket none without a reason")
	}
	if rs, ok := args["risk_signals"].([]any); ok {
		for _, r := range rs {
			if s, ok := r.(string); ok && s != "" {
				d.RiskSignals = append(d.RiskSignals, s)
			}
		}
	}
	// A model that lists risk signals but says "none" is contradicting itself.
	if d.Bucket == BucketNone && len(d.RiskSignals) > 0 {
		d.escalate(BucketSkim, "none with risk signals")
	}
	return d, nil
}

// JevClassifier uses OpenJev's calibrated probabilities. It answers from
// the first token with no reasoning, so it only keeps units where it is
// very sure; everything else goes to Fallback (or "human" if nil).
type JevClassifier struct {
	Jev      *llm.OpenJev
	Policy   Policy
	Fallback Classifier
	// Accept is the probability OpenJev's top bucket needs to be final.
	Accept map[Bucket]float64
}

func DefaultJevAccept() map[Bucket]float64 {
	return map[Bucket]float64{BucketHuman: 0.8, BucketSkim: 0.9, BucketNone: 0.97}
}

var jevQuestions = map[string]llm.JevQuestion{
	"bucket": {
		Type:         "choice",
		Instructions: "Who must review this code change? Pick by consequence, not size.",
		Criteria: map[string]string{
			"human": "can change runtime behavior: logic, errors, retries, concurrency, API, security, data",
			"skim":  "low-risk change: logging, tests, docs, obvious refactor",
			"none":  "cannot change behavior: comments, formatting, import order",
		},
	},
	"behavior": {
		Type:         "noul",
		Instructions: "Could this change alter what the program does at runtime?",
	},
}

func (c *JevClassifier) fallback(ctx context.Context, u *Unit, why string) Decision {
	if c.Fallback == nil {
		return Decision{Bucket: BucketHuman, Source: "openjev", Reason: why}
	}
	d := c.Fallback.Classify(ctx, u)
	d.Escalated = append([]string{"openjev deferred: " + why}, d.Escalated...)
	return d
}

func (c *JevClassifier) Classify(ctx context.Context, u *Unit) Decision {
	prompt, truncated := unitPrompt(u, c.Policy.MaxUnitChars)
	resp, err := c.Jev.Decide(ctx, prompt, jevQuestions)
	if err != nil {
		d := c.fallback(ctx, u, err.Error())
		d.Failed = d.Failed || c.Fallback == nil
		return d
	}
	a, ok := resp.Answers["bucket"]
	bucket := Bucket(a.Choice)
	if !ok || !bucket.Valid() {
		d := c.fallback(ctx, u, fmt.Sprintf("invalid answer %q", a.Choice))
		d.Failed = d.Failed || c.Fallback == nil
		return d
	}
	p := a.Probabilities[a.Choice]
	if p == 0 {
		p = a.Confidence
	}
	if p < c.Accept[bucket] {
		return c.fallback(ctx, u, fmt.Sprintf("p(%s)=%.2f < %.2f", bucket, p, c.Accept[bucket]))
	}
	d := Decision{Bucket: bucket, Confidence: p, Source: "openjev", Reason: fmt.Sprintf("p(%s)=%.2f", bucket, p)}
	if b := resp.Answers["behavior"].Noul; b != nil {
		d.Reason += fmt.Sprintf(", p(behavior change)=%.2f", *b)
		// Cross-check: "none" also requires a confident "no behavior change".
		if bucket == BucketNone && *b > 1-c.Accept[BucketNone] {
			return c.fallback(ctx, u, fmt.Sprintf("none but p(behavior change)=%.2f", *b))
		}
	}
	if truncated && bucket != BucketHuman {
		return c.fallback(ctx, u, "diff truncated")
	}
	return d
}
