package llm

import (
	"context"
	"sync"
	"time"
)

// Every model call's tokens are recorded, with what it was for: the
// context says the activity (a job's kind, chat, ...), the change (a PR's
// URL or a local checkout) and the result, and the call adds the provider,
// the model and its tool (classify, review, reply, ...). The app keeps the
// records (usage.go in package main) for Settings → Statistics.

// UsageTag is what calls in a context are for.
type UsageTag struct {
	Activity string `json:"activity,omitempty"` // triage, fix, chat, comments, overview, sequence, ...
	Source   string `json:"source,omitempty"`   // the PR's URL, or the local checkout (path#rev)
	Key      string `json:"key,omitempty"`      // the result, when known
	Conv     string `json:"conv,omitempty"`     // the chat conversation
}

// UsageRecord is one call's tokens.
type UsageRecord struct {
	T        time.Time `json:"t"`
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	Tool     string    `json:"tool"`
	UsageTag
	In  int `json:"in"`
	Out int `json:"out"`
}

type usageKey struct{}

// WithUsage tags the calls made in ctx. An empty field keeps the
// enclosing tag's.
func WithUsage(ctx context.Context, tag UsageTag) context.Context {
	if old, ok := ctx.Value(usageKey{}).(UsageTag); ok {
		if tag.Activity == "" {
			tag.Activity = old.Activity
		}
		if tag.Source == "" {
			tag.Source = old.Source
		}
		if tag.Key == "" {
			tag.Key = old.Key
		}
		if tag.Conv == "" {
			tag.Conv = old.Conv
		}
	}
	return context.WithValue(ctx, usageKey{}, tag)
}

// WithUsageDefault tags ctx with tag unless it has a tag already.
func WithUsageDefault(ctx context.Context, tag UsageTag) context.Context {
	if _, ok := ctx.Value(usageKey{}).(UsageTag); ok {
		return ctx
	}
	return context.WithValue(ctx, usageKey{}, tag)
}

var usageSink struct {
	sync.RWMutex
	f func(UsageRecord)
}

// SetUsageSink sets where records go (nil: nowhere).
func SetUsageSink(f func(UsageRecord)) {
	usageSink.Lock()
	usageSink.f = f
	usageSink.Unlock()
}

// recordUsage records a call's tokens, if it spent any.
func recordUsage(ctx context.Context, l LLMTool, tool string, u *Usage) {
	if u == nil || u.InputTokens+u.OutputTokens == 0 {
		return
	}
	usageSink.RLock()
	f := usageSink.f
	usageSink.RUnlock()
	if f == nil {
		return
	}
	tag, _ := ctx.Value(usageKey{}).(UsageTag)
	f(UsageRecord{T: time.Now(), Provider: l.Name(), Model: l.ModelID(), Tool: tool, UsageTag: tag, In: u.InputTokens, Out: u.OutputTokens})
}
