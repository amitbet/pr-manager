package llm

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PickModel returns the first of prefs that c can run, so a default
// cascades to an older model on an endpoint that doesn't have the newest (a
// gateway, a self-hosted proxy, an older account). A pref matches a listed
// id equal to it, then one containing it, like a dated
// claude-haiku-4-5-20251001 or a prefixed us.anthropic.claude-sonnet-5-v1:0.
// With no pref listed it takes the newest listed model of the first pref's
// family (sonnet, haiku, luna, mini...), else the first listed model. A list
// that isn't from the provider says nothing about what runs, so prefs[0]
// stands.
func PickModel(c Catalog, prefs ...string) string {
	if len(prefs) == 0 {
		return ""
	}
	ids := modelIDs(c.Models)
	if !c.Live || len(ids) == 0 || listsModels[c.Provider] == nil {
		return prefs[0]
	}
	for _, p := range prefs {
		if slices.Contains(ids, p) {
			return p
		}
	}
	for _, p := range prefs {
		for _, id := range ids {
			if strings.Contains(id, p) {
				return id
			}
		}
	}
	for _, p := range prefs {
		f := family(p)
		if f == "" {
			continue
		}
		best := ""
		for _, id := range ids {
			if family(id) == f && (best == "" || newer(id, best)) {
				best = id
			}
		}
		if best != "" {
			return best
		}
	}
	return ids[0]
}

func modelIDs(ms []Model) []string {
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
	}
	return ids
}

// families are the size tiers model names carry.
var families = []string{"fable", "opus", "sonnet", "haiku", "astra", "sol", "terra", "luna", "nano", "mini"}

var nameToken = regexp.MustCompile(`[a-z]+|\d+`)

func family(id string) string {
	for _, t := range nameToken.FindAllString(strings.ToLower(id), -1) {
		if slices.Contains(families, t) {
			return t
		}
	}
	return ""
}

// newer compares the version numbers in two ids (claude-sonnet-4-5 over
// claude-sonnet-4, gpt-5.6-luna over gpt-5.4-luna). Numbers of four digits
// or more are dates and only break ties.
func newer(a, b string) bool {
	va, da := versionOf(a)
	vb, db := versionOf(b)
	if c := slices.Compare(va, vb); c != 0 {
		return c > 0
	}
	return da > db
}

func versionOf(id string) (version []int, date int) {
	for _, t := range nameToken.FindAllString(id, -1) {
		n, err := strconv.Atoi(t)
		if err != nil {
			continue
		}
		if len(t) >= 4 {
			date = n
		} else {
			version = append(version, n)
		}
	}
	return version, date
}

// listsModels are the providers whose list is what the endpoint runs. The
// rest have a built-in list, or, for codex, one that follows the codex
// profile and leaves out models it runs (gpt-6-sol), so probing them tells
// PickModel nothing.
var listsModels = map[string]func(context.Context) Catalog{
	ClaudeAPI: probeAnthropic, OpenAIAPI: probeOpenAI, "ollama": probeOllama, AzureOpenAI: probeAzureOpenAI,
}

var (
	provMu    sync.Mutex
	provCache = map[string]Catalog{}
	provAt    = map[string]time.Time{}
)

// ProviderCatalog is one provider's catalog for PickModel: the last
// Catalogs probe when it is fresh, else a probe of that provider alone,
// kept as long. A provider without a live list is not probed.
func ProviderCatalog(ctx context.Context, provider string) Catalog {
	probe := listsModels[provider]
	if probe == nil {
		return Catalog{Provider: provider}
	}
	catMu.Lock()
	if catCache != nil && time.Since(catAt) < cacheFor {
		for _, c := range catCache {
			if c.Provider == provider {
				catMu.Unlock()
				return c
			}
		}
	}
	catMu.Unlock()
	provMu.Lock()
	defer provMu.Unlock()
	if c, ok := provCache[provider]; ok && time.Since(provAt[provider]) < cacheFor {
		return c
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := probe(ctx)
	provCache[provider], provAt[provider] = c, time.Now()
	return c
}
