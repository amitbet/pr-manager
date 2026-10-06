package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/amitbet/pr-manager/internal/activity"
	"github.com/amitbet/pr-manager/llm"
)

// A session carries on while the conversation it saw is the start of the
// one asked about, whatever became of the answer's actions since; a
// changed turn, another model or another directory starts a new one.
func TestChatSession(t *testing.T) {
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	fake := &chatLLM{}
	ws := &llm.Workspace{Dir: "/w", Session: &llm.Session{ID: "s1", System: "h"}}
	first := []chatTurn{{Role: "user", Content: "why?", At: "2026-10-06T10:00:00Z"}}
	cv := tr.openChat(chatRequest{Conversation: "github.com/o/r#1", Messages: first})
	cv.saveSession(fake, ws)
	cv.close()

	next := append(append([]chatTurn{}, first...),
		chatTurn{Role: "assistant", Content: "because [proposed actions: fix {} → done]", At: cv.at},
		chatTurn{Role: "event", Content: "fix done", At: "2026-10-06T10:05:00Z"},
		chatTurn{Role: "user", Content: "and now?", At: "2026-10-06T10:06:00Z"})
	cv = tr.openChat(chatRequest{Conversation: "github.com/o/r#1", Messages: next})
	defer cv.close()
	s := cv.resumable(fake, ws)
	if s == nil || s.Session.ID != "s1" || s.Turns != 2 {
		t.Fatalf("session %+v", s)
	}
	if got := chatTurns(next[s.Turns:]); len(got) != 1 || !strings.Contains(got[0].Content, "fix done") || !strings.Contains(got[0].Content, "and now?") {
		t.Errorf("new turns %+v", got)
	}
	if cv.resumable(fake, &llm.Workspace{Dir: "/other"}) != nil {
		t.Error("carried on in another directory")
	}
	cv.turns = append([]chatTurn{{Role: "user", Content: "how?", At: first[0].At}}, next[1:]...)
	if cv.resumable(fake, ws) != nil {
		t.Error("carried on after an earlier turn changed")
	}
	cv.turns = next[:2]
	if cv.resumable(fake, ws) != nil {
		t.Error("carried on with no new turn")
	}
}

// Conversations are saved on disk, and an empty one deletes it.
func TestChatStore(t *testing.T) {
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	tr.chatConvRoutes(mux)
	do := func(method, body string) string {
		req := httptest.NewRequest(method, "/api/chats?change="+url.QueryEscape("local:/src/app#main"), strings.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", method, w.Code, w.Body)
		}
		return w.Body.String()
	}
	if got := do("GET", ""); !strings.Contains(got, `"messages":[]`) {
		t.Errorf("empty conversation %s", got)
	}
	do("PUT", `{"messages":[{"role":"user","content":"hi"}]}`)
	if got := do("GET", ""); !strings.Contains(got, `"content":"hi"`) {
		t.Errorf("saved conversation %s", got)
	}
	do("PUT", `{"messages":[]}`)
	if _, err := os.Stat(tr.chatsDir() + "/" + convHash("local:/src/app#main") + ".json"); !os.IsNotExist(err) {
		t.Errorf("an emptied conversation is still on disk: %v", err)
	}
}

// The reader's turn says what is selected and what the open text boxes
// hold; a long conversation keeps a digest of the turns it leaves out.
func TestChatViewText(t *testing.T) {
	got := chatViewText(chatView{Where: "Review tab, Files mode", Path: "a.go",
		Selection: &chatSelection{Path: "a.go", NewStart: 10, NewEnd: 12, Text: "retry()\n"},
		Fields:    []chatField{{Name: "comment", Path: "a.go", Side: "RIGHT", Line: 12, Text: "nit: cap this", Focused: true}}})
	for _, want := range []string{"Files mode", "a.go, new lines 10-12", "retry()", "review comment on a.go:12 (new file)", "typing in it", "nit: cap this"} {
		if !strings.Contains(got, want) {
			t.Errorf("view text lacks %q:\n%s", want, got)
		}
	}
	var turns []chatTurn
	for i := range chatMaxMessages + 4 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		turns = append(turns, chatTurn{Role: role, Content: "turn " + string(rune('a'+i))})
	}
	out := chatTurns(turns)
	if !strings.HasPrefix(out[0].Content, "[Earlier in this conversation") || !strings.Contains(out[0].Content, "- turn a") {
		t.Errorf("no digest of the left-out turns: %q", out[0].Content)
	}
}

// The work log reads the CLI's activity as steps: thinking, reads,
// commands (codex's unwrapped from their shell), gh and web calls, with
// an error marking the step it belongs to, and without the app's own
// lines around the call.
func TestWorkEntries(t *testing.T) {
	now := time.Now()
	lines := []string{
		"→ claude-code/sonnet reply: 12000 prompt chars, reading /w",
		"$ claude -p --output-format stream-json",
		"thinking: the retry loop looks unbounded",
		"→ Read a.go (from line 1, 40 lines)",
		"$ /bin/zsh -lc 'git log -L 10,12:a.go'",
		"  exit 128: git log",
		"→ gh pr checks 12",
		"… thought ~200 tokens so far",
		"… thought ~400 tokens so far",
		"← reply in 12s, 100 in / 20 out tokens",
	}
	var th activity.Thread
	for i, l := range lines {
		th.Lines = append(th.Lines, activity.Line{T: now.Add(time.Duration(i) * time.Second), Text: l})
	}
	got := workEntries([]activity.Thread{th})
	want := []workEntry{
		{K: "think", X: "the retry loop looks unbounded"},
		{K: "read", X: "Read a.go (from line 1, 40 lines)"},
		{K: "run", X: "git log -L 10,12:a.go", Err: true},
		{K: "web", X: "gh pr checks 12"},
		{K: "progress", X: "thought ~400 tokens so far"},
	}
	if len(got) != len(want) {
		t.Fatalf("entries %+v", got)
	}
	for i := range want {
		if got[i].K != want[i].K || got[i].X != want[i].X || got[i].Err != want[i].Err {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// An answer's work log gets what the provider writes to the thread in
// its context, as the CLIs do.
func TestChatRunLog(t *testing.T) {
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cv := tr.openChat(chatRequest{Run: "r1"})
	ctx := cv.ctx(t.Context())
	w := activity.Writer(ctx, "")
	w.Write([]byte("→ Read a.go\n"))
	cv.close()
	if got := cv.run.entries(); len(got) != 1 || got[0].X != "Read a.go" {
		t.Errorf("entries %+v", got)
	}
}
