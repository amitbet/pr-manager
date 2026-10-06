package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// Call ignores ToolChoiceRequired and ToolChoiceAny: Ollama has no forced
// tool choice, so CallTool fails when the model answers in text and the
// caller escalates, and the tool loop asks again.
func (o *OllamaLLM) Call(ctx context.Context, req LLMRequest) (*LLMResponse, error) {
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		for _, r := range m.ToolResults {
			msgs = append(msgs, map[string]any{"role": "tool", "tool_name": r.Name, "content": orNone(r.Content)})
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				calls = append(calls, map[string]any{"function": map[string]any{"name": c.Name, "arguments": argsOrEmpty(c.Arguments)}})
			}
			msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content, "tool_calls": calls})
			continue
		}
		if m.Content != "" || len(m.ToolResults) == 0 {
			msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
		}
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

// ollamaLarge is a window, in tokens, too large for a prompt to fill.
const ollamaLarge = 128_000

// ContextTokens is the context window of the model l serves, for callers
// that size a long prompt to it: 0 when it is too large to matter, as on
// every provider but Ollama, whose local models run with the window the
// server is set to and silently drop what doesn't fit.
func ContextTokens(ctx context.Context, l LLMTool) int {
	o, ok := l.(*OllamaLLM)
	if !ok {
		return 0
	}
	n := o.contextTokens(ctx)
	if n >= ollamaLarge {
		return 0
	}
	return n
}

// contextTokens is the window of the loaded model (/api/ps), loading it
// first, as the first call would; a cloud model's is its model's own.
func (o *OllamaLLM) contextTokens(ctx context.Context) int {
	if n := o.loadedContext(ctx); n > 0 {
		return n
	}
	if remote, n := o.modelInfo(ctx); remote {
		return n
	}
	lctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_ = o.api(lctx, http.MethodPost, "/api/generate", map[string]any{"model": o.ModelID()}, nil)
	if n := o.loadedContext(ctx); n > 0 {
		return n
	}
	return 8192
}

func (o *OllamaLLM) loadedContext(ctx context.Context) int {
	var ps struct {
		Models []struct {
			Name          string `json:"name"`
			ContextLength int    `json:"context_length"`
		} `json:"models"`
	}
	if o.api(ctx, http.MethodGet, "/api/ps", nil, &ps) != nil {
		return 0
	}
	for _, m := range ps.Models {
		if sameOllamaModel(m.Name, o.ModelID()) {
			return m.ContextLength
		}
	}
	return 0
}

// modelInfo is whether the model runs on Ollama's cloud, and its window.
func (o *OllamaLLM) modelInfo(ctx context.Context) (remote bool, tokens int) {
	var tags struct {
		Models []struct {
			Name       string `json:"name"`
			RemoteHost string `json:"remote_host"`
		} `json:"models"`
	}
	if o.api(ctx, http.MethodGet, "/api/tags", nil, &tags) != nil {
		return false, 0
	}
	for _, m := range tags.Models {
		if sameOllamaModel(m.Name, o.ModelID()) {
			remote = m.RemoteHost != ""
		}
	}
	var show struct {
		ModelInfo map[string]any `json:"model_info"`
	}
	if o.api(ctx, http.MethodPost, "/api/show", map[string]any{"model": o.ModelID()}, &show) == nil {
		for k, v := range show.ModelInfo {
			if f, ok := v.(float64); ok && strings.HasSuffix(k, ".context_length") {
				tokens = int(f)
			}
		}
	}
	return remote, tokens
}

func sameOllamaModel(a, b string) bool {
	if !strings.Contains(a, ":") {
		a += ":latest"
	}
	if !strings.Contains(b, ":") {
		b += ":latest"
	}
	return a == b
}

// api calls the Ollama API once, without retries, decoding into out.
// Loading a model (/api/generate) takes the caller's time; the rest 5s.
func (o *OllamaLLM) api(ctx context.Context, method, path string, in, out any) error {
	if path != "/api/generate" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.baseURL()+path, body)
	if err != nil {
		return err
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama %s: status %d", path, resp.StatusCode)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
