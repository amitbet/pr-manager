package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/internal/proc"
)

// Model is one entry in a provider's model list.
type Model struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

// Catalog is what this machine can run for one provider: whether it is
// usable at all, and its models. Like t3code, Codex's list comes live from
// the CLI (codex app-server model/list), Claude Code's from a fixed list
// (the CLI has no list command), and the API providers and Ollama from their
// model endpoints, so the list matches what the account can call.
type Catalog struct {
	Provider  string  `json:"provider"`
	Available bool    `json:"available"`
	Reason    string  `json:"reason"` // how it is authenticated, or why it is unavailable
	Models    []Model `json:"models,omitempty"`
	Live      bool    `json:"live"` // Models came from the provider, not a fallback list
}

// claudeCodeModels: the Claude Code CLI takes these ids or its aliases.
var claudeCodeModels = []Model{
	{ID: "claude-fable-5-1", Label: "Claude Fable 5.1"},
	{ID: "claude-opus-5-5", Label: "Claude Opus 5.5"},
	{ID: "claude-opus-5", Label: "Claude Opus 5"},
	{ID: "claude-sonnet-5-5", Label: "Claude Sonnet 5.5"},
	{ID: "claude-sonnet-5", Label: "Claude Sonnet 5"},
	{ID: "claude-haiku-4-5", Label: "Claude Haiku 4.5"},
}

// Fallback lists when a provider is usable but its list can't be read.
var fallbackModels = map[string][]Model{
	"codex": {{ID: "gpt-6-astra"}, {ID: "gpt-6-sol"}, {ID: "gpt-6-luna"}, {ID: "gpt-5.6-sol"}, {ID: "gpt-5.6-terra"}, {ID: "gpt-5.6-luna"}},
	ClaudeAPI: {{ID: AnthropicClaudeOpus55, Label: "Claude Opus 5.5"}, {ID: AnthropicClaudeSonnet5, Label: "Claude Sonnet 5"},
		{ID: AnthropicClaudeHaiku45, Label: "Claude Haiku 4.5"}},
	OpenAIAPI: {{ID: OpenAIGPT6Sol}, {ID: OpenAIGPT56Sol}, {ID: OpenAIGPT54}, {ID: OpenAIGPT54Mini}, {ID: OpenAIGPT54Nano}},
}

// Catalogs probes every provider in parallel. Results are cached for
// cacheFor; refresh forces a new probe.
func Catalogs(ctx context.Context, openjevURL string, refresh bool) []Catalog {
	catMu.Lock()
	defer catMu.Unlock()
	if !refresh && catCache != nil && time.Since(catAt) < cacheFor {
		return catCache
	}
	if refresh {
		forgetSubscriptions()
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	probes := []func(context.Context) Catalog{
		probeCodex, probeClaudeCode, probeAnthropic, probeOpenAI,
		probeBedrock, probeVertex, probeFoundry, probeAzureOpenAI, probeOllama,
		func(ctx context.Context) Catalog { return probeOpenJev(ctx, openjevURL) },
	}
	out := make([]Catalog, len(probes))
	var wg sync.WaitGroup
	for i, p := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = p(ctx)
		}()
	}
	wg.Wait()
	catCache, catAt = out, time.Now()
	return out
}

var (
	catMu    sync.Mutex
	catCache []Catalog
	catAt    time.Time
)

const cacheFor = 5 * time.Minute

func probeCodex(ctx context.Context) Catalog {
	c := Catalog{Provider: "codex"}
	if _, err := exec.LookPath("codex"); err != nil {
		c.Reason = "codex CLI not installed"
		return c
	}
	if !HasSubscription("codex") {
		c.Reason = "codex is not logged in with a ChatGPT plan (codex login)"
		return c
	}
	c.Available, c.Reason = true, "ChatGPT subscription via the codex CLI"
	models, err := codexModelList(ctx)
	if err != nil || len(models) == 0 {
		c.Models = fallbackModels["codex"]
		if err != nil {
			c.Reason += " (model list unavailable: " + err.Error() + ")"
		}
		return c
	}
	c.Models, c.Live = models, true
	return c
}

// codexModelList asks `codex app-server` for model/list over JSON-RPC
// (stdio, one JSON object per line), the way t3code does.
func codexModelList(ctx context.Context) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := proc.CommandContext(ctx, "codex", "app-server")
	ownGroup(cmd)
	cmd.Env = cliEnv("OPENAI_API_KEY", "CODEX_API_KEY") // list what the subscription can run
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	send := func(v any) error {
		b, _ := json.Marshal(v)
		_, err := stdin.Write(append(b, '\n'))
		return err
	}
	if err := send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "pr-manager", "version": "0.1"}}}); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var models []Model
	var cursor string
	next := 2
	for sc.Scan() {
		var msg struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.ID == nil {
			continue // notifications
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("codex app-server: %s", msg.Error.Message)
		}
		if *msg.ID == 1 {
			if err := send(map[string]any{"method": "initialized"}); err != nil {
				return nil, err
			}
		} else {
			var page struct {
				Data []struct {
					ID          string `json:"id"`
					DisplayName string `json:"displayName"`
					Hidden      bool   `json:"hidden"`
				} `json:"data"`
				NextCursor *string `json:"nextCursor"`
			}
			if err := json.Unmarshal(msg.Result, &page); err != nil {
				return nil, err
			}
			for _, d := range page.Data {
				// Models from other providers configured in codex profiles
				// (Ollama tags like "kimi-k3:cloud") only run under that
				// profile, not with codex exec -m.
				if d.Hidden || strings.Contains(d.ID, ":") {
					continue
				}
				label := d.DisplayName
				if label == d.ID {
					label = ""
				}
				models = append(models, Model{ID: d.ID, Label: label})
			}
			if page.NextCursor == nil || *page.NextCursor == "" || *page.NextCursor == cursor {
				return models, nil
			}
			cursor = *page.NextCursor
		}
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := send(map[string]any{"id": next, "method": "model/list", "params": params}); err != nil {
			return nil, err
		}
		next++
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("timed out")
	}
	return nil, fmt.Errorf("codex app-server exited: %v", sc.Err())
}

