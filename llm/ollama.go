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

type OllamaLLM struct {
	Model      string
	BaseURL    string
	HTTPClient *http.Client
}

func (o *OllamaLLM) client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Timeout: 300 * time.Second}
}

func (o *OllamaLLM) baseURL() string {
	if strings.TrimSpace(o.BaseURL) != "" {
		return strings.TrimRight(o.BaseURL, "/")
	}
	if env := strings.TrimSpace(os.Getenv("OLLAMA_BASE_URL")); env != "" {
		return strings.TrimRight(env, "/")
	}
	return "http://localhost:11434"
}

func (o *OllamaLLM) ModelID() string {
	if strings.TrimSpace(o.Model) == "" {
		return OllamaQwen35_9B
	}
	return o.Model
}

func (o *OllamaLLM) Name() string { return "ollama" }

// Call ignores ToolChoiceRequired beyond sending only the requested tool:
// Ollama has no forced tool choice, so CallTool fails when the model
// answers in text and the caller escalates.
func (o *OllamaLLM) Call(ctx context.Context, req LLMRequest) (*LLMResponse, error) {
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
	}
	payload := map[string]any{
		"model":    o.ModelID(),
		"messages": msgs,
		"stream":   false,
		"options":  map[string]any{"temperature": 0},
	}
	if req.MaxTokens > 0 {
		payload["options"].(map[string]any)["num_predict"] = req.MaxTokens
	}
	if len(req.Tools) > 0 && req.ToolChoice != ToolChoiceNone {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, def := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        def.Name,
					"description": def.Description,
					"parameters":  def.InputSchema,
				},
			})
		}
		payload["tools"] = tools
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	status, body, err := doRetry(ctx, o.client(), func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL()+"/api/chat", bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		return httpReq, nil
	})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("ollama API error: status %d, body: %s", status, string(body))
	}
	var out struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		DoneReason      string `json:"done_reason"`
		PromptEvalCount int    `json:"prompt_eval_count"`
		EvalCount       int    `json:"eval_count"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	r := &LLMResponse{
		Text:  out.Message.Content,
		Usage: Usage{InputTokens: out.PromptEvalCount, OutputTokens: out.EvalCount},
	}
	for _, tc := range out.Message.ToolCalls {
		args := map[string]any{}
		// Arguments arrive as an object, or as a JSON string on some models.
		if len(tc.Function.Arguments) > 0 && string(tc.Function.Arguments) != "null" {
			if err := json.Unmarshal(tc.Function.Arguments, &args); err != nil {
				var s string
				if json.Unmarshal(tc.Function.Arguments, &s) != nil {
					return nil, badToolArgs(tc.Function.Name, string(tc.Function.Arguments))
				}
				if strings.TrimSpace(s) != "" {
					if err := json.Unmarshal([]byte(s), &args); err != nil {
						return nil, badToolArgs(tc.Function.Name, s)
					}
				}
			}
		}
		r.ToolCalls = append(r.ToolCalls, ToolCall{Name: tc.Function.Name, Arguments: args})
	}
	switch {
	case len(r.ToolCalls) > 0:
		r.StopReason = StopToolUse
	case out.DoneReason == "length":
		r.StopReason = StopMaxTokens
	case out.DoneReason == "stop":
		r.StopReason = StopEndTurn
	}
	return r, nil
}
