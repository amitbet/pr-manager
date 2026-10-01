package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

// refreshThreads loads the PR's open review threads onto units, checks
// the new ones, and re-places the units under tp with the confirmed ones
// counted. Threads whose comments have not changed keep their verdict, so
// a refresh only costs the gh call. A failed fetch keeps the threads units
// already have. Local changes have no threads (nil).
func (t *triager) refreshThreads(ctx context.Context, info *triage.PRInfo, units []*triage.Unit, o options, tp triage.TierPolicy, progress func(string, int, int)) *triage.ThreadStats {
	if info.LocalPath != "" || info.Number == 0 {
		return nil
	}
	if progress == nil {
		progress = func(string, int, int) {}
	}
	progress("comments", 0, 0)
	fresh, st, err := triage.FetchThreads(ctx, info.PRRef, info.Author)
	if err != nil {
		log.Printf("review threads %s: %v", info.URL, err)
		st.Error = err.Error()
		return &st
	}
	var old []triage.Thread
	for _, u := range units {
		old = append(old, u.Threads...)
		u.Threads = nil
	}
	st.Unanchored = triage.AssignThreads(units, triage.MergeThreads(old, fresh))
	if sum, err := threadJudge(o); err != nil {
		log.Printf("review threads %s: %v", info.URL, err)
	} else if sum != nil {
		src := &triage.Source{Dir: t.fetcher.RepoDir(info.PRRef), Head: info.HeadOid}
		limit := o.reviewConcurrency
		if limit <= 0 {
			limit = o.concurrency
		}
		sum.JudgeThreads(ctx, src, units, limit, func(done, total int) { progress("comments", done, total) })
	}
	for _, u := range units {
		for i := range u.Threads {
			if th := &u.Threads[i]; th.Trusted && th.Status == "" {
				th.Status, th.Reason = triage.ThreadUnchecked, "no review model configured"
			}
		}
		triage.SortThreads(u.Threads)
		tp.ApplyThreads(u)
	}
	return &st
}

// withThreads refreshes a saved result's threads and saves it: comments
// come and go while the PR head, and so the cached triage, stays. The
// refresh can take minutes, so only the threads and what they place are
// put on the result as saved by then.
func (t *triager) withThreads(ctx context.Context, r *PRResult, o options, progress func(string, int, int)) *PRResult {
	units := resultUnits(r)
	tp := r.tierPolicy()
	st := t.refreshThreads(ctx, r.PR, units, o, tp, progress)
	if st == nil {
		return r
	}
	// New comments may repeat issues on other units. Only they are
	// compared, so a refresh that brought none makes no call.
	if sum, err := threadJudge(o); err == nil && sum != nil {
		progress("dedupe", 0, 0)
		sum.Dedupe(ctx, units, tp)
	}
	threads := map[string][]triage.Thread{}
	links := map[string][]triage.Issue{}
	for _, u := range units {
		threads[u.ID] = u.Threads
		links[u.ID] = u.Issues
	}
	setThreads := func(r *PRResult) {
		units := resultUnits(r)
		for _, u := range units {
			if th, ok := threads[u.ID]; ok {
				u.Threads = th
			}
			// The issues are the saved ones; only the links are new.
			if is := links[u.ID]; len(is) == len(u.Issues) {
				for i := range u.Issues {
					if u.Issues[i].Title == is[i].Title {
						u.Issues[i].SameAs, u.Issues[i].Compared = is[i].SameAs, is[i].Compared
					}
				}
			}
			tp.ApplyDismissals(u)
		}
		r.Threads = st
		rep := &triage.Report{Units: units}
		r.Counts = rep.Counts()
		r.Impact, r.Likelihood, r.Attention = rep.Scores()
	}
	saved, err := t.updateResult(r.Key, setThreads)
	if err != nil {
		log.Printf("save review threads %s: %v", r.Key, err)
		setThreads(r)
		return r
	}
	return saved
}

// resultLock is the lock of one saved result. Every write of a result
// holds it, so writers that run at once (the overview, the sequence, a
// thread refresh) each keep what the others saved.
func (t *triager) resultLock(key string) *sync.Mutex {
	t.trMu.Lock()
	defer t.trMu.Unlock()
	mu := t.trLocks["result:"+key]
	if mu == nil {
		mu = &sync.Mutex{}
		t.trLocks["result:"+key] = mu
	}
	return mu
}

// updateResult reads the saved result again under its lock, lets set put
// what the caller computed on it, and saves it. The slow part (a model
// call) belongs before it, outside the lock.
func (t *triager) updateResult(key string, set func(*PRResult)) (*PRResult, error) {
	mu := t.resultLock(key)
	mu.Lock()
	defer mu.Unlock()
	r, err := t.Load(key)
	if err != nil {
		return nil, err
	}
	set(r)
	return r, t.writeResult(r)
}

// saveResult saves a whole result, as when a triage writes a new one.
func (t *triager) saveResult(r *PRResult) error {
	mu := t.resultLock(r.Key)
	mu.Lock()
	defer mu.Unlock()
	return t.writeResult(r)
}

// writeResult writes through a temporary file renamed over the result, so
// a concurrent Load never reads half of one. The caller holds its lock.
func (t *triager) writeResult(r *PRResult) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	p := filepath.Join(t.results, r.Key+".json")
	tmp, err := os.CreateTemp(t.results, ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), 0o644)
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), p)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
	}
	return werr
}

// threadJudge is the reviewer that checks comments: the summarizer's
// model, as critic too. nil when the summarizer is off.
func threadJudge(o options) (*triage.Summarizer, error) {
	if o.summarizer == "off" {
		return nil, nil
	}
	l, err := llm.New(o.summarizer, o.summaryModel)
	if err != nil {
		return nil, err
	}
	llm.SetEffort(l, o.reviewEffort)
	return &triage.Summarizer{LLM: l, Critic: l, Policy: triage.DefaultPolicy(), Tools: o.reviewTools}, nil
}
