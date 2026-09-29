package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
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

// sequence returns a result's sequence diagram, writing it if the result
// has none yet. A triage starts writing it as soon as the result is saved
// (see sequenceAfter), so opening the tab usually finds it written or
// waits on that call instead of starting another.
func (t *triager) sequence(ctx context.Context, key string, jo jobOptions) (*triage.Sequence, error) {
	return t.writeSequence(ctx, key, t.options(jo))
}

// sequenceAfter writes the sequence diagram of a result just saved, in the
// background, so the triage does not wait for it: at low effort it takes
// longer than the overview (27-33s against 17s on an 11-unit PR).
func (t *triager) sequenceAfter(key string, o options) {
	go func() {
		if _, err := t.writeSequence(context.Background(), key, o); err != nil {
			log.Printf("sequence %s: %v", key, err)
		}
	}()
}

func (t *triager) writeSequence(ctx context.Context, key string, o options) (*triage.Sequence, error) {
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
	if r.Sequence != nil && r.Sequence.Version >= triage.SequenceVersion {
		return r.Sequence, nil
	}
	l, err := sequencer(o)
	if err != nil {
		return nil, err
	}
	// Finish and save even if the reader moves on to another PR.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	sq, err := triage.WriteSequence(ctx, l, r.PR, resultUnits(r))
	if err != nil {
		return nil, err
	}
	// Read it again: the call is long enough for a thread refresh or a fix
	// to have saved the result in the meantime.
	if again, err := t.Load(key); err == nil {
		r = again
	}
	r.Sequence = sq
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return sq, os.WriteFile(filepath.Join(t.results, key+".json"), b, 0o644)
}

// resultUnits are a result's units as the triage had them. Unit.Hunks is
// not saved (the result keeps them next to the unit), so a loaded result's
// units get theirs back: without them the sequence prompt leaves every
// unit out, and threads have no lines to land on.
func resultUnits(r *PRResult) []*triage.Unit {
	var units []*triage.Unit
	for _, f := range r.Files {
		for _, u := range f.Units {
			if len(u.Unit.Hunks) == 0 {
				u.Unit.Hunks = u.Hunks
			}
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

// sequenceEffort caps the reasoning effort of the sequence call. It redraws
// what the units already say rather than reviewing code, and its time goes
// to reasoning tokens: on an 11-unit PR gpt-6-sol took 58s at medium (1422
// reasoning tokens) and 27-33s at low (~480) for as good a diagram. The
// small model was faster still but left the before side nearly empty.
const sequenceEffort = "low"

// sequencer is the overview's model with its effort capped at sequenceEffort.
func sequencer(o options) (llm.LLMTool, error) {
	l, err := newOverviewer(o)
	if err != nil {
		return nil, err
	}
	llm.SetEffort(l, capEffort(o.reviewEffort, sequenceEffort))
	return l, nil
}

// capEffort is effort, or limit when effort is higher or unset (the model's
// default is not known to be lower).
func capEffort(effort, limit string) string {
	rank := map[string]int{"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5}
	e, ok := rank[effort]
	if l, lok := rank[limit]; ok && lok && e <= l {
		return effort
	}
	return limit
}
