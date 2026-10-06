package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

// The watch's timing. A PR still open is asked about again after
// watchFirst, then twice as long each time it is still open, up to
// watchMax. Every gh call, whichever PR it is for, waits watchGap after
// the one before it, so a cache of a hundred PRs is spread over a few
// minutes instead of asked about at once.
const (
	watchFirst = 2 * time.Minute
	watchMax   = time.Hour
	watchGap   = 2 * time.Second
	// watchTouch is how soon the same repo can be touched again: opening
	// its PRs one after another checks the repo once. A PR shown again
	// within it of its last ask isn't asked again either, so folding and
	// unfolding a list doesn't call gh each time.
	watchTouch = time.Minute
)

// prWatch keeps the state of the saved PRs (open, merged, closed) current,
// so a PR merged on GitHub stops showing as open in the sidebar. It only
// asks gh about the PRs on screen: the UI reports the ones it lists (not
// those under "Show N more", in a folded repo or hidden as untouched) and
// the one open, and a PR is asked about when it comes into view. Opening
// a result asks again about the open PRs on screen in its repo. A PR
// found merged or closed is no longer asked about.
type prWatch struct {
	t       *triager
	resolve func(context.Context, triage.PRRef) (*triage.PRInfo, error)
	now     func() time.Time
	wake    chan struct{}

	mu      sync.Mutex
	prs     map[string]*watchedPR // by PR identity (prID)
	touched map[string]time.Time  // repo -> when it was last touched
	gen     int                   // bumped when a saved state changes
}

type watchedPR struct {
	ref     triage.PRRef
	state   string
	visible bool      // on screen, as the UI last reported
	once    bool      // ask once though not open: a closed PR opened
	checked time.Time // last asked about
	next    time.Time
	wait    time.Duration
}

// shownPR is a PR the UI has on screen, with the state it shows.
type shownPR struct {
	triage.PRRef
	State string `json:"state"`
}

func prID(ref triage.PRRef) string { return ref.RepoArg() + "#" + strconv.Itoa(ref.Number) }

func isOpen(state string) bool { return strings.EqualFold(state, "open") }

func newPRWatch(t *triager) *prWatch {
	return &prWatch{
		t: t, resolve: t.fetcher.Resolve, now: time.Now,
		wake: make(chan struct{}, 1),
		prs:  map[string]*watchedPR{}, touched: map[string]time.Time{},
	}
}

// start runs the watch until the triager shuts down. Nothing is asked
// about until the UI reports what it shows.
func (w *prWatch) start() {
	done := w.t.track()
	if done == nil {
		return
	}
	go func() {
		defer done()
		w.run(w.t.root)
	}()
}

// show sets the PRs on screen. One shown as open that wasn't on screen is
// due now, unless it was asked about in the last watchTouch; one no longer
// on screen waits until it is shown again. A PR the watch already knows
// keeps the state it found, which the UI may not have reloaded yet.
func (w *prWatch) show(prs []shownPR) {
	now := w.now()
	on := map[string]bool{}
	w.mu.Lock()
	for _, s := range prs {
		if s.Number == 0 {
			continue
		}
		id := prID(s.PRRef)
		on[id] = true
		p := w.prs[id]
		if p == nil {
			if !isOpen(s.State) {
				continue
			}
			p = &watchedPR{ref: s.PRRef, state: s.State}
			w.prs[id] = p
		}
		if !p.visible && now.Sub(p.checked) >= watchTouch {
			p.next, p.wait = now, watchFirst
		}
		p.visible = true
	}
	for id, p := range w.prs {
		if !on[id] {
			p.visible = false
		}
	}
	w.mu.Unlock()
	w.poke()
}