func probeClaudeCode(ctx context.Context) Catalog {
	c := Catalog{Provider: "claude-code"}
	if _, err := exec.LookPath("claude"); err != nil {
		c.Reason = "claude CLI not installed"
		return c
	}
	if !HasSubscription("claude-code") {
		c.Reason = "claude is not logged in with a Claude plan (claude auth login)"
		return c
	}
	c.Available, c.Reason, c.Models = true, "Claude subscription via the claude CLI", claudeCodeModels
	return c
}

func probeAnthropic(ctx context.Context) Catalog {
	c := Catalog{Provider: ClaudeAPI}
	a := &AnthropicLLM{}
	key := a.apiKey()
	if key == "" && a.authToken() == "" {
		c.Reason = "ANTHROPIC_API_KEY is not set"
		return c
	}
	c.Available, c.Reason = true, "ANTHROPIC_API_KEY"
	headers := map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}
	if key == "" {
		c.Reason, headers = "ANTHROPIC_AUTH_TOKEN", map[string]string{"Authorization": "Bearer " + a.authToken(), "anthropic-version": "2023-06-01"}
	}
	if a.baseURL() != "https://api.anthropic.com" {
		c.Reason += " via " + a.baseURL()
	}
	var resp struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	err := getJSON(ctx, a.baseURL()+"/v1/models?limit=100", headers, &resp)
	for _, d := range resp.Data {
		c.Models = append(c.Models, Model{ID: d.ID, Label: d.DisplayName})
	}
	return withFallback(c, err)
}

// openAIChat keeps the text-generation models; the endpoint also lists
// embeddings, audio, image and moderation models.
var (
	openAIChat    = regexp.MustCompile(`^(gpt-|o\d|chatgpt-)`)
	openAINotChat = regexp.MustCompile(`(audio|realtime|tts|transcribe|image|search|embedding|moderation|instruct|codex)`)
)

func probeOpenAI(ctx context.Context) Catalog {
	c := Catalog{Provider: OpenAIAPI}
	o := &OpenAILLM{}
	key := o.apiKey()
	if key == "" {
		c.Reason = "OPENAI_API_KEY is not set"
		return c
	}
	c.Available, c.Reason = true, "OPENAI_API_KEY"
	if o.baseURL() != "https://api.openai.com" {
		c.Reason += " via " + o.baseURL()
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	err := getJSON(ctx, o.baseURL()+"/v1/models", map[string]string{"Authorization": "Bearer " + key}, &resp)
	for _, d := range resp.Data {
		if openAIChat.MatchString(d.ID) && !openAINotChat.MatchString(d.ID) {
			c.Models = append(c.Models, Model{ID: d.ID})
		}
	}
	// Newest-looking first: longer version strings sort after shorter ones otherwise.
	sort.Slice(c.Models, func(i, j int) bool { return c.Models[i].ID > c.Models[j].ID })
	return withFallback(c, err)
}

func probeOllama(ctx context.Context) Catalog {
	c := Catalog{Provider: "ollama"}
	o := &OllamaLLM{}
	var resp struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := getJSON(ctx, o.baseURL()+"/api/tags", nil, &resp); err != nil {
		c.Reason = "no Ollama server at " + o.baseURL()
		return c
	}
	c.Available, c.Reason, c.Live = true, "Ollama at "+o.baseURL(), true
	for _, m := range resp.Models {
		c.Models = append(c.Models, Model{ID: m.Name})
	}
	if len(c.Models) == 0 {
		c.Available, c.Reason = false, "Ollama is running but has no models (ollama pull)"
	}
	return c
}

// probeOpenJev: OpenJev answers classify questions only and has one model.
func probeOpenJev(ctx context.Context, url string) Catalog {
	j := &OpenJev{BaseURL: url}
	c := Catalog{Provider: "openjev"}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, j.baseURL()+"/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.Reason = "no OpenJev server at " + j.baseURL()
		return c
	}
	resp.Body.Close()
	c.Available, c.Reason = true, "OpenJev at "+j.baseURL()
	return c
}

func withFallback(c Catalog, err error) Catalog {
	if err == nil && len(c.Models) > 0 {
		c.Live = true
		return c
	}
	c.Models = fallbackModels[c.Provider]
	if err != nil {
		c.Reason += " (model list unavailable: " + err.Error() + ")"
	}
	return c
}

func getJSON(ctx context.Context, url string, headers map[string]string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, val := range headers {
		req.Header.Set(k, val)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return json.Unmarshal(b, v)
}
