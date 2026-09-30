package activity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestThreads(t *testing.T) {
	l := New()
	ctx := With(context.Background(), l)
	Printf(ctx, "started")

	cctx, done := Command(ctx, "/repo", "git", "fetch", "origin")
	done(nil)
	uctx, th := Start(ctx, "llm", "classify %s", "a.go")
	Printf(uctx, "→ call")
	Errorf(uctx, "✗ timeout")
	th.Finish(nil)
	_, fail := Command(ctx, "", "gh", "pr", "view")
	fail(errors.New("exit 1"))
	_ = cctx

	got := l.Snapshot()
	if len(got) != 4 {
		t.Fatalf("threads = %d, want 4", len(got))
	}
	want := []struct{ kind, name, status string }{
		{"job", "job", "running"},
		{"git", "git fetch origin", "done"},
		{"llm", "classify a.go", "error"}, // Errorf marks it failed
		{"gh", "gh pr view", "error"},
	}
	for i, w := range want {
		g := got[i]
		if g.Kind != w.kind || g.Name != w.name || g.Status != w.status {
			t.Errorf("thread %d = %s %q %s, want %s %q %s", i, g.Kind, g.Name, g.Status, w.kind, w.name, w.status)
		}
	}
	if got[0].Lines[0].Text != "started" || got[1].Lines[0].Text != "in /repo" {
		t.Errorf("lines: %+v %+v", got[0].Lines, got[1].Lines)
	}
	if last := got[3].Lines[len(got[3].Lines)-1].Text; last != "error: exit 1" {
		t.Errorf("gh last line = %q", last)
	}
}

func TestNoLog(t *testing.T) {
	ctx, th := Start(context.Background(), "llm", "x")
	Printf(ctx, "dropped")
	th.Printf("dropped")
	th.Finish(nil)
	_, _ = Writer(ctx, "").Write([]byte("dropped\n"))
}

func TestWriterAndCap(t *testing.T) {
	l := New()
	ctx, th := Start(With(context.Background(), l), "llm", "x")
	w := Writer(ctx, "err: ")
	w.Format = func(s string) string { return strings.ToUpper(s) }
	_, _ = w.Write([]byte("one\ntw"))
	_, _ = w.Write([]byte("o\n\nthree"))
	w.Flush()
	if got := texts(l.Snapshot()[0]); got != "err: ONE|err: TWO|err: THREE" {
		t.Errorf("lines = %s", got)
	}
	for i := range maxLines + 10 {
		th.Printf("%d", i)
	}
	s := l.Snapshot()[0]
	if len(s.Lines) != maxLines || s.Dropped != 13 || s.Lines[len(s.Lines)-1].Text != fmt.Sprint(maxLines+9) {
		t.Errorf("lines = %d, dropped = %d", len(s.Lines), s.Dropped)
	}
}

func texts(t Thread) string {
	var s []string
	for _, l := range t.Lines {
		s = append(s, l.Text)
	}
	return strings.Join(s, "|")
}

func TestWriterPendingCap(t *testing.T) {
	l := New()
	ctx, _ := Start(With(context.Background(), l), "llm", "x")
	w := Writer(ctx, "")
	chunk := []byte(strings.Repeat("x", 64<<10))
	for range (maxPendingLen / len(chunk)) + 1 {
		_, _ = w.Write(chunk)
	}
	if n := len(l.Snapshot()[0].Lines); n != 1 {
		t.Fatalf("lines = %d, want the oversized pending line flushed once", n)
	}
	if len(w.buf) >= maxPendingLen {
		t.Errorf("buf = %d bytes, want it flushed", len(w.buf))
	}
}
