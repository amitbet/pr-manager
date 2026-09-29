package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// AnthropicLLM calls the Messages API: the Claude API itself, a gateway in
// front of it (ANTHROPIC_BASE_URL), or Claude on a cloud platform.
type AnthropicLLM struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
	// Platform, if set, sends the request to Claude on Bedrock, Vertex AI
	// or Foundry, which serve the same body under their own URL and auth.
	Platform Platform
}

// Platform is a cloud that serves the Messages API.
type Platform interface {
	ID() string // provider id
	DefaultModel() string
	URL(model string) (string, error)
	// Body adjusts the first-party request body in place.
	Body(payload map[string]any)
	// Auth sets the auth (and version) headers; body is what is sent.
	Auth(ctx context.Context, req *http.Request, body []byte) error
}

func (a *AnthropicLLM) client() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	return &http.Client{Timeout: 300 * time.Second}
}

func (a *AnthropicLLM) apiKey() string {
	if strings.TrimSpace(a.APIKey) != "" {
		return a.APIKey
	}
	return os.Getenv("ANTHROPIC_API_KEY")
}

// authToken is a bearer token for a gateway that doesn't take x-api-key.
func (a *AnthropicLLM) authToken() string { return os.Getenv("ANTHROPIC_AUTH_TOKEN") }

func (a *AnthropicLLM) baseURL() string {
	if strings.TrimSpace(a.BaseURL) != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	if u := strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "https://api.anthropic.com"
}

func (a *AnthropicLLM) ModelID() string {
	if a.Model != "" {
		return a.Model
	}
	if a.Platform != nil {
		return a.Platform.DefaultModel()
	}
	return AnthropicClaudeHaiku45
}

func (a *AnthropicLLM) Name() string {
	if a.Platform != nil {
		return a.Platform.ID()
	}
	return ClaudeAPI
}

// noForcedTool: models that reject tool_choice "tool" (and "any") with a
// 400. They get tool_choice "auto" and an instruction to call the tool;
// CallTool still fails if the answer has no call. Deployment names on
// Foundry can hide the model, so a 400 about tool_choice also lands here.
var noForcedTool sync.Map // model id -> true

func forcedToolOK(model string) bool {
	if _, bad := noForcedTool.Load(model); bad {
		return false
	}
	m := strings.ToLower(model)
	for _, s := range []string{"opus-5-5", "fable-5-1", "mythos-5-1", "mythos-preview"} {
		if strings.Contains(m, s) {
			return false
		}
	}
	return true
}

func buildAnthropicMessages(messages []ChatMessage) ([]map[string]any, []string) {
	out := make([]map[string]any, 0, len(messages))
	var system []string
	for _, m := range messages {
		switch m.Role {
		case "system":
			if strings.TrimSpace(m.Content) != "" {
				system = append(system, m.Content)
			}
		case "user", "assistant":
			out = append(out, map[string]any{
				"role":    m.Role,
				"content": []map[string]any{{"type": "text", "text": m.Content}},
			})
		}
	}
	return out, system
}

func buildAnthropicTools(defs []ToolDefinition) []map[string]any {
	out := make([]map[string]any, 0, len(defs))
	for _, def := range defs {
		schema := def.InputSchema
		if len(schema) == 0 {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"name":         def.Name,
			"description":  def.Description,
			"input_schema": schema,
		})
	}
	return out
}

