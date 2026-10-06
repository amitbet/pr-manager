package main

import (
	"context"
	"testing"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

// watchFixture saves results for PRs 1 (two heads) and 2 open, 3 merged
// and 4 closed in one repo, and returns a watch over them on a fake clock
// whose gh answers come from states.
func watchFixture(t *testing.T) (*prWatch, map[int]string, *time.Time, *int) {
	t.Helper()
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	save := func(key string, n int, state string, at time.Time) {
		r := &PRResult{Key: key, CreatedAt: at, PR: &triage.PRInfo{PRRef: ref(n), State: state}}
		if err := tr.saveResult(r); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	save("o__r__1__old", 1, "OPEN", t0)
	save("o__r__1__new", 1, "OPEN", t0.Add(time.Hour))
	save("o__r__2", 2, "OPEN", t0)
	save("o__r__3", 3, "MERGED", t0)
	save("o__r__4", 4, "CLOSED", t0)
	states := map[int]string{1: "OPEN", 2: "OPEN", 3: "MERGED", 4: "CLOSED"}
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

func ref(n int) triage.PRRef { return triage.PRRef{Owner: "o", Repo: "r", Number: n} }

func shown(states map[int]string, ns ...int) []shownPR {
	var out []shownPR
	for _, n := range ns {
		out = append(out, shownPR{PRRef: ref(n), State: states[n]})
	}
	return out
}

// dueNow is the PR due now, or 0.
func dueNow(w *prWatch) int {
	p, wait := w.due()
	if p == nil || wait > 0 {
		return 0
	}
	return p.ref.Number
}

func TestPRWatchAsksOnlyAboutShownPRs(t *testing.T) {
	w, states, now, _ := watchFixture(t)
	if p, _ := w.due(); p != nil {
		t.Fatal("something is due before the UI showed anything")
	}
	// PR 2 is under "Show N more"; merged 3 is listed.
	w.show(shown(states, 1, 3))
	if n := dueNow(w); n != 1 {
		t.Fatalf("due %d, want the shown open PR 1", n)
	}
	w.check(context.Background(), w.prs[prID(ref(1))])
	if p, _ := w.due(); p == nil || p.ref.Number != 1 {
		t.Fatalf("due %v, want only PR 1, later: merged and hidden PRs aren't asked about", p)
	}

	// "Show 1 more": PR 2 is asked about at once.
	w.show(shown(states, 1, 2, 3))
	if n := dueNow(w); n != 2 {
		t.Fatalf("due %d after PR 2 was shown, want 2", n)
	}
	w.check(context.Background(), w.prs[prID(ref(2))])

	// Hidden again and shown again within a minute of its ask: not asked.
	w.show(shown(states, 1))
	w.show(shown(states, 1, 2))
	if n := dueNow(w); n != 0 {
		t.Errorf("due %d, want nothing: PR 2 was just asked about", n)
	}
	// Hidden, it isn't asked about when its turn comes.
	w.show(shown(states, 1))
	*now = now.Add(watchFirst)
	if n := dueNow(w); n != 1 {
		t.Errorf("due %d, want PR 1 only", n)
	}
}

func TestPRWatchBacksOffAndSavesMerge(t *testing.T) {
	w, states, now, calls := watchFixture(t)
	w.show(shown(states, 1, 2))
	p := w.prs[prID(ref(1))]
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
	// The UI hasn't reloaded and still shows it open: it stays merged.
	*now = now.Add(time.Hour)
	w.show(shown(map[int]string{1: "OPEN", 2: "OPEN"}, 1, 2))
	for range 3 {
		if n := dueNow(w); n == 1 {
			t.Fatal("a PR found merged is asked about again")
		} else if n == 2 {
			w.check(context.Background(), w.prs[prID(ref(2))])
		}
	}

	// An unchanged state saves nothing and leaves the generation.
	before := *calls
	w.check(context.Background(), w.prs[prID(ref(2))])
	if *calls != before+1 || w.generation() != 1 {
		t.Errorf("calls %d generation %d", *calls-before, w.generation())
	}
}

func TestPRWatchTouch(t *testing.T) {
	w, states, now, _ := watchFixture(t)
	w.show(shown(states, 1, 2, 4))
	p := w.prs[prID(ref(2))]
	for range 3 {
		w.check(context.Background(), p)
	}
	w.check(context.Background(), w.prs[prID(ref(1))])

	// Opening closed PR 4 asks about it once (it may have been reopened)
	// and starts the backoff of the repo's open PRs on screen over.
	*now = now.Add(2 * watchTouch)
	w.touch(ref(4), "CLOSED")
	if p.wait != watchFirst || !p.next.Equal(*now) {
		t.Errorf("touched repo's PR 2 waits %v, due %v; want due now", p.wait, p.next)
	}
	c := w.prs[prID(ref(4))]
	if c == nil || !c.once {
		t.Fatal("opening a closed PR doesn't ask about it")
	}
	w.check(context.Background(), c)
	*now = now.Add(watchMax)
	if p, _ := w.due(); p != nil && p.ref.Number == 4 {
		t.Error("a closed PR is asked about again after its one ask")
	}

	// Opening merged PR 3 doesn't ask about it.
	*now = now.Add(2 * watchTouch)
	w.touch(ref(3), "MERGED")
	if w.prs[prID(ref(3))].once {
		t.Error("opening a merged PR asks about it")
	}

	// Within watchTouch the repo isn't touched again.
	w.check(context.Background(), p)
	w.touch(ref(1), "OPEN")
	if p.wait != 2*watchFirst {
		t.Errorf("a second touch within %v restarted the backoff", watchTouch)
	}
}
