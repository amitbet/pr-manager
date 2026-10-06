package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/internal/activity"
)

// While the chat agent works, the UI shows what it does, one line per
// step (after t3code's work log): what it thinks, the files it reads, the
// searches and commands it runs, its gh and web calls. The steps are the
// lines the CLIs' stream formatters write to the activity log
// (claudeStream, codexEvent in llm/cli.go), read back as entries. The UI
// polls GET /api/chat-runs/{id} for them while it waits, and keeps them
// with the answer.

// workEntry is one step: K is its kind (think, read, search, run, web,
// note, error), X its one line.
type workEntry struct {
	T   time.Time `json:"t"`
	K   string    `json:"k"`
	X   string    `json:"x"`
	Err bool      `json:"err,omitempty"`
}

type chatRun struct {
	id      string
	log     *activity.Log
	thread  *activity.Thread
	started time.Time
	mu      sync.Mutex
	done    bool
}

var chatRuns sync.Map // run id -> *chatRun

func (t *triager) startChatRun(id string) *chatRun {
	r := &chatRun{id: id, log: activity.New(), started: time.Now()}
	chatRuns.Store(id, r)
	time.AfterFunc(15*time.Minute, func() { chatRuns.CompareAndDelete(id, r) })
	return r
}

func (r *chatRun) finish() {
	r.thread.Finish(nil)
	r.mu.Lock()
	r.done = true
	r.mu.Unlock()
	time.AfterFunc(chatRunKeep, func() { chatRuns.CompareAndDelete(r.id, r) })
}

func (r *chatRun) isDone() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

func (r *chatRun) entries() []workEntry {
	if r == nil {
		return nil
	}
	return workEntries(r.log.Snapshot())
}

var (
	// The app's own lines around a model call, and the CLI's command line.
	workNoise = regexp.MustCompile(`^(→ \S+/\S+ \w+: \d+ prompt chars|← \w+ in |✗ \w+ after |\$ \S*(claude|codex)(\.cmd|\.exe)? (-p|exec) |stderr: |carrying on session )`)
	shellWrap = regexp.MustCompile(`^\S*/(ba|z)?sh -l?c (.*)$`)
	// An API provider's tool call, as the tool loop logs it (llm/agent.go):
	// "  read_file {"path":"a.go"}: 1200 chars", or its error after the colon.
	apiCall = regexp.MustCompile(`^  (read_file|list_dir|glob|grep|git|fetch|gh) (\{.*?\}|\{.*…): (.*)$`)
	apiDone = regexp.MustCompile(`^\d+ chars$`)
)

// apiEntry reads an API provider's tool call line as a step.
func apiEntry(text string) (workEntry, bool) {
	m := apiCall.FindStringSubmatch(text)
	if m == nil {
		return workEntry{}, false
	}
	var a struct {
		Path, Pattern, Glob, URL string
		Args                     []string
	}
	_ = json.Unmarshal([]byte(m[2]), &a)
	in := ""
	if a.Path != "" && a.Path != "." {
		in = " in " + a.Path
	}
	var x, k string
	switch m[1] {
	case "read_file":
		x, k = "Read "+a.Path, "read"
	case "list_dir":
		x, k = "List "+orDefault(a.Path, "."), "read"
	case "glob":
		x, k = fmt.Sprintf("Glob %q%s", a.Pattern, in), "search"
	case "grep":
		x, k = fmt.Sprintf("Grep %q%s", a.Pattern, in), "search"
		if a.Glob != "" {
			x += " --glob " + a.Glob
		}
	case "git":
		x, k = "git "+strings.Join(a.Args, " "), "run"
	case "fetch":
		x, k = "Fetch "+a.URL, "web"
	case "gh":
		x, k = "gh "+strings.Join(a.Args, " "), "web"
	}
	if strings.TrimSpace(strings.TrimPrefix(x, strings.SplitN(x, " ", 2)[0])) == "" {
		x = m[1] + " " + m[2]
	}
	return workEntry{K: k, X: x, Err: !apiDone.MatchString(m[3])}, true
}

// workEntries reads a run's activity as steps, oldest first. An error
// line marks the step before it.
func workEntries(threads []activity.Thread) []workEntry {
	var lines []activity.Line
	for _, th := range threads {
		// The agent's own threads: not the git and go commands the app
		// runs to set its workspace up.
		if th.Kind == "llm" || th.Kind == "agent" || th.Kind == "job" {
			lines = append(lines, th.Lines...)
		}
	}
	sort.SliceStable(lines, func(a, b int) bool { return lines[a].T.Before(lines[b].T) })
	var out []workEntry
	for _, l := range lines {
		text := strings.TrimRight(l.Text, " \n")
		if text == "" || workNoise.MatchString(text) {
			continue
		}
		e := workEntry{T: l.T}
		if a, ok := apiEntry(l.Text); ok {
			a.T, a.X = l.T, clipText(a.X, 600)
			out = append(out, a)
			continue
		}
		switch trimmed := strings.TrimSpace(text); {
		case strings.HasPrefix(trimmed, "error:") || strings.HasPrefix(trimmed, "exit ") || strings.HasPrefix(trimmed, "✗"):
			if n := len(out); n > 0 && !out[n-1].Err && out[n-1].K != "think" && out[n-1].K != "note" {
				out[n-1].Err = true
				continue
			}
			e.K, e.X, e.Err = "error", trimmed, true
		case strings.HasPrefix(text, "… "):
			e.K, e.X = "progress", strings.TrimPrefix(text, "… ")
		case strings.HasPrefix(text, "thinking: "):
			e.K, e.X = "think", strings.TrimPrefix(text, "thinking: ")
		case strings.HasPrefix(text, "$ "):
			e.K, e.X = "run", unwrapShell(strings.TrimPrefix(text, "$ "))
		case strings.HasPrefix(text, "→ "):
			e.X = strings.TrimPrefix(text, "→ ")
			e.K = toolKind(e.X)
		case strings.HasPrefix(text, "← subagent"):
			e.K, e.X = "note", strings.TrimPrefix(text, "← ")
		default:
			e.K, e.X = "note", text
		}
		e.X = clipText(e.X, 600)
		// Progress replaces the progress before it.
		if n := len(out); e.K == "progress" && n > 0 && out[n-1].K == "progress" {
			out[n-1] = e
			continue
		}
		out = append(out, e)
	}
	if len(out) > chatWorkEntries {
		out = out[len(out)-chatWorkEntries:]
	}
	return out
}

func toolKind(call string) string {
	name, _, _ := strings.Cut(call, " ")
	switch name {
	case "Read", "read_file", "list_dir", "LS":
		return "read"
	case "Grep", "Glob", "grep", "glob":
		return "search"
	case "Fetch", "WebSearch", "WebFetch", "gh":
		return "web"
	case "$", "git":
		return "run"
	}
	return "tool"
}

// unwrapShell shows the script of a `zsh -lc '…'`, as codex runs every
// command.
func unwrapShell(cmd string) string {
	m := shellWrap.FindStringSubmatch(cmd)
	if m == nil {
		return cmd
	}
	s := strings.TrimSpace(m[2])
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	return s
}
