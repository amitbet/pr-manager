// Package activity records what a job is doing, so the UI can show it while
// the job runs: one thread per command or model call (gh, git, a unit's
// classify or review), each with its own log lines.
//
// The log travels in the context. Without one every call is a no-op, so
// the CLI paths pay nothing.
package activity

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	maxLines    = 400       // per thread; older lines are dropped
	maxLineLen  = 2000      // longer lines are cut
	maxDataLen  = 256 << 10 // larger attached data is dropped
	maxThreads  = 2000      // per log; older finished threads are dropped
	statusRun   = "running"
	statusDone  = "done"
	statusError = "error"
)

type Line struct {
	T    time.Time `json:"t"`
	Text string    `json:"text"`
	// Data is a structured value shown under the line, such as a model's
	// output, which the UI lays out instead of printing as one long line.
	Data json.RawMessage `json:"data,omitempty"`
}

type Thread struct {
	ID     int       `json:"id"`
	Kind   string    `json:"kind"` // gh | git | llm | stage | …
	Name   string    `json:"name"`
	Status string    `json:"status"` // running | done | error
	Start  time.Time `json:"start"`
	End    time.Time `json:"end,omitzero"`
	// Dropped counts lines cut from the front to keep the thread small.
	Dropped int    `json:"dropped,omitempty"`
	Lines   []Line `json:"lines"`

	log    *Log
	failed bool
}

// Log is the activity of one job.
type Log struct {
	mu      sync.Mutex
	next    int
	threads []*Thread
}

func New() *Log { return &Log{} }

type logKey struct{}
type threadKey struct{}

// With returns ctx carrying l.
func With(ctx context.Context, l *Log) context.Context {
	return context.WithValue(ctx, logKey{}, l)
}

func from(ctx context.Context) *Log {
	l, _ := ctx.Value(logKey{}).(*Log)
	return l
}

// Start opens a thread in ctx's log. Printf on the returned context writes
// to it; Finish closes it. Without a log both are no-ops.
func Start(ctx context.Context, kind, format string, args ...any) (context.Context, *Thread) {
	l := from(ctx)
	if l == nil {
		return ctx, nil
	}
	l.mu.Lock()
	l.next++
	t := &Thread{ID: l.next, Kind: kind, Name: clip(fmt.Sprintf(format, args...)), Status: statusRun, Start: time.Now(), log: l}
	l.threads = append(l.threads, t)
	if len(l.threads) > maxThreads {
		for i, old := range l.threads {
			if old.Status != statusRun {
				l.threads = append(l.threads[:i], l.threads[i+1:]...)
				break
			}
		}
	}
	l.mu.Unlock()
	return context.WithValue(ctx, threadKey{}, t), t
}

// Printf adds a line to ctx's thread (to a "job" thread when there is none).
func Printf(ctx context.Context, format string, args ...any) {
	t, _ := ctx.Value(threadKey{}).(*Thread)
	if t == nil {
		l := from(ctx)
		if l == nil {
			return
		}
		t = l.jobThread()
	}
	t.Printf(format, args...)
}

// PrintData is Printf with data attached to the line. Data too large to
// keep is appended to the text as JSON, cut like any long line.
func PrintData(ctx context.Context, data any, format string, args ...any) {
	t, _ := ctx.Value(threadKey{}).(*Thread)
	if t == nil {
		l := from(ctx)
		if l == nil {
			return
		}
		t = l.jobThread()
	}
	b, err := json.Marshal(data)
	if err != nil || len(b) > maxDataLen {
		t.Printf("%s: %s", fmt.Sprintf(format, args...), b)
		return
	}
	t.log.mu.Lock()
	defer t.log.mu.Unlock()
	t.add(Line{T: time.Now(), Text: clip(fmt.Sprintf(format, args...)), Data: b})
}

// Errorf is Printf for a failure: the thread ends as failed.
func Errorf(ctx context.Context, format string, args ...any) {
	if t, _ := ctx.Value(threadKey{}).(*Thread); t != nil {
		t.log.mu.Lock()
		t.failed = true
		t.log.mu.Unlock()
	}
	Printf(ctx, format, args...)
}

