package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/internal/activity"
	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

// A chat conversation is kept on disk, per change (the PR, or the local
// checkout), under <cache>/chats/<hash>:
//
//	<hash>.json            the conversation as the UI keeps it
//	<hash>.session.json    the CLI session it carries on in (chatSession)
//	<hash>/code            a worktree of the reviewed head, kept between turns
//	<hash>/material        the material as files (see chatbundle.go)
//
// On codex and claude-code each answer carries on the CLI's own session
// (llm.Session): the agent keeps what it read and ran, and is sent only
// the turns since its last answer. Claude Code finds a session by its
// working directory, so the code and the material stay where they are.
// When the conversation no longer matches the session (a retry, a new
// conversation, another model), or resuming fails, a new session starts
// with the whole conversation.

const (
	chatConvMax     = 8 << 20             // bytes of a saved conversation
	chatConvKeep    = 30 * 24 * time.Hour // conversations untouched longer go
	chatCodeKeep    = 7 * 24 * time.Hour  // and worktrees untouched longer
	chatPruneEvery  = time.Hour
	chatRunKeep     = 2 * time.Minute // a finished run's log stays this long
	chatWorkEntries = 120             // entries of a work log
)

func (t *triager) chatsDir() string { return filepath.Join(t.opts.cache, "chats") }

func convHash(change string) string {
	sum := sha256.Sum256([]byte(change))
	return hex.EncodeToString(sum[:8])
}

// chatSession is a conversation's CLI session.
type chatSession struct {
	Provider string      `json:"provider"`
	Model    string      `json:"model"`
	Dir      string      `json:"dir"`            // the agent's working directory
	Repo     string      `json:"repo,omitempty"` // the clone the worktree is of
	Session  llm.Session `json:"session"`
	// Turns are how many turns of the conversation the session has seen,
	// its answer included; Prefix is their hash (turnsHash).
	Turns  int       `json:"turns"`
	Prefix string    `json:"prefix"`
	At     time.Time `json:"at"`
}

