package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

// translation is a result's reader-facing text in another language. Lang
// is empty when the result is shown as it is (English, or already in the
// language); Units is then empty too.
type translation struct {
	Lang string `json:"lang"`
	// Source hashes the text it was translated from, so a result whose
	// text changed is translated again.
	Source   string                     `json:"source,omitempty"`
	Units    map[string]triage.UnitText `json:"units"`
	Overview *triage.Overview           `json:"overview,omitempty"`
}

func resultTexts(r *PRResult) map[string]triage.UnitText {
	texts := map[string]triage.UnitText{}
	for _, f := range r.Files {
		for _, u := range f.Units {
			texts[u.ID] = triage.TextOf(u.Unit)
		}
	}
	return texts
}

// textsHash hashes the texts and the overview. Without an overview it is
// the hash of the texts alone, as it was before overviews.
func textsHash(texts map[string]triage.UnitText, ov *triage.Overview) string {
	b, _ := json.Marshal(texts) // map keys are sorted
	if ov != nil {
		o, _ := json.Marshal(ov)
		b = append(b, o...)
	}
	h := sha256.Sum256(b)
	return fmt.Sprintf("%x", h[:8])
}

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

// translation returns r's text in the job's summary language, translating
// it once and caching it under results/translations. English is never
// translated.
func (t *triager) translation(ctx context.Context, r *PRResult, jo jobOptions) (*translation, error) {
	return t.translate(ctx, r, t.options(jo))
}

func (t *triager) translate(ctx context.Context, r *PRResult, o options) (*translation, error) {
	ctx = usageCtx(ctx, "translate", r)
	lang := o.summaryLang
	if triage.IsEnglish(lang) || strings.EqualFold(lang, r.SummaryLang) {
		return &translation{Units: map[string]triage.UnitText{}}, nil
	}
	texts := resultTexts(r)
	source := textsHash(texts, r.Overview)
	dir := filepath.Join(t.results, "translations")
	path := filepath.Join(dir, r.Key+"__"+strings.Trim(nonWord.ReplaceAllString(strings.ToLower(lang), "-"), "-")+".json")

	t.trMu.Lock()
	mu := t.trLocks[path]
	if mu == nil {
		mu = &sync.Mutex{}
		t.trLocks[path] = mu
	}
	t.trMu.Unlock()
	mu.Lock()
	defer mu.Unlock()

	var cached translation
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &cached) == nil && cached.Source == source && strings.EqualFold(cached.Lang, lang) {
		return &cached, nil
	}
	l, err := newTranslator(o)
	if err != nil {
		return nil, err
	}
	// Finish and cache even if the reader moves on to another PR, but not
	// past shutdown.
	ctx, stop := t.detached(ctx)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	units, ov, err := triage.Translate(ctx, l, lang, texts, r.Overview)
	if err != nil {
		return nil, err
	}
	tr := &translation{Lang: lang, Source: source, Units: units, Overview: ov}
	b, err := json.Marshal(tr)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return tr, os.WriteFile(path, b, 0o644)
}

var newTranslator = translator // tests replace it

// translator is the model that translates, as resolveProviders picked it.
func translator(o options) (llm.LLMTool, error) {
	if o.translator == "" || o.translator == "off" || o.translator == "openjev" {
		return nil, errors.New("no model to translate with: pick a translator provider")
	}
	l, err := llm.New(o.translator, o.translateModel)
	if err != nil {
		return nil, err
	}
	llm.SetEffort(l, o.translateEffort)
	return l, nil
}

// translateUnits translates units without a cache, for -C runs.
func translateUnits(ctx context.Context, o options, units []*triage.Unit) (*translation, error) {
	texts := map[string]triage.UnitText{}
	for _, u := range units {
		texts[u.ID] = triage.TextOf(u)
	}
	l, err := translator(o)
	if err != nil {
		return nil, err
	}
	tr := &translation{Lang: o.summaryLang}
	tr.Units, _, err = triage.Translate(ctx, l, o.summaryLang, texts, nil)
	return tr, err
}

// applyTranslation puts the -summary-lang text on units, for the CLI. A
// failed translation leaves them in English.
func applyTranslation(o options, units []*triage.Unit, tr *translation, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "pr-manager: not translated to %s: %v\n", o.summaryLang, err)
		return
	}
	for _, u := range units {
		if t, ok := tr.Units[u.ID]; ok {
			triage.ApplyText(u, t)
		}
	}
}