// Close logs the job's outcome and ends its job thread.
func (l *Log) Close(err error) {
	t := l.jobThread()
	if err != nil {
		t.Printf("failed: %v", err)
	} else {
		t.Printf("finished")
	}
	t.Finish(err)
}

// jobThread is the thread for lines logged outside any other thread.
func (l *Log) jobThread() *Thread {
	l.mu.Lock()
	for _, t := range l.threads {
		if t.Kind == "job" {
			l.mu.Unlock()
			return t
		}
	}
	l.mu.Unlock()
	_, t := Start(With(context.Background(), l), "job", "job")
	return t
}

func (t *Thread) Printf(format string, args ...any) {
	if t == nil {
		return
	}
	now := time.Now()
	t.log.mu.Lock()
	defer t.log.mu.Unlock()
	for _, s := range strings.Split(strings.TrimRight(fmt.Sprintf(format, args...), "\n"), "\n") {
		t.add(Line{T: now, Text: clip(s)})
	}
}

// add appends l, dropping the oldest lines past maxLines. The log's lock
// is held.
func (t *Thread) add(l Line) {
	t.Lines = append(t.Lines, l)
	if n := len(t.Lines) - maxLines; n > 0 {
		t.Lines = append(t.Lines[:0:0], t.Lines[n:]...)
		t.Dropped += n
	}
}

// Finish closes the thread, as failed when err is set.
func (t *Thread) Finish(err error) {
	if t == nil {
		return
	}
	if err != nil {
		t.Printf("error: %v", err)
	}
	t.log.mu.Lock()
	defer t.log.mu.Unlock()
	t.Status, t.End = statusDone, time.Now()
	if err != nil || t.failed {
		t.Status = statusError
	}
}

// Command logs a command about to run in its own thread. Call the
// returned func with the command's error when it finishes.
func Command(ctx context.Context, dir, name string, args ...string) (context.Context, func(err error)) {
	ctx, t := Start(ctx, name, "%s %s", name, strings.Join(args, " "))
	if t == nil {
		return ctx, func(error) {}
	}
	if dir != "" {
		t.Printf("in %s", dir)
	}
	start := time.Now()
	return ctx, func(err error) {
		if err == nil {
			t.Printf("ok in %s", time.Since(start).Round(time.Millisecond))
		}
		t.Finish(err)
	}
}

// Writer returns ctx's thread as an io.Writer (see Thread.Writer).
func Writer(ctx context.Context, prefix string) *LineWriter {
	t, _ := ctx.Value(threadKey{}).(*Thread)
	return t.Writer(prefix)
}

// Writer returns an io.Writer that logs each line written to it to t,
// prefixed with prefix. Partial lines are held until the newline or Flush.
func (t *Thread) Writer(prefix string) *LineWriter {
	return &LineWriter{t: t, prefix: prefix}
}

type LineWriter struct {
	t      *Thread
	prefix string
	mu     sync.Mutex
	buf    []byte
	// Format, if set, rewrites each line; "" drops it.
	Format func(string) string
}

func (w *LineWriter) Write(p []byte) (int, error) {
	if w.t == nil {
		return len(p), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		w.emit(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// Flush logs a trailing partial line.
func (w *LineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emit(string(w.buf))
		w.buf = nil
	}
}

func (w *LineWriter) emit(s string) {
	s = strings.TrimRight(s, "\r")
	if w.Format != nil {
		s = w.Format(s)
	}
	if strings.TrimSpace(s) == "" {
		return
	}
	w.t.Printf("%s%s", w.prefix, s)
}

// Snapshot copies the threads for the UI.
func (l *Log) Snapshot() []Thread {
	if l == nil {
		return []Thread{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Thread, len(l.threads))
	for i, t := range l.threads {
		out[i] = *t
		out[i].Lines = append([]Line{}, t.Lines...)
		out[i].log = nil
	}
	return out
}

func clip(s string) string {
	if len(s) > maxLineLen {
		return s[:maxLineLen] + "…"
	}
	return s
}
