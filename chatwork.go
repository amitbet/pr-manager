package main

import (
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
)

// workEntries reads a run's activity as steps, oldest first. An error
// line marks the step before it.
func workEntries(threads []activity.Thread) []workEntry {
	var lines []activity.Line
	for _, th := range threads {
		lines = append(lines, th.Lines...)
	}
	sort.SliceStable(lines, func(a, b int) bool { return lines[a].T.Before(lines[b].T) })
	var out []workEntry
	for _, l := range lines {
		text := strings.TrimRight(l.Text, " \n")
		if text == "" || workNoise.MatchString(text) {
			continue
		}
		e := workEntry{T: l.T}
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
