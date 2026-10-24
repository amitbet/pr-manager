package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/internal/activity"
	"github.com/amitbet/pr-manager/triage"
)

// A suggested fix is the patch the fixer would make for one issue or
// review comment, shown on the Issues tab before anything changes. It is
// made like a fix's first round, the scope checks included (fixScope), in
// a scratch copy of the reviewed code, which is thrown away: the checkout
// is never touched. The patch is kept on the result, and Apply starts a
// fix whose first round applies it as it is (then checks, tests and
// audits it like any round); when the code moved and it no longer applies,
// the fixer makes the fix again.

// fixSuggestion is one suggested fix, kept on the result.
type fixSuggestion struct {
	UnitID string `json:"unit_id"`
	// Title names the issue (an issue's index moves when the review
	// changes); Thread names a review comment instead.
	Title  string `json:"title,omitempty"`
	Thread string `json:"thread,omitempty"`
	Patch  string `json:"patch"`
	Reason string `json:"reason,omitempty"`
	// Extra is what the fixer said it added beyond the smallest fix;
	// Scope what fixScope still found out of scope.
	Extra string    `json:"extra,omitempty"`
	Scope string    `json:"scope,omitempty"`
	Model string    `json:"model,omitempty"`
	At    time.Time `json:"at"`
}

func (s fixSuggestion) matches(unitID, title, thread string) bool {
	return s.UnitID == unitID && s.Title == title && s.Thread == thread
}

// suggestTarget is the one issue or thread req names on r.
func suggestTarget(r *PRResult, req fixRequest) (targetedIssue, error) {
	if req.UnitID == "" || len(req.Targets) > 0 || req.All || req.Change != nil {
		return targetedIssue{}, errors.New("a suggestion is for one issue or review comment")
	}
	targets := fixTargets(r, req)
	if len(targets) != 1 {
		return targetedIssue{}, errors.New("no such issue to suggest a fix for")
	}
	return targets[0], nil
}

// suggestFix makes and keeps the suggested fix for one issue (or thread)
// of a result.
func (t *triager) suggestFix(ctx context.Context, key string, req fixRequest, progress func(string, int, int)) (*fixSuggestion, error) {
	r, err := t.Load(key)
	if err != nil {
		return nil, err
	}
	x, err := suggestTarget(r, req)
	if err != nil {
		return nil, err
	}
	targets := []targetedIssue{x}
	o := t.options(req.jobOptions)
	if o.summarizer == "off" {
		return nil, errors.New("enable a summarizer to suggest fixes")
	}
	progress("checkout", 0, 0)
	dir, done, err := t.resultCheckout(r)
	if err != nil {
		return nil, err
	}
	defer done()
	ctx, th := activity.Start(ctx, "llm", "suggest a fix: %s", x.Issue.Title)
	scratch, base, cleanup, err := fixScratch(ctx, dir, fixFiles(targets))
	if err != nil {
		th.Finish(err)
		return nil, err
	}
	defer cleanup()
	// One patch, not an agent: a suggestion is one issue, and quick.
	progress("suggest", 0, 0)
	out, err := fixRound(ctx, o, scratch, r, targets, nil, false, fixCall{})
	if err == nil && len(out.changed) == 0 {
		err = errors.New("the fixer changed nothing")
	}
	var patch string
	if err == nil {
		patch, err = scratchPatch(ctx, scratch, base)
	}
	th.Finish(err)
	if err != nil {
		return nil, err
	}
	sug := fixSuggestion{UnitID: x.UnitID, Patch: patch, Extra: out.extra, Scope: out.scope, At: time.Now()}
	if l, err := newFixLLM(o.summarizer, o.summaryModel); err == nil {
		sug.Model = l.Name() + "/" + l.ModelID()
	}
	if x.Comment != nil {
		sug.Thread = x.Comment.Thread
	} else {
		sug.Title = x.Issue.Title
	}
	if _, err := t.updateResult(key, func(r *PRResult) {
		r.Suggestions = slices.DeleteFunc(r.Suggestions, func(s fixSuggestion) bool { return s.matches(sug.UnitID, sug.Title, sug.Thread) })
		r.Suggestions = append(r.Suggestions, sug)
	}); err != nil {
		return nil, err
	}
	return &sug, nil
}

// suggestedPatch is the suggestion a fix asked to apply (req.Suggested)
// for its one target, or nil.
func suggestedPatch(r *PRResult, req fixRequest, targets []targetedIssue) *fixAnswer {
	if !req.Suggested || len(targets) != 1 {
		return nil
	}
	x := targets[0]
	title, thread := x.Issue.Title, ""
	if x.Comment != nil {
		title, thread = "", x.Comment.Thread
	}
	for _, s := range r.Suggestions {
		if s.matches(x.UnitID, title, thread) && strings.TrimSpace(s.Patch) != "" {
			return &fixAnswer{patch: s.Patch, extra: s.Extra}
		}
	}
	return nil
}

// resultCheckout is a checkout of the code a result reviewed: a fix's own
// checkout, the reader's working tree for a local review of it, or else a
// worktree of the reviewed head with the PR's agent files taken out
// (removed by done).
func (t *triager) resultCheckout(r *PRResult) (dir string, done func(), err error) {
	noop := func() {}
	if r.PR == nil || r.PR.HeadOid == "" {
		return "", noop, errors.New("this result has no head commit")
	}
	if r.LocalFixDir != "" {
		return r.LocalFixDir, noop, nil
	}
	repo := r.PR.LocalPath
	if repo == "" && t.fetcher != nil {
		repo = t.fetcher.RepoDir(r.PR.PRRef)
	}
	if repo == "" {
		return "", noop, errors.New("no checkout of the change")
	}
	if r.PR.LocalPath != "" && r.PR.Rev == "" {
		return repo, noop, nil // the working tree is what was reviewed
	}
	ws, cleanup, err := triage.ReviewWorkspace(&triage.Source{Dir: repo, Head: r.PR.HeadOid})
	if err != nil {
		return "", noop, err
	}
	if ws == nil {
		cleanup()
		return "", noop, errors.New("no checkout of the change")
	}
	return ws.Dir, cleanup, nil
}

func (t *triager) suggestRoutes(mux *http.ServeMux) {
	// A suggestion takes a minute or more: it runs as a job, listed with
	// the others, whose result the UI loads again when it is done.
	mux.HandleFunc("POST /api/results/{key}/suggest", func(w http.ResponseWriter, r *http.Request) {
		var req fixRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, 400, err)
			return
		}
		res, err := t.Load(r.PathValue("key"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		if _, err := suggestTarget(res, req); err != nil {
			writeErr(w, 400, err)
			return
		}
		j := t.startAux("suggest", res, func(ctx context.Context, progress func(string, int, int)) error {
			_, err := t.suggestFix(ctx, res.Key, req, progress)
			return err
		})
		writeJSON(w, 202, t.snapshot(j))
	})
}