// touch is a result of ref being opened, ref's state as saved: the open
// PRs on screen in its repo are asked about again, with their backoff
// started over, and so is ref unless it is merged (a closed PR can be
// reopened). A repo touched in the last watchTouch is left.
func (w *prWatch) touch(ref triage.PRRef, state string) {
	if ref.Number == 0 {
		return
	}
	repo := ref.RepoArg()
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	defer w.poke()
	// The result being opened is on screen, and a new one is due now.
	id := prID(ref)
	if w.prs[id] == nil {
		w.prs[id] = &watchedPR{ref: ref, state: state}
	}
	p := w.prs[id]
	p.visible = true
	if !isOpen(p.state) && !strings.EqualFold(p.state, "merged") && now.Sub(p.checked) >= watchTouch {
		p.once, p.next = true, now
	}
	if last, ok := w.touched[repo]; ok && now.Sub(last) < watchTouch {
		return
	}
	w.touched[repo] = now
	for _, p := range w.prs {
		if p.visible && p.ref.RepoArg() == repo {
			p.next, p.wait = now, watchFirst
		}
	}
}

func (w *prWatch) poke() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// due returns the PR on screen that is due first and how long until it
// is due (zero if it is), or nil when none is.
func (w *prWatch) due() (*watchedPR, time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var first *watchedPR
	for _, p := range w.prs {
		if p.visible && (isOpen(p.state) || p.once) && (first == nil || p.next.Before(first.next)) {
			first = p
		}
	}
	if first == nil {
		return nil, 0
	}
	return first, max(first.next.Sub(w.now()), 0)
}

func (w *prWatch) run(ctx context.Context) {
	for {
		p, wait := w.due()
		if p != nil && wait == 0 {
			w.check(ctx, p)
			// The gap follows every call, whatever a touch made due.
			select {
			case <-ctx.Done():
				return
			case <-time.After(watchGap):
			}
			continue
		}
		var timer <-chan time.Time
		if p != nil {
			timer = time.After(wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-timer:
		}
	}
}

// check asks gh about one PR, saves a changed state on every result of
// it, and schedules the next ask. A PR no longer open is kept, with its
// state, so a UI that hasn't reloaded its list doesn't bring it back.
// Only run calls it, so p.state is not written anywhere else meanwhile.
func (w *prWatch) check(ctx context.Context, p *watchedPR) {
	info, err := w.resolve(ctx, p.ref)
	if err != nil && ctx.Err() == nil {
		log.Printf("pr watch %s: %v", prID(p.ref), err)
	}
	changed := err == nil && info.State != p.state && w.save(p.ref, info)
	w.mu.Lock()
	defer w.mu.Unlock()
	p.checked, p.once = w.now(), false
	if changed {
		w.gen++
	}
	if err == nil {
		p.state = info.State
	}
	w.backoff(p)
}

// backoff schedules p's next ask, doubling the wait. The caller holds mu.
func (w *prWatch) backoff(p *watchedPR) {
	p.next = w.now().Add(p.wait)
	p.wait = min(p.wait*2, watchMax)
}

// save puts info on every saved result of ref, and reports whether any
// changed.
func (w *prWatch) save(ref triage.PRRef, info *triage.PRInfo) bool {
	list, err := w.t.List()
	if err != nil {
		log.Printf("pr watch: %v", err)
		return false
	}
	changed := false
	for _, s := range list {
		if s.LocalPath != "" || prID(s.PR) != prID(ref) {
			continue
		}
		r, err := w.t.Load(s.Key)
		if err != nil {
			continue
		}
		if w.t.withPRInfo(r, info).PR.State != s.State {
			changed = true
		}
	}
	return changed
}

// generation is bumped each time the watch changes a saved PR's state;
// the sidebar reloads its list when it moves.
func (w *prWatch) generation() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.gen
}

func (w *prWatch) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/prwatch", func(rw http.ResponseWriter, r *http.Request) {
		writeJSON(rw, 200, map[string]int{"gen": w.generation()})
	})
	mux.HandleFunc("POST /api/prwatch/visible", func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			PRs []shownPR `json:"prs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(rw, 400, err)
			return
		}
		w.show(req.PRs)
		rw.WriteHeader(204)
	})
}
