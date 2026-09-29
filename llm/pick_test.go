package llm

import "testing"

func TestPickModel(t *testing.T) {
	live := func(ids ...string) Catalog {
		c := Catalog{Provider: ClaudeAPI, Live: true}
		for _, id := range ids {
			c.Models = append(c.Models, Model{ID: id})
		}
		return c
	}
	for _, tc := range []struct {
		name  string
		c     Catalog
		prefs []string
		want  string
	}{
		{"first listed pref", live("claude-sonnet-5", "claude-sonnet-5-5"), []string{"claude-sonnet-5-5", "claude-sonnet-5"}, "claude-sonnet-5-5"},
		{"cascades to the next pref", live("claude-opus-5", "claude-sonnet-5"), []string{"claude-sonnet-5-5", "claude-sonnet-5"}, "claude-sonnet-5"},
		{"dated id", live("claude-haiku-4-5-20251001"), []string{"claude-haiku-4-5"}, "claude-haiku-4-5-20251001"},
		{"newest of the family", live("claude-3-7-sonnet-20250219", "claude-sonnet-4-5-20250929", "claude-sonnet-4-20250514", "claude-opus-4-1"), []string{"claude-sonnet-5-5"}, "claude-sonnet-4-5-20250929"},
		{"family of a later pref", live("gpt-4.1-mini", "gpt-5.4-luna"), []string{"gpt-6-sol", "gpt-5.4-mini"}, "gpt-4.1-mini"},
		{"nothing alike: first listed", live("qwen3:8b", "llama3.2"), []string{"qwen3.5:9b"}, "qwen3:8b"},
		{"codex list is not what it runs", Catalog{Provider: "codex", Live: true, Models: []Model{{ID: "gpt-5.6-sol"}}}, []string{"gpt-6-sol"}, "gpt-6-sol"},
		{"built-in list: first pref", Catalog{Models: []Model{{ID: "x"}}}, []string{"claude-sonnet-5-5"}, "claude-sonnet-5-5"},
		{"no prefs", live("a"), nil, ""},
	} {
		if got := PickModel(tc.c, tc.prefs...); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
