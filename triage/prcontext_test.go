package triage

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/llm"
)

// errorCacheCase loads the inventory-error-cache fixture: retry logic moved
// out of GetRecord into the new fetchFromServer.
func errorCacheCase(t *testing.T) (EvalCase, *Source) {
	t.Helper()
	cases, err := LoadEvalCases("../testdata/eval")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Name == "inventory-error-cache" {
			src, err := c.Source()
			if err != nil {
				t.Fatal(err)
			}
			return c, src
		}
	}
	t.Fatal("fixture inventory-error-cache missing")
	return EvalCase{}, nil
}

func unitsByID(units []*Unit) map[string]*Unit {
	m := map[string]*Unit{}
	for _, u := range units {
		m[u.ID] = u
	}
	return m
}

func TestReviewContextMovedAndNewCode(t *testing.T) {
	_, src := errorCacheCase(t)
	units := BuildUnits(src.Files, src.Content, 24000)
	setReviewContext(units, src.BaseContent, DefaultReviewContextChars)
	byID := unitsByID(units)

	moved := byID["store/client.go:(*Client).fetchFromServer"].ReviewContext
	for _, want := range []string{
		"(*Client).fetchFromServer does not exist at the merge base",
		"removed from store/client.go:(*Client).GetRecord in this PR",
	} {
		if !strings.Contains(moved, want) {
			t.Errorf("fetchFromServer context lacks %q:\n%s", want, moved)
		}
	}
	// The unit the code moved from is shown first.
	if first := firstShown(moved); first != "store/client.go:(*Client).GetRecord" {
		t.Errorf("first shown = %q", first)
	}
	// Callers come before the rest of the file.
	if first := firstShown(byID["store/client.go:isErrorReply"].ReviewContext); first != "store/client.go:(*Client).fetchFromServer" {
		t.Errorf("isErrorReply first shown = %q", first)
	}
	// The modified function existed before and nothing moved into it.
	old := byID["store/client.go:(*Client).GetRecord"].ReviewContext
	if strings.Contains(old, "does not exist at the merge base") || strings.Contains(old, "code that moved") {
		t.Errorf("GetRecord notes:\n%s", old)
	}
	// Everything fits: the other file's diff is there, nothing is hidden.
	if !strings.Contains(old, "#### store/loader.go:loadMetadata") || strings.Contains(old, "not shown") {
		t.Errorf("GetRecord context:\n%s", old)
	}
}

func firstShown(ctx string) string {
	m := regexp.MustCompile(`(?m)^#### (\S+)`).FindStringSubmatch(ctx)
	if m == nil {
		return ""
	}
	return m[1]
}

func TestReviewContextBudget(t *testing.T) {
	_, src := errorCacheCase(t)
	units := BuildUnits(src.Files, src.Content, 24000)
	setReviewContext(units, src.BaseContent, 1500)
	for _, u := range units {
		if u.ReviewContext == "" {
			continue
		}
		shown := 0
		for _, m := range regexp.MustCompile("(?s)```diff\n(.*?)\n```").FindAllStringSubmatch(u.ReviewContext, -1) {
			shown += len(m[1])
		}
		if shown > 1500 {
			t.Errorf("%s: %d chars of context over budget", u.ID, shown)
		}
		if !strings.Contains(u.ReviewContext, "Also changed, not shown: ") {
			t.Errorf("%s: hidden units not listed", u.ID)
		}
	}
}

func TestReviewPromptShowsTheRestOfThePR(t *testing.T) {
	_, src := errorCacheCase(t)
	prompts := map[string]string{}
	var systems []string
	review := &fakeLLM{fn: func(req llm.LLMRequest) (*llm.LLMResponse, error) {
		prompts[strings.SplitN(req.Messages[1].Content, "\n", 3)[1]] = req.Messages[1].Content
		systems = append(systems, req.Messages[0].Content)
		return toolResp(req.Tools[0].Name, map[string]any{"bucket": "human", "change_kind": "behavior", "confidence": 0.9, "reason": "r",
			"headline": "h", "summary": "s", "focus": []any{}, "issues": []any{
				map[string]any{"severity": "high", "title": "Pointer errors bypass detection", "failure_scenario": "x",
					"introduced_by_pr": true, "depends_on_unseen_code": true},
			}}), nil
	}}
	p := &Pipeline{
		Presorter: &Presorter{Policy: DefaultPolicy()},
		// Tools on, but the fake provider can't use a workspace.
		Summarizer: &Summarizer{LLM: review, Policy: DefaultPolicy(), Tools: true},
	}
	units := unitsByID(p.Run(context.Background(), src))
	pr := prompts["Declaration: (*Client).fetchFromServer"]
	if !strings.Contains(pr, "Other changes in the same PR") || !strings.Contains(pr, "code that moved") || strings.Contains(pr, "(not shown)") {
		t.Errorf("review prompt:\n%s", pr)
	}
	for _, s := range systems {
		if strings.Contains(s, "You can read the repository") {
			t.Error("tools instructions without a workspace")
		}
	}
	u := units["store/client.go:isErrorReply"]
	if len(u.Issues) != 0 || len(u.Focus) != 1 || !strings.HasPrefix(u.Focus[0], "unverified: Pointer errors") || !u.Reviewed {
		t.Errorf("unseen-code claim: issues %+v focus %q", u.Issues, u.Focus)
	}
}

func TestReviewWorkspaceFromHeadDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, cleanup, err := reviewWorkspace(&Source{HeadDir: dir}, nil)
	defer cleanup()
	if err != nil || ws == nil || ws.Dir != dir {
		t.Fatalf("ws = %+v, err = %v", ws, err)
	}
	if mc := goModCache(); mc != "" && (len(ws.ReadDirs) != 1 || ws.ReadDirs[0] != mc) {
		t.Errorf("read dirs = %q, want module cache %q", ws.ReadDirs, mc)
	}
	if ws, _, _ := reviewWorkspace(&Source{}, nil); ws != nil {
		t.Errorf("no head: ws = %+v", ws)
	}
}

func TestReviewWorkspaceWorktree(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"},
	} {
		if _, err := Git(repo, args...); err != nil {
			t.Skip("git unavailable:", err)
		}
	}
	ws, cleanup, err := reviewWorkspace(&Source{Dir: repo, Head: "HEAD"}, nil)
	if err != nil || ws == nil {
		t.Fatalf("ws = %+v, err = %v", ws, err)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, ".git")); err != nil {
		t.Errorf("worktree not checked out: %v", err)
	}
	cleanup()
	if _, err := os.Stat(ws.Dir); !os.IsNotExist(err) {
		t.Errorf("worktree left behind: %v", err)
	}
}

func movedUnit(id string, lines ...string) *Unit {
	return &Unit{ID: id, File: id, Status: StatusModified, Hunks: []Hunk{{Header: "@@ -1 +1 @@", Lines: lines}}}
}

// Error checks and zero-value returns are in every function; sharing them
// does not make the added code moved.
func TestMovedNeedsMoreThanBoilerplate(t *testing.T) {
	stock := []string{"if err != nil {", "return nil, err", "return false, err", "} else if err != nil {"}
	var added, removed []string
	for _, l := range stock {
		added, removed = append(added, "+\t"+l), append(removed, "-\t"+l)
	}
	added = append(added, "+\tresult := chargeCard(ctx, card, amount)", "+\tledger.Record(result.ID, amount)")
	a, b := movedUnit("a.go:New", added...), movedUnit("b.go:Old", removed...)
	setReviewContext([]*Unit{a, b}, nil, DefaultReviewContextChars)
	if strings.Contains(a.ReviewContext, "code that moved") {
		t.Errorf("boilerplate counted as moved:\n%s", a.ReviewContext)
	}

	// Three distinctive lines in a row did move, and the note gives the share.
	moved := []string{"conn, err := dial(ctx, addr)", "resp, err := conn.Get(ctx, key)", "cache.Put(key, resp, ttl)"}
	added, removed = nil, nil
	for _, l := range moved {
		added, removed = append(added, "+\t"+l), append(removed, "-\t"+l)
	}
	added = append(added, "+\tmetrics.Inc(\"fetch_total\")", "+\tlog.Printf(\"fetched %s\", key)")
	a, b = movedUnit("a.go:New", added...), movedUnit("b.go:Old", removed...)
	setReviewContext([]*Unit{a, b}, nil, DefaultReviewContextChars)
	if !strings.Contains(a.ReviewContext, "3 of the 5 distinctive added lines here match lines removed from b.go:Old") || !strings.Contains(a.ReviewContext, "the other 2 are new") {
		t.Errorf("moved note:\n%s", a.ReviewContext)
	}

	// Scattered matches count only as most of the added code.
	added = []string{"+\t" + moved[0], "+\tregisterHandler(ctx, 1)", "+\t" + moved[1], "+\tregisterHandler(ctx, 2)", "+\t" + moved[2], "+\tregisterHandler(ctx, 3)"}
	a, b = movedUnit("a.go:New", added...), movedUnit("b.go:Old", removed...)
	setReviewContext([]*Unit{a, b}, nil, DefaultReviewContextChars)
	if strings.Contains(a.ReviewContext, "code that moved") {
		t.Errorf("3 scattered lines of 6 counted as moved:\n%s", a.ReviewContext)
	}
}
