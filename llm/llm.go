// Package llm holds the non-streaming Call path of the Anthropic, OpenAI and
// Ollama adapters, plus Bedrock (Messages API; Converse only for other
// models), Vertex AI, Foundry and Azure OpenAI (cloud.go).
//   - no Stream, images or embeddings
//   - ToolChoiceRequired forces a single named tool (structured output)
//   - token usage is returned on the response rather than stored on the
//     client, so one client can be shared across goroutines
package llm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/internal/activity"
)

// ChatMessage is a minimal role/content pair used across providers
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

type ToolCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	CallID    string         `json:"call_id,omitempty"`
}

type ToolChoiceType string

const (
	ToolChoiceAuto ToolChoiceType = "auto"
	ToolChoiceNone ToolChoiceType = "none"
	// ToolChoiceRequired forces the model to call the first tool in
	// LLMRequest.Tools. Used for structured output.
	ToolChoiceRequired ToolChoiceType = "required"
)

type LLMRequest struct {
	Messages   []ChatMessage    `json:"messages"`
	Tools      []ToolDefinition `json:"tools,omitempty"`
	ToolChoice ToolChoiceType   `json:"tool_choice,omitempty"`
	MaxTokens  int32            `json:"max_tokens,omitempty"`
	// Workspace, if set, lets providers that run an agent (the CLIs) read
	// files while answering. The API providers ignore it.
	Workspace *Workspace `json:"-"`
}

// Workspace is what a tool-using provider may read: Dir is its working
// directory (the repo at the reviewed revision), ReadDirs are extra
// read-only directories such as the Go module cache. With Edit it may
// also change files in Dir, and on Claude Code hand work to subagents;
// it still runs no commands.
//
// With Shell it may also run commands that read (rg, git log, git blame,
// ls, pipes), with nothing written: codex in its read-only sandbox, Claude
// Code in its OS sandbox with every write and the network denied, or,
// where that sandbox can't run (Windows), only a list of read-only
// commands. NoWrite are more directories it must not write to.
type Workspace struct {
	Dir      string
	ReadDirs []string
	Edit     bool
	Shell    bool
	NoWrite  []string
}

// SupportsWorkspace reports whether l can use LLMRequest.Workspace.
func SupportsWorkspace(l LLMTool) bool {
	switch l.(type) {
	case *CodexCLI, *ClaudeCodeCLI:
		return true
	}
	return false
}

// StopReason is the provider-agnostic reason a generation ended.
type StopReason string

