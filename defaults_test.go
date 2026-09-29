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
