package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/internal/activity"
)

// The API providers have no agent of their own to read a workspace with,
// so the app runs one: the model gets the answer tool and the workspace
// tools (fstools.go), the app runs every call it makes and sends back the
// output, until it calls the answer tool. The CLIs do the same inside
// their own process.

const (
	toolRounds      = 40      // model calls of one answer
	toolOutputTotal = 600_000 // chars of tool output over one answer
	toolNudges      = 2       // text-only answers asked again
	// CharsPerToken is a conservative ratio for sizing text to a window.
	CharsPerToken = 3
)

// LocalTool is a tool the app runs for the model.
type LocalTool struct {
	Def ToolDefinition
	Run func(ctx context.Context, args map[string]any) (string, error)
}

// callInWorkspace is CallToolReading on an API provider: final, with the
// workspace's tools.
func callInWorkspace(ctx context.Context, l LLMTool, ws *Workspace, msgs []ChatMessage, final ToolDefinition, maxTokens int32) (map[string]any, Usage, error) {
	fs, err := newWorkspaceTools(ws)
	if err != nil {
		return nil, Usage{}, err
	}
	tools := fs.tools()
	if ws.Web {
		tools = append(tools, fetchTool())
	}
	if ws.GH != "" {
		tools = append(tools, ghTool(ws.GH))
	}
	return CallWithTools(ctx, l, msgs, final, tools, maxTokens)
}

// CallWithTools lets the model call tools, runs them, and returns the
// arguments of its call to final, which ends it. The last round forces
// final, so an answer comes out of a long search too.
func CallWithTools(ctx context.Context, l LLMTool, msgs []ChatMessage, final ToolDefinition, tools []LocalTool, maxTokens int32) (args map[string]any, usage Usage, err error) {
	defs := []ToolDefinition{final}
	byName := map[string]LocalTool{}
	for _, t := range tools {
		defs = append(defs, t.Def)
		byName[t.Def.Name] = t
	}
	chars := 0
	for _, m := range msgs {
		chars += len(m.Content)
	}
	activity.Printf(ctx, "→ %s/%s %s: %d prompt chars, with %d tools", l.Name(), l.ModelID(), final.Name, chars, len(tools))
	start, calls := time.Now(), 0
	defer func() {
		took := time.Since(start).Round(100 * time.Millisecond)
		if err != nil {
			activity.Errorf(ctx, "✗ %s after %s and %d tool calls: %v", final.Name, took, calls, err)
			return
		}
		activity.PrintData(ctx, args, "← %s in %s, %d tool calls, %d in / %d out tokens", final.Name, took, calls, usage.InputTokens, usage.OutputTokens)
	}()

	msgs = append([]ChatMessage(nil), msgs...)
	left, nudges := toolOutputTotal, 0
	// A model with a small window gets smaller outputs, and loses the
	// oldest ones when the conversation outgrows it (see fitWindow).
	callMax, window := toolOutputMax, 0
	if n := ContextTokens(ctx, l); n > 0 {
		window = (n - min(int(maxTokens), n/4)) * CharsPerToken
		callMax = max(window/5, 2000)
	}
	for round := 0; ; round++ {
		last := round >= toolRounds-1 || left <= 0
		choice := ToolChoiceAny
		if last {
			choice = ToolChoiceRequired
		}
		if window > 0 {
			fitWindow(msgs, defs, window)
		}
		resp, err := l.Call(ctx, LLMRequest{Messages: msgs, Tools: defs, ToolChoice: choice, MaxTokens: maxTokens})
		if err != nil {
			return nil, usage, err
		}
		usage.InputTokens += resp.Usage.InputTokens
		usage.OutputTokens += resp.Usage.OutputTokens
		var todo []ToolCall
		for i, c := range resp.ToolCalls {
			if c.Name == final.Name {
				return c.Arguments, usage, nil
			}
			if c.CallID == "" { // Ollama has no ids
				c.CallID = fmt.Sprintf("call_%d_%d", round, i)
			}
			todo = append(todo, c)
		}
		if len(todo) == 0 {
			if last || nudges >= toolNudges {
				return nil, usage, fmt.Errorf("%s/%s: no %s call (stop=%s)", l.Name(), l.ModelID(), final.Name, resp.StopReason)
			}
			nudges++
			msgs = append(msgs, ChatMessage{Role: "assistant", Content: orNone(resp.Text)},
				ChatMessage{Role: "user", Content: fmt.Sprintf("Answer by calling the %s tool, or call the other tools first to look further.", final.Name)})
			continue
		}
		results := runLocal(ctx, byName, todo)
		calls += len(todo)
		for i := range results {
			r := &results[i]
			if len(r.Content) > callMax {
				r.Content = r.Content[:callMax] + "\n[cut to fit the model's context window: narrow the call]"
			}
			if len(r.Content) > left {
				r.Content = r.Content[:max(left, 0)] + "\n[cut: the tool output budget of this answer is spent; answer now]"
			}
			left -= len(r.Content)
		}
		msgs = append(msgs, ChatMessage{Role: "assistant", Content: resp.Text, ToolCalls: todo}, ChatMessage{Role: "user", ToolResults: results})
	}
}

// fitWindow drops the output of the oldest tool calls until the
// conversation fits window chars, so the server doesn't cut it from the
// start, where the question is.
func fitWindow(msgs []ChatMessage, defs []ToolDefinition, window int) {
	size := jsonLen(defs)
	for _, m := range msgs {
		size += len(m.Content)
		for _, c := range m.ToolCalls {
			size += jsonLen(c.Arguments) + len(c.Name)
		}
		for _, r := range m.ToolResults {
			size += len(r.Content)
		}
	}
	const dropped = "[output dropped to fit the context window; call again if you still need it]"
	for i := range msgs {
		for k := range msgs[i].ToolResults {
			if size <= window {
				return
			}
			r := &msgs[i].ToolResults[k]
			if len(r.Content) > len(dropped) {
				size -= len(r.Content) - len(dropped)
				r.Content = dropped
			}
		}
	}
}

// runLocal runs calls at once and returns their results in order.
func runLocal(ctx context.Context, byName map[string]LocalTool, calls []ToolCall) []ToolResult {
	out := make([]ToolResult, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = ToolResult{CallID: c.CallID, Name: c.Name}
			t, ok := byName[c.Name]
			if !ok {
				out[i].Content, out[i].IsError = fmt.Sprintf("there is no tool %q", c.Name), true
				return
			}
			text, err := t.Run(ctx, argsOrEmpty(c.Arguments))
			if err != nil {
				text, out[i].IsError = err.Error(), true
				activity.Errorf(ctx, "  %s %s: %v", c.Name, argsLine(c.Arguments), err)
			} else {
				activity.Printf(ctx, "  %s %s: %d chars", c.Name, argsLine(c.Arguments), len(text))
			}
			out[i].Content = text
		}()
	}
	wg.Wait()
	return out
}

func argsLine(a map[string]any) string {
	b, _ := json.Marshal(a)
	if len(b) > 300 {
		return string(b[:300]) + "…"
	}
	return string(b)
}

func jsonLen(v any) int {
	b, _ := json.Marshal(v)
	return len(b)
}
