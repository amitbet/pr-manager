package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// OpenAILLM talks to Chat Completions, or to the Responses API when a
// reasoning effort is set. BaseURL (or OPENAI_BASE_URL) can point at any
// OpenAI-compatible server, such as a LiteLLM or Portkey gateway.
type OpenAILLM struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
	// Provider and Auth are set for Azure OpenAI: its own provider id, and
	// an api-key or Entra ID header instead of the bearer API key.
	Provider string
	Auth     func(ctx context.Context, req *http.Request) error
	// Effort is the reasoning effort (none|minimal|low|medium|high|xhigh)
	// for reasoning models. Newer models (gpt-5.6, gpt-6) only accept
	// function tools with reasoning through /v1/responses, so any effort
	// other than "none" goes there; "" keeps Chat Completions with the
	// model's default.
	Effort string
}

func (o *OpenAILLM) client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Timeout: 300 * time.Second}
}

func (o *OpenAILLM) apiKey() string {
	if strings.TrimSpace(o.APIKey) != "" {
		return o.APIKey
	}
	return os.Getenv("OPENAI_API_KEY")
}

// baseURL is the root the /v1/... paths go under. OPENAI_BASE_URL is read
// the way the OpenAI SDKs read it, with /v1 included.
func (o *OpenAILLM) baseURL() string {
	if strings.TrimSpace(o.BaseURL) != "" {
		return strings.TrimRight(o.BaseURL, "/")
	}
	if u := strings.TrimRight(strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")), "/"); u != "" {
		return strings.TrimSuffix(u, "/v1")
	}
	return "https://api.openai.com"
}

func (o *OpenAILLM) ModelID() string {
	if o.Model == "" {
		return OpenAIGPT54Mini
	}
	return o.Model
}

func (o *OpenAILLM) Name() string {
	if o.Provider != "" {
		return o.Provider
	}
	return OpenAIAPI
}

// isGPT5 covers reasoning models, which reject temperature and max_tokens.
func isGPT5(model string) bool {
	return strings.Contains(model, "gpt-5") || strings.Contains(model, "gpt-6") ||
		strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4")
}

func (o *OpenAILLM) useResponses() bool {
	return isGPT5(o.ModelID()) && o.Effort != "" && o.Effort != "none"
}

func (o *OpenAILLM) buildPayload(req LLMRequest) map[string]any {
	model := o.ModelID()
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
	}
	payload := map[string]any{"model": model, "messages": msgs}
	if req.MaxTokens > 0 {
		if isGPT5(model) {
			payload["max_completion_tokens"] = req.MaxTokens
		} else {
			payload["max_tokens"] = req.MaxTokens
		}
	}
	if !isGPT5(model) {
		payload["temperature"] = 0.0
	}
	if o.Effort == "none" && isGPT5(model) {
		payload["reasoning_effort"] = "none"
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, def := range req.Tools {
			params := def.InputSchema
			if len(params) == 0 {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        def.Name,
					"description": def.Description,
					"parameters":  params,
				},
			})
		}
		payload["tools"] = tools
		switch req.ToolChoice {
		case ToolChoiceNone:
			payload["tool_choice"] = "none"
		case ToolChoiceRequired:
			payload["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": req.Tools[0].Name},
			}
		default:
			payload["tool_choice"] = "auto"
		}
	}
	return payload
}

