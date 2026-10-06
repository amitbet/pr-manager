package main

import (
	"context"
	"testing"
	"time"

	"github.com/amitbet/pr-manager/llm"
)

// usageLLM answers reply with some tokens spent.
type usageLLM struct{ in, out int }

func (u *usageLLM) Call(_ context.Context, _ llm.LLMRequest) (*llm.LLMResponse, error) {
	return &llm.LLMResponse{ToolCalls: []llm.ToolCall{{Name: "reply", Arguments: map[string]any{"answer": "ok"}}}, Usage: llm.Usage{InputTokens: u.in, OutputTokens: u.out}}, nil
}
func (u *usageLLM) ModelID() string { return "m1" }
func (u *usageLLM) Name() string    { return "fake" }

// Every call's tokens are kept with what it was for, and the report sums
// them by activity, day, model, repository and change.
func TestUsageReport(t *testing.T) {
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer llm.SetUsageSink(nil)
	call := func(ctx context.Context, in, out int) {
		if _, _, err := llm.CallTool(ctx, &usageLLM{in, out}, []llm.ChatMessage{{Role: "user", Content: "q"}}, chatTool, 100); err != nil {
			t.Fatal(err)
		}
	}
	pr := "https://github.com/acme/web/pull/7"
	_, job, _ := tr.newJob("triage", pr)
	call(job, 1000, 100)
	call(llm.WithUsage(context.Background(), llm.UsageTag{Activity: "chat", Source: pr, Conv: "c"}), 500, 50)
	call(llm.WithUsage(context.Background(), llm.UsageTag{Activity: "fix", Source: "/src/app#feature"}), 200, 20)
	call(context.Background(), 0, 0) // nothing spent, nothing kept

	rep := tr.usageReport(time.Time{}, time.Now())
	if rep.Totals.Calls != 3 || rep.Totals.In != 1700 || rep.Totals.Out != 170 {
		t.Fatalf("totals %+v", rep.Totals)
	}
	if rep.Totals.By["analysis"] != 1100 || rep.Totals.By["chat"] != 550 || rep.Totals.By["fixing"] != 220 {
		t.Errorf("by activity %v", rep.Totals.By)
	}
	if len(rep.Repos) != 2 || rep.Repos[0].Repo != "acme/web" || len(rep.Repos[0].Items) != 1 || rep.Repos[0].Items[0].Label != "#7" {
		t.Fatalf("repos %+v", rep.Repos)
	}
	if local := rep.Repos[1]; local.Repo != "app (local)" || local.Items[0].Label != "feature" || local.Items[0].Sub != "feature" {
		t.Errorf("local %+v %+v", local, local.Items[0])
	}
	if len(rep.Days) != 1 || rep.Days[0].In != 1700 || len(rep.Models) != 1 || rep.Models[0].Model != "fake/m1" {
		t.Errorf("days %+v models %+v", rep.Days, rep.Models)
	}
	if got := tr.usageReport(time.Now().Add(time.Hour), time.Now().Add(time.Hour)); got.Totals.Calls != 0 {
		t.Errorf("a later range has %d calls", got.Totals.Calls)
	}
}
