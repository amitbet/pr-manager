package llm

import (
	"context"
	"errors"

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