func (o *OpenAILLM) post(ctx context.Context, path string, payload map[string]any) ([]byte, error) {
	if o.Auth == nil && o.apiKey() == "" {
		return nil, fmt.Errorf("missing OPENAI_API_KEY")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	status, body, err := doRetry(ctx, o.client(), func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL()+path, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if o.Auth != nil {
			if err := o.Auth(ctx, httpReq); err != nil {
				return nil, fmt.Errorf("%s auth: %w", o.Name(), err)
			}
		} else {
			httpReq.Header.Set("Authorization", "Bearer "+o.apiKey())
		}
		return httpReq, nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s request failed: %w", o.Name(), err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("%s API error: status %d, body: %s", o.Name(), status, string(body))
	}
	return body, nil
}

func (o *OpenAILLM) Call(ctx context.Context, req LLMRequest) (*LLMResponse, error) {
	if o.useResponses() {
		return o.callResponses(ctx, req)
	}
	body, err := o.post(ctx, "/v1/chat/completions", o.buildPayload(req))
	if err != nil {
		return nil, err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	r := &LLMResponse{Usage: Usage{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens}}
	if len(out.Choices) == 0 {
		return r, nil
	}
	c := out.Choices[0]
	r.Text = c.Message.Content
	r.StopReason = mapOpenAIFinishReason(c.FinishReason)
	for _, tc := range c.Message.ToolCalls {
		args := map[string]any{}
		if strings.TrimSpace(tc.Function.Arguments) != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				return nil, badToolArgs(tc.Function.Name, tc.Function.Arguments)
			}
		}
		r.ToolCalls = append(r.ToolCalls, ToolCall{Name: tc.Function.Name, Arguments: args, CallID: tc.ID})
	}
	return r, nil
}

// badToolArgs reports tool-call arguments that are not valid JSON, so callers
// take their failure path instead of acting on garbage input.
func badToolArgs(name, raw string) error {
	const max = 200
	if len(raw) > max {
		raw = raw[:max] + "..."
	}
	return fmt.Errorf("tool call %q arguments are not valid JSON: %s", name, raw)
}

func mapOpenAIFinishReason(s string) StopReason {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "stop":
		return StopEndTurn
	case "length":
		return StopMaxTokens
	case "tool_calls", "function_call":
		return StopToolUse
	case "content_filter":
		return StopRefusal
	default:
		return StopUnknown
	}
}

// buildResponsesPayload is the /v1/responses form of a request: system
// messages become input items, tools are flat, and max tokens covers
// reasoning plus the answer.
func (o *OpenAILLM) buildResponsesPayload(req LLMRequest) map[string]any {
	input := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		input = append(input, map[string]any{"role": m.Role, "content": m.Content})
	}
	payload := map[string]any{"model": o.ModelID(), "input": input, "reasoning": map[string]any{"effort": o.Effort}, "store": false}
	if req.MaxTokens > 0 {
		payload["max_output_tokens"] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, def := range req.Tools {
			params := def.InputSchema
			if len(params) == 0 {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"type": "function", "name": def.Name, "description": def.Description, "parameters": params})
		}
		payload["tools"] = tools
		switch req.ToolChoice {
		case ToolChoiceNone:
			payload["tool_choice"] = "none"
		case ToolChoiceRequired:
			payload["tool_choice"] = map[string]any{"type": "function", "name": req.Tools[0].Name}
		default:
			payload["tool_choice"] = "auto"
		}
	}
	return payload
}

func (o *OpenAILLM) callResponses(ctx context.Context, req LLMRequest) (*LLMResponse, error) {
	body, err := o.post(ctx, "/v1/responses", o.buildResponsesPayload(req))
	if err != nil {
		return nil, err
	}
	var out struct {
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			CallID    string `json:"call_id"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	r := &LLMResponse{Usage: Usage{InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens}, StopReason: StopEndTurn}
	if out.Status == "incomplete" {
		r.StopReason = StopUnknown
		if out.IncompleteDetails != nil && out.IncompleteDetails.Reason == "max_output_tokens" {
			r.StopReason = StopMaxTokens
		}
	}
	for _, item := range out.Output {
		switch item.Type {
		case "function_call":
			args := map[string]any{}
			if strings.TrimSpace(item.Arguments) != "" {
				if err := json.Unmarshal([]byte(item.Arguments), &args); err != nil {
					return nil, badToolArgs(item.Name, item.Arguments)
				}
			}
			r.ToolCalls = append(r.ToolCalls, ToolCall{Name: item.Name, Arguments: args, CallID: item.CallID})
			r.StopReason = StopToolUse
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" {
					r.Text += c.Text
				}
			}
		}
	}
	return r, nil
}
