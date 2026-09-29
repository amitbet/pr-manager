package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fastRetry swaps in a policy whose sleeps are recorded, not slept.
func fastRetry(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	old := httpRetry
	httpRetry.Sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	t.Cleanup(func() { httpRetry = old })
	return &waits
}

// statusServer answers with codes in turn, then 200 with ok.
func statusServer(t *testing.T, ok string, header http.Header, codes ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(hits.Add(1)) - 1
		if b, _ := io.ReadAll(r.Body); len(b) == 0 {
			t.Errorf("attempt %d: empty body", n)
		}
		if n < len(codes) {
			for k, v := range header {
				w.Header()[k] = v
			}
			w.WriteHeader(codes[n])
			_, _ = fmt.Fprintf(w, `{"error":"status %d"}`, codes[n])
			return
		}
		_, _ = io.WriteString(w, ok)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

const (
	anthropicOK = `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`
	openaiOK    = `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`
)

var hello = LLMRequest{Messages: []ChatMessage{{Role: "user", Content: "hello"}}}

func TestRetryOverloadedThenOK(t *testing.T) {
	waits := fastRetry(t)
	srv, hits := statusServer(t, anthropicOK, nil, 529, 503)
	a := &AnthropicLLM{APIKey: "k", BaseURL: srv.URL}
	resp, err := a.Call(context.Background(), hello)
	if err != nil || resp.Text != "hi" {
		t.Fatalf("call: %v %v", resp, err)
	}
	if hits.Load() != 3 || len(*waits) != 2 {
		t.Errorf("hits %d waits %v, want 3 hits after 2 waits", hits.Load(), *waits)
	}
	for i, d := range *waits {
		if base := time.Second << i; d < base/2 || d > base*3/2 {
			t.Errorf("wait %d = %v, want about %v", i, d, base)
		}
	}
}

func TestRetryNotOnClientError(t *testing.T) {
	fastRetry(t)
	for _, code := range []int{400, 401, 403, 404, 422} {
		srv, hits := statusServer(t, openaiOK, nil, code)
		o := &OpenAILLM{APIKey: "k", BaseURL: srv.URL, Model: "gpt-4o"}
		_, err := o.Call(context.Background(), hello)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprint(code)) {
			t.Errorf("%d: err %v", code, err)
		}
		if hits.Load() != 1 {
			t.Errorf("%d: %d attempts, want 1", code, hits.Load())
		}
	}
}

func TestRetryGivesUp(t *testing.T) {
	waits := fastRetry(t)
	srv, hits := statusServer(t, `{}`, nil, 500, 500, 500, 500, 500, 500)
	o := &OllamaLLM{BaseURL: srv.URL, Model: "m"}
	if _, err := o.Call(context.Background(), hello); err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("err %v, want the last 500", err)
	}
	if want := httpRetry.MaxRetries + 1; int(hits.Load()) != want || len(*waits) != want-1 {
		t.Errorf("hits %d waits %d, want %d attempts", hits.Load(), len(*waits), want)
	}
}

func TestRetryAfterHeaders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{"seconds", http.Header{"Retry-After": {"7"}}, 7 * time.Second},
		{"ms", http.Header{"Retry-After-Ms": {"250"}, "Retry-After": {"1"}}, 250 * time.Millisecond},
		{"openai reset", http.Header{"X-Ratelimit-Reset-Requests": {"2s"}, "X-Ratelimit-Reset-Tokens": {"6m0s"}}, time.Minute},
		{"capped", http.Header{"Retry-After": {"3600"}}, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waits := fastRetry(t)
			httpRetry.MaxWait = time.Minute
			srv, hits := statusServer(t, openaiOK, tc.header, 429)
			o := &OpenAILLM{APIKey: "k", BaseURL: srv.URL, Model: "gpt-4o"}
			if _, err := o.Call(context.Background(), hello); err != nil {
				t.Fatal(err)
			}
			if hits.Load() != 2 || len(*waits) != 1 || (*waits)[0] != tc.want {
				t.Errorf("hits %d waits %v, want one wait of %v", hits.Load(), *waits, tc.want)
			}
		})
	}
	if d, ok := serverWait(http.Header{"Retry-After": {"Wed, 30 Sep 2026 10:00:05 GMT"}}, 503,
		time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)); !ok || d != 5*time.Second {
		t.Errorf("http-date Retry-After: %v %v", d, ok)
	}
}

func TestRetryStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := httpRetry
	t.Cleanup(func() { httpRetry = old })
	httpRetry.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return sleepCtx(ctx, time.Hour)
	}
	srv, hits := statusServer(t, anthropicOK, nil, 503, 503, 503)
	a := &AnthropicLLM{APIKey: "k", BaseURL: srv.URL}
	start := time.Now()
	_, err := a.Call(ctx, hello)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err %v, want context.Canceled", err)
	}
	if hits.Load() != 1 || time.Since(start) > 5*time.Second {
		t.Errorf("%d attempts in %v, want 1 and no wait", hits.Load(), time.Since(start))
	}
}

// A dropped connection is retried; the request is rebuilt, so auth (a SigV4
// signature with its timestamp) is redone per attempt.
func TestRetryDroppedConnectionReauths(t *testing.T) {
	fastRetry(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		_, _ = io.WriteString(w, anthropicOK)
	}))
	defer srv.Close()
	p := &countingPlatform{url: srv.URL + "/v1/messages"}
	a := &AnthropicLLM{Platform: p, Model: "m"}
	if _, err := a.Call(context.Background(), hello); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 || p.auths.Load() != 2 {
		t.Errorf("hits %d auths %d, want 2 each", hits.Load(), p.auths.Load())
	}
}

type countingPlatform struct {
	url   string
	auths atomic.Int32
}

func (p *countingPlatform) ID() string                 { return "test" }
func (p *countingPlatform) DefaultModel() string       { return "m" }
func (p *countingPlatform) URL(string) (string, error) { return p.url, nil }
func (p *countingPlatform) Body(map[string]any)        {}
func (p *countingPlatform) Auth(_ context.Context, req *http.Request, _ []byte) error {
	req.Header.Set("X-Attempt", fmt.Sprint(p.auths.Add(1)))
	return nil
}

// The tool_choice 400 still triggers the one-off resend with auto, after a
// transient 529 on the first attempt.
func TestRetryKeepsForcedToolFallback(t *testing.T) {
	fastRetry(t)
	noForcedTool.Delete("retry-deployment")
	var choices []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch {
		case len(choices) == 0:
			w.WriteHeader(529)
		case strings.Contains(string(b), `"type":"tool"`):
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"message":"tool_choice not supported"}}`)
		default:
			_, _ = io.WriteString(w, `{"content":[{"type":"tool_use","id":"t1","name":"submit","input":{"ok":true}}],"stop_reason":"tool_use"}`)
		}
		choices = append(choices, fmt.Sprint(strings.Contains(string(b), `"type":"tool"`)))
	}))
	defer srv.Close()
	a := &AnthropicLLM{APIKey: "k", BaseURL: srv.URL, Model: "retry-deployment"}
	args, _, err := CallTool(context.Background(), a, []ChatMessage{{Role: "user", Content: "go"}}, ToolDefinition{Name: "submit"}, 100)
	if err != nil || args["ok"] != true {
		t.Fatalf("call: %v %v", args, err)
	}
	if got := strings.Join(choices, ","); got != "true,true,false" {
		t.Errorf("forced per attempt %s, want 529 forced, 400 forced, then auto", got)
	}
}
