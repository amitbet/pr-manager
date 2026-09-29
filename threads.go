package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"

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
// come and go while the PR head, and so the cached triage, stays.
func (t *triager) withThreads(ctx context.Context, r *PRResult, o options, progress func(string, int, int)) *PRResult {
	units := resultUnits(r)
	st := t.refreshThreads(ctx, r.PR, units, o, r.tierPolicy(), progress)
	if st == nil {
		return r
	}
	r.Threads = st
	rep := &triage.Report{Units: units}
	r.Counts = rep.Counts()
	r.Impact, r.Likelihood, r.Attention = rep.Scores()
	if err := t.saveResult(r); err != nil {
		log.Printf("save review threads %s: %v", r.Key, err)
	}
	return r
}

func (t *triager) saveResult(r *PRResult) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(t.results, r.Key+".json"), b, 0o644)
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
