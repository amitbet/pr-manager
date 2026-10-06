package llm

import (
	"context"
	"errors"

	"github.com/amitbet/pr-manager/internal/ghread"
	"github.com/amitbet/pr-manager/internal/webfetch"
)

// fetchTool is the web fetch the app runs for an API provider whose
// workspace has Web (the CLIs fetch with their own tools, see session.go).
func fetchTool() LocalTool {
	return LocalTool{
		Def: ToolDefinition{Name: "fetch", Description: webfetch.Description, InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"url": map[string]any{"type": "string"}},
			"required":   []string{"url"},
		}},
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			u, _ := args["url"].(string)
			if u == "" {
				return "", errors.New("missing url")
			}
			return webfetch.Fetch(ctx, u)
		},
	}
}

// ghTool is gh for an API provider whose workspace has GH, the directory
// it runs in (the CLIs call it through the app's MCP server).
func ghTool(dir string) LocalTool {
	return LocalTool{
		Def: ToolDefinition{Name: "gh", Description: ghread.Description, InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"args": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "gh's arguments, without gh"},
			},
			"required": []string{"args"},
		}},
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			list, _ := args["args"].([]any)
			var a []string
			for _, x := range list {
				s, ok := x.(string)
				if !ok {
					return "", errors.New("args must be strings")
				}
				a = append(a, s)
			}
			return ghread.Run(ctx, dir, a)
		},
	}
}