const (
	StopUnknown      StopReason = ""
	StopEndTurn      StopReason = "end_turn"
	StopMaxTokens    StopReason = "max_tokens"
	StopToolUse      StopReason = "tool_use"
	StopStopSequence StopReason = "stop_sequence"
	StopRefusal      StopReason = "refusal"
)

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type LLMResponse struct {
	Text       string     `json:"text,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	StopReason StopReason `json:"stop_reason,omitempty"`
	Usage      Usage      `json:"usage"`
}

// LLMTool defines a unified interface for calling different LLM providers
type LLMTool interface {
	Call(ctx context.Context, req LLMRequest) (*LLMResponse, error)
	ModelID() string
	Name() string
}

// Model presets
const (
	AnthropicClaudeHaiku45 = "claude-haiku-4-5-20251001"
	AnthropicClaudeSonnet5 = "claude-sonnet-5"
	AnthropicClaudeOpus55  = "claude-opus-5-5"

	OpenAIGPT54Mini = "gpt-5.4-mini"
	OpenAIGPT54Nano = "gpt-5.4-nano"
	OpenAIGPT54     = "gpt-5.4"
	OpenAIGPT56Sol  = "gpt-5.6-sol"
	OpenAIGPT6Sol   = "gpt-6-sol"

	OllamaQwen35_9B = "qwen3.5:9b"
)

// The API providers are named for the key they bill to, apart from the
// local coding assistants (codex, claude-code) that use a subscription.
const (
	OpenAIAPI = "openai-api"
	ClaudeAPI = "claude-api"
)

// Providers are the LLM provider ids New accepts.
var Providers = []string{"codex", "claude-code", OpenAIAPI, ClaudeAPI, Bedrock, Vertex, Foundry, AzureOpenAI, "ollama"}

// ProviderID maps the old names (openai, anthropic, claude) and common
// aliases (aws, gcp, azure) to the current ones; anything else is returned
// lowercased.
func ProviderID(p string) string {
	switch p = strings.ToLower(strings.TrimSpace(p)); p {
	case "openai":
		return OpenAIAPI
	case "anthropic", "claude":
		return ClaudeAPI
	case "aws", "amazon-bedrock":
		return Bedrock
	case "vertex-ai", "gcp", "google":
		return Vertex
	case "azure-foundry", "microsoft-foundry":
		return Foundry
	case "azure", "azure-openai-api":
		return AzureOpenAI
	}
	return p
}

// New builds a provider by name with the given model ("" = provider default).
func New(provider, model string) (LLMTool, error) {
	switch ProviderID(provider) {
	case ClaudeAPI:
		return &AnthropicLLM{Model: model}, nil
	case OpenAIAPI:
		return &OpenAILLM{Model: model}, nil
	case "ollama":
		return &OllamaLLM{Model: model}, nil
	case "codex":
		return &CodexCLI{Model: model}, nil
	case "claude-code":
		return &ClaudeCodeCLI{Model: model}, nil
	case Bedrock:
		return &BedrockLLM{Model: model}, nil
	case Vertex:
		return &AnthropicLLM{Model: model, Platform: vertex{}}, nil
	case Foundry:
		return &AnthropicLLM{Model: model, Platform: foundry{}}, nil
	case AzureOpenAI:
		return NewAzureOpenAI(model), nil
	default:
		return nil, fmt.Errorf("unknown provider %q (%s)", provider, strings.Join(Providers, "|"))
	}
}

// SetEffort sets the reasoning effort on providers that support it and
// returns whether it applied. The Anthropic API is left alone: extended
// thinking cannot be combined with the forced tool choice every call here
// uses. The CLIs use structured output instead of a forced tool, so they
// take it.
func SetEffort(l LLMTool, effort string) bool {
	switch t := l.(type) {
	case *OpenAILLM:
		t.Effort = effort
	case *CodexCLI:
		t.Effort = effort
	case *ClaudeCodeCLI:
		t.Effort = effort
	default:
		return false
	}
	return true
}

// CallTool forces a call to tool and returns its arguments.
func CallTool(ctx context.Context, l LLMTool, msgs []ChatMessage, tool ToolDefinition, maxTokens int32) (map[string]any, Usage, error) {
	return CallToolIn(ctx, l, nil, msgs, tool, maxTokens)
}

// CallToolIn is CallTool with a workspace the provider may read (nil: none).
func CallToolIn(ctx context.Context, l LLMTool, ws *Workspace, msgs []ChatMessage, tool ToolDefinition, maxTokens int32) (args map[string]any, usage Usage, err error) {
	chars := 0
	for _, m := range msgs {
		chars += len(m.Content)
	}
	tools := ""
	if ws != nil {
		tools = ", reading " + ws.Dir
	}
	activity.Printf(ctx, "→ %s/%s %s: %d prompt chars%s", l.Name(), l.ModelID(), tool.Name, chars, tools)
	start := time.Now()
	defer func() {
		took := time.Since(start).Round(100 * time.Millisecond)
		if err != nil {
			activity.Errorf(ctx, "✗ %s after %s: %v", tool.Name, took, err)
			return
		}
		activity.PrintData(ctx, args, "← %s in %s, %d in / %d out tokens", tool.Name, took, usage.InputTokens, usage.OutputTokens)
	}()
	resp, err := l.Call(ctx, LLMRequest{
		Messages:   msgs,
		Tools:      []ToolDefinition{tool},
		ToolChoice: ToolChoiceRequired,
		MaxTokens:  maxTokens,
		Workspace:  ws,
	})
	if err != nil {
		return nil, Usage{}, err
	}
	for _, c := range resp.ToolCalls {
		if c.Name == tool.Name {
			return c.Arguments, resp.Usage, nil
		}
	}
	return nil, resp.Usage, fmt.Errorf("%s/%s: no %s call (stop=%s)", l.Name(), l.ModelID(), tool.Name, resp.StopReason)
}