// turnsHash identifies the turns of a conversation. An answer counts by
// its time alone: the UI adds what became of its actions to its text.
func turnsHash(turns []chatTurn) string {
	h := sha256.New()
	for _, m := range turns {
		c := m.Content
		if m.Role == "assistant" {
			c = ""
		}
		fmt.Fprintf(h, "%s\x00%s\x00%s\x01", m.Role, m.At, c)
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

var chatLocks sync.Map // hash -> *sync.Mutex

// chatConv is one answer's view of its conversation.
type chatConv struct {
	t          *triager
	c          context.Context // the answer's (see ctx)
	id, hash   string
	dir        string // <cache>/chats/<hash>
	sess       *chatSession
	unlock     func()
	run        *chatRun
	at         string // when it answered
	turns      []chatTurn
	repo       string
	scratch    bool // the workspace is the conversation's own worktree
	resumed    bool
	noWeb      bool
	freshStart bool
}

// openChat opens req's conversation, waiting for an answer to it already
// being written. Without a conversation id everything is as it was: a
// temporary worktree and bundle, no session.
func (t *triager) openChat(req chatRequest) *chatConv {
	cv := &chatConv{t: t, id: req.Conversation, unlock: func() {}, turns: req.Messages, noWeb: req.NoWeb}
	if req.Run != "" {
		cv.run = t.startChatRun(req.Run)
	}
	if cv.id == "" || t.opts.cache == "" {
		cv.id = ""
		return cv
	}
	t.pruneChats()
	cv.hash = convHash(cv.id)
	cv.dir = filepath.Join(t.chatsDir(), cv.hash)
	mu, _ := chatLocks.LoadOrStore(cv.hash, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	cv.unlock = mu.(*sync.Mutex).Unlock
	if b, err := os.ReadFile(cv.dir + ".session.json"); err == nil {
		var s chatSession
		if json.Unmarshal(b, &s) == nil {
			cv.sess = &s
		}
	}
	return cv
}

func (cv *chatConv) close() {
	cv.unlock()
	if cv.run != nil {
		cv.run.finish()
	}
}

// ctx carries the answer's work log, when the UI follows it, with a
// thread open: the CLIs' output is written to the thread in ctx.
func (cv *chatConv) ctx(ctx context.Context) context.Context {
	if cv.run != nil {
		ctx, cv.run.thread = activity.Start(activity.With(ctx, cv.run.log), "llm", "chat")
	}
	ctx = llm.WithUsage(ctx, llm.UsageTag{Activity: "chat", Conv: cv.id})
	cv.c = ctx
	return ctx
}

// workspace is chatWorkspace, but for a conversation the head's worktree
// is its own and stays between turns.
func (cv *chatConv) workspace(ctx context.Context, r *PRResult) (dir string, cleanup func(), note string, err error) {
	exists := func(d string) bool {
		st, err := os.Stat(d)
		return err == nil && st.IsDir()
	}
	stable := (r.LocalFixDir != "" && exists(r.LocalFixDir)) || (r.PR.LocalPath != "" && r.PR.Rev == "" && exists(r.PR.LocalPath))
	if cv.id == "" || stable {
		return cv.t.chatWorkspace(ctx, r)
	}
	cleanup = func() {}
	t := cv.t
	repo := r.PR.LocalPath
	if repo == "" && t.fetcher != nil {
		repo = t.fetcher.RepoDir(r.PR.PRRef)
	}
	if repo == "" || r.PR.HeadOid == "" || !exists(repo) {
		return "", cleanup, "", nil
	}
	cv.repo = repo
	code := filepath.Join(cv.dir, "code")
	if err := convWorktree(ctx, repo, code, r.PR.HeadOid); err != nil {
		return "", cleanup, "", err
	}
	// The PR head is untrusted: its agent files must not reach the agent
	// as the project's instructions.
	if r.PR.LocalPath == "" {
		if err := triage.StripAgentFiles(code); err != nil {
			return "", cleanup, "", err
		}
	}
	now := time.Now()
	_ = os.Chtimes(code, now, now) // in use: see pruneChats
	cv.scratch = true
	if d, err := filepath.EvalSymlinks(code); err == nil {
		code = d
	}
	return code, cleanup, "the repository at the reviewed head commit " + short(r.PR.HeadOid) + ".", nil
}

// convWorktree makes code a detached worktree of repo at head: as it is
// if it is there already, checked out again if it moved.
func convWorktree(ctx context.Context, repo, code, head string) error {
	if _, err := os.Stat(code); err != nil {
		if err := os.MkdirAll(filepath.Dir(code), 0o755); err != nil {
			return err
		}
		_, err := triage.GitCtx(ctx, repo, "worktree", "add", "--detach", code, head)
		return err
	}
	if cur, err := triage.GitCtx(ctx, code, "rev-parse", "HEAD"); err == nil {
		// Put back what StripAgentFiles took, to take it again, and what
		// an agent that builds here wrote (build.go in llm).
		if _, err := triage.GitCtx(ctx, code, "checkout", "--force", "--detach", head); err == nil {
			_, err = triage.GitCtx(ctx, code, "clean", "-fdq")
			return err
		}
		if strings.TrimSpace(cur) == head {
			return errors.New("can't check the worktree out again")
		}
	}
	_, _ = triage.GitCtx(ctx, repo, "worktree", "remove", "--force", code)
	if err := os.RemoveAll(code); err != nil {
		return err
	}
	_, _ = triage.GitCtx(ctx, repo, "worktree", "prune")
	if err := os.MkdirAll(filepath.Dir(code), 0o755); err != nil {
		return err
	}
	_, err := triage.GitCtx(ctx, repo, "worktree", "add", "--detach", code, head)
	return err
}

// bundle writes the material, to the conversation's folder or a temporary
// one; done removes a temporary one.
func (cv *chatConv) bundle(m *chatMaterial) (dir string, done func(), err error) {
	if cv.id == "" {
		dir, err = writeChatBundle(m)
		return dir, func() { os.RemoveAll(dir) }, err
	}
	dir, err = writeChatBundleAt(m, filepath.Join(cv.dir, "material"))
	return dir, func() {}, err
}

// reach lets the agent past its sandbox without writing: on a CLI the web,
// and gh through the app's MCP server (mcpserver.go), in the repository's
// clone; on an API provider the web fetch. The view goes with the reader's turn (chatview.go), so the
// material doesn't follow it and a session isn't sent it again.
func (cv *chatConv) reach(ws *llm.Workspace, l llm.LLMTool, r *PRResult, m *chatMaterial) {
	if ws == nil {
		return
	}
	if llm.SupportsWorkspace(l) {
		cv.builds(cv.c, ws)
		cv.snapshots(ws)
	}
	// An API provider gets the app's fetch and gh tools (llm/webtool.go).
	if !llm.SupportsWorkspace(l) {
		if cv.noWeb {
			return
		}
		ws.Web = true
		if _, err := exec.LookPath("gh"); err == nil && r.PR.URL != "" {
			ws.GH = orDefault(cv.cloneDir(r), ws.Dir)
		}
		return
	}
	m.view = chatView{}
	if cv.noWeb { // Settings: no web, no gh
		return
	}
	ws.Web = true
	exe, err := os.Executable()
	if err != nil {
		return
	}
	var tools []string
	if _, err := exec.LookPath("gh"); err == nil && r.PR.URL != "" {
		tools = append(tools, "gh")
	}
	// Claude Code has its own fetch (WebFetch); codex only searches.
	if _, isCodex := l.(*llm.CodexCLI); isCodex && ws.Web {
		tools = append(tools, "fetch")
	}
	if len(tools) == 0 {
		return
	}
	ws.MCP = []llm.MCPServer{{Name: "prm", Command: exe, Args: []string{"mcp", "-dir", orDefault(cv.cloneDir(r), ws.Dir), "-tools", strings.Join(tools, ",")}, Tools: tools}}
}

// cloneDir is the repository's clone (or the local checkout), where gh
// finds the repo from its remote.
func (cv *chatConv) cloneDir(r *PRResult) string {
	if r.PR.LocalPath != "" {
		return r.PR.LocalPath
	}
	if cv.t.fetcher != nil {
		return cv.t.fetcher.RepoDir(r.PR.PRRef)
	}
	return ""
}

// call answers: all is the system prompt and the conversation as chat
// made it, view goes with the reader's newest turn. A CLI with a session
// that has seen the conversation so far is sent only the turns since.
func (cv *chatConv) call(ctx context.Context, l llm.LLMTool, ws *llm.Workspace, all []llm.ChatMessage, view chatView, maxTokens int32) (map[string]any, llm.Usage, error) {
	system, full := all[0], all[1:]
	cli := ws != nil && llm.SupportsWorkspace(l) && cv.id != ""
	if cli {
		if s := cv.resumable(l, ws); s != nil {
			turns := withView(chatTurns(cv.turns[s.Turns:]), view)
			if len(turns) > 0 && turns[0].Role == "user" {
				ws.Session = &llm.Session{ID: s.Session.ID, System: s.Session.System}
				activity.Printf(ctx, "carrying on session %s with %d new turns", short(s.Session.ID), len(turns))
				args, usage, err := llm.CallToolReading(ctx, l, ws, append([]llm.ChatMessage{system}, turns...), chatTool, maxTokens)
				if err == nil {
					cv.resumed = true
					cv.saveSession(l, ws)
					return args, usage, nil
				}
				if ctx.Err() != nil {
					return nil, usage, err
				}
				activity.Errorf(ctx, "carrying on the session failed (%v); starting a new one", err)
			}
		}
		ws.Session = &llm.Session{}
		cv.freshStart = true
	}
	args, usage, err := llm.CallToolReading(ctx, l, ws, append([]llm.ChatMessage{system}, withView(full, view)...), chatTool, maxTokens)
	if err == nil && cli {
		cv.saveSession(l, ws)
	} else if err == nil {
		cv.at = time.Now().Format(time.RFC3339)
	}
	return args, usage, err
}

// withView adds what is on screen to the newest of the reader's turns.
func withView(msgs []llm.ChatMessage, view chatView) []llm.ChatMessage {
	out := append([]llm.ChatMessage(nil), msgs...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role == "user" {
			out[i].Content += "\n\n" + chatViewText(view)
			break
		}
	}
	return out
}

// resumable is the session to carry on, if it has seen exactly the
// beginning of this conversation, with this model, in this directory.
func (cv *chatConv) resumable(l llm.LLMTool, ws *llm.Workspace) *chatSession {
	s := cv.sess
	if s == nil || s.Session.ID == "" || s.Provider != l.Name() || s.Model != l.ModelID() || s.Dir != ws.Dir {
		return nil
	}
	if s.Turns <= 0 || s.Turns >= len(cv.turns) || turnsHash(cv.turns[:s.Turns]) != s.Prefix {
		return nil
	}
	return s
}

// saveSession records the session after an answer, which the UI keeps
// with the time it is given (cv.at).
func (cv *chatConv) saveSession(l llm.LLMTool, ws *llm.Workspace) {
	cv.at = time.Now().Format(time.RFC3339)
	if ws.Session == nil || ws.Session.ID == "" {
		return
	}
	turns := append(append([]chatTurn(nil), cv.turns...), chatTurn{Role: "assistant", At: cv.at})
	s := chatSession{Provider: l.Name(), Model: l.ModelID(), Dir: ws.Dir, Repo: cv.repo, Session: *ws.Session,
		Turns: len(turns), Prefix: turnsHash(turns), At: time.Now()}
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	if err := os.MkdirAll(cv.t.chatsDir(), 0o755); err == nil {
		_ = os.WriteFile(cv.dir+".session.json", b, 0o600)
	}
}

// answeredAt is when the answer was written, as the UI is to keep it.
func (cv *chatConv) answeredAt() string {
	return orDefault(cv.at, time.Now().Format(time.RFC3339))
}

// dropChat deletes a conversation: its turns, session, worktree and
// material.
func (t *triager) dropChat(id string) error {
	h := convHash(id)
	mu, _ := chatLocks.LoadOrStore(h, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	base := filepath.Join(t.chatsDir(), h)
	var s chatSession
	if b, err := os.ReadFile(base + ".session.json"); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	dropCode(base, s.Repo)
	for _, p := range []string{base + ".json", base + ".session.json"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.RemoveAll(base)
}

// dropCode removes a conversation's worktree.
func dropCode(base, repo string) {
	code := filepath.Join(base, "code")
	if _, err := os.Stat(code); err != nil {
		return
	}
	if repo != "" {
		_, _ = triage.Git(repo, "worktree", "remove", "--force", code)
	}
	_ = os.RemoveAll(code)
	if repo != "" {
		_, _ = triage.Git(repo, "worktree", "prune")
	}
}

var chatPruned struct {
	sync.Mutex
	at time.Time
}

// pruneChats, at most hourly, deletes conversations untouched for a month
// and the worktrees of those untouched for a week.
func (t *triager) pruneChats() {
	chatPruned.Lock()
	if time.Since(chatPruned.at) < chatPruneEvery {
		chatPruned.Unlock()
		return
	}
	chatPruned.at = time.Now()
	chatPruned.Unlock()
	es, err := os.ReadDir(t.chatsDir())
	if err != nil {
		return
	}
	for _, e := range es {
		name, ok := strings.CutSuffix(e.Name(), ".session.json")
		if !ok {
			continue
		}
		base := filepath.Join(t.chatsDir(), name)
		var s chatSession
		if b, err := os.ReadFile(base + ".session.json"); err == nil {
			_ = json.Unmarshal(b, &s)
		}
		conv, err := os.Stat(base + ".json")
		if err != nil || time.Since(conv.ModTime()) > chatConvKeep {
			dropCode(base, s.Repo)
			os.Remove(base + ".json")
			os.Remove(base + ".session.json")
			os.RemoveAll(base)
			continue
		}
		if code, err := os.Stat(filepath.Join(base, "code")); err == nil && time.Since(code.ModTime()) > chatCodeKeep {
			dropCode(base, s.Repo)
		}
	}
}

// chatConvRoutes are the conversations' store:
//
//	GET /api/chats?change=X   the conversation, {"messages": [...]}
//	PUT /api/chats?change=X   save it; with no messages, delete it
//	GET /api/chat-runs/{id}   the work log of an answer being written
func (t *triager) chatConvRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/chats", func(w http.ResponseWriter, r *http.Request) {
		change := r.URL.Query().Get("change")
		if change == "" {
			writeErr(w, 400, errors.New("no change"))
			return
		}
		b, err := os.ReadFile(filepath.Join(t.chatsDir(), convHash(change)+".json"))
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, 200, map[string]any{"messages": []any{}})
			return
		}
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	})
	mux.HandleFunc("PUT /api/chats", func(w http.ResponseWriter, r *http.Request) {
		change := r.URL.Query().Get("change")
		var in struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, chatConvMax)).Decode(&in); err != nil || change == "" {
			writeErr(w, 400, errors.Join(err, errors.New("a change and its messages")))
			return
		}
		if len(in.Messages) == 0 {
			if err := t.dropChat(change); err != nil {
				writeErr(w, 500, err)
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
		b, err := json.Marshal(map[string]any{"change": change, "messages": in.Messages})
		if err == nil {
			err = os.MkdirAll(t.chatsDir(), 0o755)
		}
		if err == nil {
			p := filepath.Join(t.chatsDir(), convHash(change)+".json")
			tmp := p + ".tmp"
			if err = os.WriteFile(tmp, b, 0o600); err == nil {
				err = os.Rename(tmp, p)
			}
		}
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/chats/snapshot", func(w http.ResponseWriter, r *http.Request) {
		change := r.URL.Query().Get("change")
		var in struct {
			PNG string `json:"png"` // a data URL
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, chatSnapshotMax)).Decode(&in); err != nil || change == "" {
			writeErr(w, 400, errors.Join(err, errors.New("a change and a picture")))
			return
		}
		p, err := t.saveSnapshot(change, in.PNG)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]any{"path": p})
	})
	mux.HandleFunc("GET /api/chat-runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, ok := chatRuns.Load(r.PathValue("id"))
		if !ok {
			writeJSON(w, 200, map[string]any{"entries": []any{}, "done": true})
			return
		}
		run := v.(*chatRun)
		writeJSON(w, 200, map[string]any{"entries": run.entries(), "done": run.isDone(), "started": run.started})
	})
}
