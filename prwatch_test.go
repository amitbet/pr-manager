package main

import (
	"context"
	"testing"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

// watchFixture saves results for PRs 1 (two heads) and 2 open and 3
// merged in one repo, and returns a watch over them on a fake clock whose
// gh answers come from states.
func watchFixture(t *testing.T) (*prWatch, map[int]string, *time.Time, *int) {
	t.Helper()
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	save := func(key string, n int, state string, at time.Time) {
		r := &PRResult{Key: key, CreatedAt: at, PR: &triage.PRInfo{PRRef: triage.PRRef{Owner: "o", Repo: "r", Number: n}, State: state}}
		if err := tr.saveResult(r); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	save("o__r__1__old", 1, "OPEN", t0)
	save("o__r__1__new", 1, "OPEN", t0.Add(time.Hour))
	save("o__r__2", 2, "OPEN", t0)
	save("o__r__3", 3, "MERGED", t0)
	states := map[int]string{1: "OPEN", 2: "OPEN", 3: "MERGED"}
	now := t0
	calls := 0
	w := newPRWatch(tr)
	w.now = func() time.Time { return now }
	w.resolve = func(_ context.Context, ref triage.PRRef) (*triage.PRInfo, error) {
		calls++
		return &triage.PRInfo{PRRef: ref, State: states[ref.Number]}, nil
	}
	return w, states, &now, &calls
}

func TestPRWatchBacksOffAndStopsOnMerge(t *testing.T) {
	w, states, now, calls := watchFixture(t)
	w.sync("", "")
	if len(w.prs) != 2 {
		t.Fatalf("watching %d PRs, want the 2 open ones", len(w.prs))
	}
	p := w.prs["github.com/o/r#1"]
	w.check(context.Background(), p)
	if got := p.next.Sub(*now); got != watchFirst {
		t.Errorf("next ask in %v, want %v", got, watchFirst)
	}
	w.check(context.Background(), p)
	if p.wait != 4*watchFirst {
		t.Errorf("wait %v after two asks, want it doubled twice", p.wait)
	}
	for range 10 {
		w.check(context.Background(), p)
	}
	if p.wait != watchMax {
		t.Errorf("wait %v, want capped at %v", p.wait, watchMax)
	}

	states[1] = "MERGED"
	w.check(context.Background(), p)
	if _, ok := w.prs["github.com/o/r#1"]; ok {
		t.Error("a merged PR is still watched")
	}
	if w.generation() != 1 {
		t.Errorf("generation %d, want 1 after a state changed", w.generation())
	}
	for _, key := range []string{"o__r__1__old", "o__r__1__new"} {
		r, err := w.t.Load(key)
		if err != nil {
			t.Fatal(err)
		}
		if r.PR.State != "MERGED" {
			t.Errorf("%s saved %s, want every result of the PR merged", key, r.PR.State)
		}
	}

	// An unchanged state saves nothing and leaves the generation.
	before := *calls
	w.check(context.Background(), w.prs["github.com/o/r#2"])
	if *calls != before+1 || w.generation() != 1 {
		t.Errorf("calls %d generation %d", *calls-before, w.generation())
	}
}

func TestPRWatchTouchRestartsBackoffOncePerMinute(t *testing.T) {
	w, _, now, _ := watchFixture(t)
	w.sync("", "")
	p := w.prs["github.com/o/r#2"]
	for range 3 {
		w.check(context.Background(), p)
	}
	ref := triage.PRRef{Owner: "o", Repo: "r", Number: 3}
	w.touch(ref)
	waitFor(t, func() bool { w.mu.Lock(); defer w.mu.Unlock(); return p.wait == watchFirst })
	if !p.next.Equal(*now) {
		t.Errorf("touched repo's PR due at %v, want now", p.next)
	}
	if _, ok := w.prs["github.com/o/r#3"]; ok {
		t.Error("touching a merged PR watches it")
	}

	w.check(context.Background(), p)
	w.touch(ref) // within watchTouch: ignored
	time.Sleep(20 * time.Millisecond)
	w.mu.Lock()
	wait := p.wait
	w.mu.Unlock()
	if wait != 2*watchFirst {
		t.Errorf("a second touch within %v restarted the backoff", watchTouch)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 200 {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out")
}
