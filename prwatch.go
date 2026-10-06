package main

import (
	"context"
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
	// its PRs one after another checks the repo once.
	watchTouch = time.Minute
)

// prWatch keeps the state of the saved PRs (open, merged, closed) current,
// so a PR merged on GitHub stops showing as open in the sidebar. It asks
// gh about the PRs saved as open when the app starts, and about a repo's
// open PRs again when one of its results is opened; a PR found merged or
// closed is no longer asked about until it is touched again.
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
	ref   triage.PRRef
	state string
	next  time.Time
	wait  time.Duration
}

func prID(ref triage.PRRef) string { return ref.RepoArg() + "#" + strconv.Itoa(ref.Number) }

func newPRWatch(t *triager) *prWatch {
	return &prWatch{
		t: t, resolve: t.fetcher.Resolve, now: time.Now,
		wake: make(chan struct{}, 1),
		prs:  map[string]*watchedPR{}, touched: map[string]time.Time{},
	}
}

// start loads the saved open PRs, due at once, and runs the watch until
// the triager shuts down.
func (w *prWatch) start() {
	done := w.t.track()
	if done == nil {
		return
	}
	go func() {
		defer done()
		w.sync("", "") // List reads every result; not on startup's time
		w.run(w.t.root)
	}()
}

// sync adds the saved PRs left open, of one repo or of all (""), due now,
// and the PR also, unless it is merged: a closed PR can be reopened. A PR
// already watched is made due now and its backoff starts over.
func (w *prWatch) sync(repo, also string) {
	list, err := w.t.List()
	if err != nil {
		log.Printf("pr watch: %v", err)
		return
	}
	now := w.now()
	w.mu.Lock()
	for _, s := range list {
		if s.LocalPath != "" || s.PR.Number == 0 || (repo != "" && s.PR.RepoArg() != repo) {
			continue
		}
		id := prID(s.PR)
		p := w.prs[id]
		if p == nil {
			// List is newest result first, so the first seen is the state
			// the sidebar shows.
			if !strings.EqualFold(s.State, "open") && (id != also || strings.EqualFold(s.State, "merged")) {
				continue
			}
			p = &watchedPR{ref: s.PR, state: s.State}
			w.prs[id] = p
		}
		p.next, p.wait = now, watchFirst
	}
	w.mu.Unlock()
	w.poke()
}

// touch is a result of ref being opened: its repo's open PRs and ref are
// asked about again, unless the repo was touched in the last watchTouch.
func (w *prWatch) touch(ref triage.PRRef) {
	if ref.Number == 0 {
		return
	}
	repo := ref.RepoArg()
	w.mu.Lock()
	last, ok := w.touched[repo]
	if ok && w.now().Sub(last) < watchTouch {
		w.mu.Unlock()
		return
	}
	w.touched[repo] = w.now()
	w.mu.Unlock()
	go w.sync(repo, prID(ref)) // List reads every result; not on the request's time
}

func (w *prWatch) poke() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// due returns the watched PR that is due first and how long until it is
// due (zero if it is), or nil when nothing is watched.
func (w *prWatch) due() (*watchedPR, time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var first *watchedPR
	for _, p := range w.prs {
		if first == nil || p.next.Before(first.next) {
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
// it, and schedules the next ask, or stops watching a PR no longer open.
// Only run calls it, so p.state is not written anywhere else meanwhile.
func (w *prWatch) check(ctx context.Context, p *watchedPR) {
	info, err := w.resolve(ctx, p.ref)
	if err != nil && ctx.Err() == nil {
		log.Printf("pr watch %s: %v", prID(p.ref), err)
	}
	changed := err == nil && info.State != p.state && w.save(p.ref, info)
	w.mu.Lock()
	defer w.mu.Unlock()
	if changed {
		w.gen++
	}
	if err == nil {
		p.state = info.State
		if !strings.EqualFold(info.State, "open") {
			delete(w.prs, prID(p.ref))
			return
		}
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
}
