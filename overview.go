package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

// overview returns a result's overview. Results from before overviews, or
// whose overview failed at triage, get one written and saved with the
// result the first time they are opened.
func (t *triager) overview(ctx context.Context, key string, jo jobOptions) (*triage.Overview, error) {
	t.trMu.Lock()
	mu := t.trLocks["overview:"+key]
	if mu == nil {
		mu = &sync.Mutex{}
		t.trLocks["overview:"+key] = mu
	}
	t.trMu.Unlock()
	mu.Lock()
	defer mu.Unlock()

	r, err := t.Load(key)
	if err != nil {
		return nil, err
	}
	if r.Overview != nil {
		return r.Overview, nil
	}
	l, err := newOverviewer(t.options(jo))
	if err != nil {
		return nil, err
	}
	// Finish and save even if the reader moves on to another PR.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	if len(r.PR.Commits) == 0 {
		dir := r.PR.LocalPath
		if dir == "" {
			dir = t.fetcher.RepoDir(r.PR.PRRef)
		}
		r.PR.Commits = triage.CommitMessages(ctx, dir, r.PR.BaseOid, r.PR.HeadOid)
	}
	if r.Overview, err = triage.WriteOverview(ctx, l, r.PR, resultUnits(r)); err != nil {
		return nil, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return r.Overview, os.WriteFile(filepath.Join(t.results, key+".json"), b, 0o644)
}

// sequence returns a result's sequence diagram, writing it the first time
// the tab is opened. It is lazy because most PRs are never looked at this
// way and the call is a review-sized one; once written it is saved with
// the result like the overview.
func (t *triager) sequence(ctx context.Context, key string, jo jobOptions) (*triage.Sequence, error) {
	t.trMu.Lock()
	mu := t.trLocks["sequence:"+key]
	if mu == nil {
		mu = &sync.Mutex{}
		t.trLocks["sequence:"+key] = mu
	}
	t.trMu.Unlock()
	mu.Lock()
	defer mu.Unlock()

	r, err := t.Load(key)
	if err != nil {
		return nil, err
	}
	if r.Sequence != nil {
		return r.Sequence, nil
	}
	l, err := newOverviewer(t.options(jo))
	if err != nil {
		return nil, err
	}
	// Finish and save even if the reader moves on to another PR.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	if r.Sequence, err = triage.WriteSequence(ctx, l, r.PR, resultUnits(r)); err != nil {
		return nil, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return r.Sequence, os.WriteFile(filepath.Join(t.results, key+".json"), b, 0o644)
}

func resultUnits(r *PRResult) []*triage.Unit {
	var units []*triage.Unit
	for _, f := range r.Files {
		for _, u := range f.Units {
			units = append(units, u.Unit)
		}
	}
	return units
}

var newOverviewer = overviewer // tests replace it

// overviewer is the summarizer's model: the overview is a review call.
func overviewer(o options) (llm.LLMTool, error) {
	if o.summarizer == "" || o.summarizer == "off" {
		return nil, errors.New("no summarizer to write the overview")
	}
	l, err := llm.New(o.summarizer, o.summaryModel)
	if err != nil {
		return nil, err
	}
	llm.SetEffort(l, o.reviewEffort)
	return l, nil
}