func (a *AnthropicLLM) buildPayload(req LLMRequest) map[string]any {
	messages, system := buildAnthropicMessages(req.Messages)
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 2048
	}
	payload := map[string]any{
		"model":      a.ModelID(),
		"messages":   messages,
		"max_tokens": maxTokens,
	}
	tools := buildAnthropicTools(req.Tools)
	forced := req.ToolChoice == ToolChoiceRequired && len(tools) > 0
	if forced && !forcedToolOK(a.ModelID()) {
		forced = false
		system = append(system, fmt.Sprintf("Answer by calling the %s tool exactly once. Do not answer in text.", req.Tools[0].Name))
	}
	if len(system) > 0 {
		// The system prompt is identical across every unit in a run, so a
		// cache breakpoint here bills it at ~10% after the first call.
		payload["system"] = []map[string]any{{
			"type":          "text",
			"text":          strings.Join(system, "\n\n"),
			"cache_control": map[string]any{"type": "ephemeral"},
		}}
	}
	if len(tools) > 0 {
		tools[len(tools)-1]["cache_control"] = map[string]any{"type": "ephemeral"}
		payload["tools"] = tools
		switch {
		case req.ToolChoice == ToolChoiceNone:
			payload["tool_choice"] = map[string]any{"type": "none"}
		case forced:
			payload["tool_choice"] = map[string]any{"type": "tool", "name": req.Tools[0].Name}
		default:
			payload["tool_choice"] = map[string]any{"type": "auto"}
		}
	}
	return payload
}

func (a *AnthropicLLM) Call(ctx context.Context, req LLMRequest) (*LLMResponse, error) {
	status, body, err := a.post(ctx, req)
	if err == nil && status == http.StatusBadRequest && req.ToolChoice == ToolChoiceRequired &&
		forcedToolOK(a.ModelID()) && bytes.Contains(body, []byte("tool_choice")) {
		noForcedTool.Store(a.ModelID(), true)
		status, body, err = a.post(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("%s API error: status %d, body: %s", a.Name(), status, string(body))
	}
	return parseAnthropicResponse(body)
}

// post sends one Messages request and returns the status and body.
func (a *AnthropicLLM) post(ctx context.Context, req LLMRequest) (int, []byte, error) {
	payload := a.buildPayload(req)
	url := a.baseURL() + "/v1/messages"
	if a.Platform != nil {
		a.Platform.Body(payload)
		var err error
		if url, err = a.Platform.URL(a.ModelID()); err != nil {
			return 0, nil, err
		}
	} else if strings.TrimSpace(a.apiKey()) == "" && a.authToken() == "" {
		return 0, nil, fmt.Errorf("missing ANTHROPIC_API_KEY")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	return doRetry(ctx, a.client(), func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if a.Platform != nil {
			if err := a.Platform.Auth(ctx, httpReq, b); err != nil {
				return nil, fmt.Errorf("%s auth: %w", a.Name(), err)
			}
		} else {
			httpReq.Header.Set("anthropic-version", "2023-06-01")
			if key := a.apiKey(); key != "" {
				httpReq.Header.Set("x-api-key", key)
			} else {
				httpReq.Header.Set("Authorization", "Bearer "+a.authToken())
			}
		}
		return httpReq, nil
	})
}

func parseAnthropicResponse(body []byte) (*LLMResponse, error) {
	var out struct {
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
		Usage      struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	var sb strings.Builder
	var calls []ToolCall
	for _, block := range out.Content {
		switch block["type"] {
		case "text":
			text, _ := block["text"].(string)
			sb.WriteString(text)
		case "tool_use":
			input, _ := block["input"].(map[string]any)
			calls = append(calls, ToolCall{
				Name:      fmt.Sprintf("%v", block["name"]),
				Arguments: input,
				CallID:    fmt.Sprintf("%v", block["id"]),
			})
		}
	}
	return &LLMResponse{
		Text:       sb.String(),
		ToolCalls:  calls,
		StopReason: mapAnthropicStopReason(out.StopReason),
		Usage: Usage{
			InputTokens:  out.Usage.InputTokens + out.Usage.CacheCreationInputTokens + out.Usage.CacheReadInputTokens,
			OutputTokens: out.Usage.OutputTokens,
		},
	}, nil
}

func mapAnthropicStopReason(s string) StopReason {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "end_turn", "pause_turn":
		return StopEndTurn
	case "max_tokens":
		return StopMaxTokens
	case "tool_use":
		return StopToolUse
	case "stop_sequence":
		return StopStopSequence
	case "refusal":
		return StopRefusal
	default:
		return StopUnknown
	}
}
