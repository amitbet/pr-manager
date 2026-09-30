package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serveJSON(t *testing.T, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestMalformedToolArgumentsFail(t *testing.T) {
	req := LLMRequest{Messages: []ChatMessage{{Role: "user", Content: "u"}}}
	cases := []struct {
		name string
		body string
		call func(url string) (*LLMResponse, error)
	}{
		{"openai chat", `{"choices":[{"message":{"tool_calls":[{"id":"1","function":{"name":"submit","arguments":"{\"a\":"}}]},"finish_reason":"tool_calls"}]}`,
			func(url string) (*LLMResponse, error) {
				return (&OpenAILLM{Model: OpenAIGPT54Mini, APIKey: "k", BaseURL: url}).Call(context.Background(), req)
			}},
		{"openai responses", `{"status":"completed","output":[{"type":"function_call","name":"submit","call_id":"1","arguments":"not json"}]}`,
			func(url string) (*LLMResponse, error) {
				return (&OpenAILLM{Model: OpenAIGPT56Sol, Effort: "medium", APIKey: "k", BaseURL: url}).Call(context.Background(), req)
			}},
		{"ollama object", `{"message":{"tool_calls":[{"function":{"name":"submit","arguments":[1,2]}}]},"done_reason":"stop"}`,
			func(url string) (*LLMResponse, error) {
				return (&OllamaLLM{BaseURL: url}).Call(context.Background(), req)
			}},
		{"ollama string", `{"message":{"tool_calls":[{"function":{"name":"submit","arguments":"{oops"}}]},"done_reason":"stop"}`,
			func(url string) (*LLMResponse, error) {
				return (&OllamaLLM{BaseURL: url}).Call(context.Background(), req)
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.call(serveJSON(t, c.body).URL)
			if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
				t.Fatalf("err = %v, want invalid JSON error", err)
			}
		})
	}
}

func TestValidToolArgumentsParse(t *testing.T) {
	req := LLMRequest{Messages: []ChatMessage{{Role: "user", Content: "u"}}}
	s := serveJSON(t, `{"message":{"tool_calls":[{"function":{"name":"submit","arguments":"{\"a\":1}"}}]},"done_reason":"stop"}`)
	r, err := (&OllamaLLM{BaseURL: s.URL}).Call(context.Background(), req)
	if err != nil || len(r.ToolCalls) != 1 || r.ToolCalls[0].Arguments["a"] != 1.0 {
		t.Fatalf("r=%+v err=%v", r, err)
	}
}
