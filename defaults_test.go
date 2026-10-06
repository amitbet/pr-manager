package main

import (
	"testing"

	"github.com/amitbet/pr-manager/llm"
)

// Tests never probe providers: every default is the first of its list.
func init() {
	modelCatalog = func(p string) llm.Catalog { return llm.Catalog{Provider: p} }
}

func TestDefaultModelCascades(t *testing.T) {
	old := modelCatalog
	defer func() { modelCatalog = old }()
	modelCatalog = func(p string) llm.Catalog {
		return llm.Catalog{Provider: llm.ClaudeAPI, Live: true, Models: []llm.Model{{ID: "claude-sonnet-5"}, {ID: "claude-haiku-4-5-20251001"}}}
	}
	o := resolveProviders(options{summarizer: llm.ClaudeAPI, classifier: llm.ClaudeAPI, translator: "auto", translateEffort: "auto"})
	if o.translateModel != "claude-sonnet-5" {
		t.Errorf("translate model %q, want claude-sonnet-5 (no 5.5 on this endpoint)", o.translateModel)
	}
	if o.classifyModel != "claude-haiku-4-5-20251001" || o.summaryModel != "claude-sonnet-5" {
		t.Errorf("classify %q, summary %q", o.classifyModel, o.summaryModel)
	}
}

// The chat agent follows the reviewer, with the reviewer's model; on
// another provider it gets that provider's analyze defaults.
func TestChatDefaults(t *testing.T) {
	o := resolveProviders(options{summarizer: llm.ClaudeAPI, summaryModel: "claude-x", classifier: llm.ClaudeAPI, chat: "auto"})
	if o.chat != llm.ClaudeAPI || o.chatModel != "claude-x" {
		t.Errorf("chat %s/%s, want the reviewer's claude-api/claude-x", o.chat, o.chatModel)
	}
	o = resolveProviders(options{summarizer: llm.ClaudeAPI, classifier: llm.ClaudeAPI, chat: "openai"})
	if o.chat != llm.OpenAIAPI || o.chatModel != summaryDefaults[llm.OpenAIAPI][0] {
		t.Errorf("chat %s/%s, want openai-api's analyze default", o.chat, o.chatModel)
	}
	if o := resolveProviders(options{summarizer: "off", classifier: "openjev", fallback: "off", chat: "auto"}); o.chat != "" {
		t.Errorf("chat %q with no provider that writes text", o.chat)
	}
}
