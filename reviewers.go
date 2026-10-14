package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/pr-manager/internal/activity"
	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

// extraReviewer is one of the reader's own review skills or prompts, set
// up in Settings and run on a result only when asked (POST reanalyze,
// what=reviewers). Its findings are pooled with the review's (see
// triage/extra.go). The built-in review never runs them, and nothing of
// theirs is in the cache key, so a triage is the same with or without.
type extraReviewer struct {
	// Name labels its findings (Issue.Source) and keys its last run.
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	// Skill is a skill name or slash command the CLI knows, such as
	// "/code-review high"; Prompt adds instructions, or is the whole
	// review without a skill.
	Skill  string `json:"skill,omitempty"`
	Prompt string `json:"prompt,omitempty"`
}

// extraRun is what an extra reviewer's last run on a result did.
type extraRun struct {
	Name  string    `json:"name"`
	Model string    `json:"model"`
	At    time.Time `json:"at"`
	// Found is what it reported, Unplaced the findings on files outside
	// the change; Kept the issues the critic left standing.
	Found    int    `json:"found"`
	Unplaced int    `json:"unplaced,omitempty"`
	Kept     int    `json:"kept"`
	Error    string `json:"error,omitempty"`
}

// Tests replace them.
var (
	newReviewerLLM    = llm.New
	newReviewerCritic = threadJudge
)

// extraReviewMaxTokens caps an extra reviewer's answer: a whole PR's findings.
const extraReviewMaxTokens = 32000

func (rv *extraReviewer) check() error {
	rv.Name, rv.Skill, rv.Prompt = strings.TrimSpace(rv.Name), strings.TrimSpace(rv.Skill), strings.TrimSpace(rv.Prompt)
	rv.Provider = llm.ProviderID(rv.Provider)
	switch {
	case rv.Name == "":
		return errors.New("a reviewer needs a name")
	case rv.Provider == "":
		return fmt.Errorf("reviewer %s: no provider", rv.Name)
	case rv.Skill == "" && rv.Prompt == "":
		return fmt.Errorf("reviewer %s: give it a skill or a prompt", rv.Name)
	}
	if _, err := llm.New(rv.Provider, rv.Model); err != nil {
		return fmt.Errorf("reviewer %s: %w", rv.Name, err)
	}
	// Only the CLIs run as the reader set them up, with their skills.
	if rv.Skill != "" && rv.Provider != "codex" && rv.Provider != "claude-code" {
		return fmt.Errorf("reviewer %s: skills need codex or claude-code, not %s", rv.Name, rv.Provider)
	}
	return nil
}

// checkReviewers checks every reviewer and that no two share a name.
func checkReviewers(revs []extraReviewer) error {
	if len(revs) == 0 {
		return errors.New("no reviewers to run: add them in Settings, Reviewers")
	}
	seen := map[string]bool{}
	for i := range revs {
		if err := revs[i].check(); err != nil {
			return err
		}
		if seen[revs[i].Name] {
			return fmt.Errorf("two reviewers are named %s", revs[i].Name)
		}
		seen[revs[i].Name] = true
	}
	return nil
}

// reviewerDir is the checkout the extra reviewers read: the reader's own
// checkout for a local review of its working tree, else a worktree of the
// reviewed head, with a PR's agent files taken out (see
// triage.ReviewWorkspace). own says it is the reader's code, whose
// project settings and skills the CLIs may load.
func (t *triager) reviewerDir(r *PRResult) (dir string, own bool, cleanup func(), err error) {
	noop := func() {}
	if p := r.PR.LocalPath; p != "" && r.PR.Rev == "" {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p, true, noop, nil
		}
	}
	repo := r.PR.LocalPath
	if repo == "" && t.fetcher != nil {
		repo = t.fetcher.RepoDir(r.PR.PRRef)
	}
	if repo == "" || r.PR.HeadOid == "" {
		return "", false, noop, errors.New("no checkout of the reviewed head to run the reviewers in")
	}
	ws, cleanup, err := triage.ReviewWorkspace(&triage.Source{Dir: repo, Head: r.PR.HeadOid})
	if err != nil {
		return "", false, noop, err
	}
	if ws == nil {
		return "", false, noop, errors.New("no checkout of the reviewed head to run the reviewers in")
	}
	return ws.Dir, r.PR.LocalPath != "", cleanup, nil
}

// runReviewers runs revs on r side by side, pools what they found with
// the review's issues, checks it with the critic, links the repeats and
// saves the result. A reviewer whose call fails keeps the findings of its
// last run; one that answers replaces them.
func (t *triager) runReviewers(ctx context.Context, r *PRResult, revs []extraReviewer, o options, progress func(string, int, int)) error {
	if r.PR.BaseOid == "" {
		return errors.New("this result has no merge base to diff against: triage it again")
	}
	dir, own, cleanup, err := t.reviewerDir(r)
	if err != nil {
		return err
	}
	defer cleanup()
	units := resultUnits(r)
	var files []string
	seen := map[string]bool{}
	for _, u := range units {
		if !seen[u.File] {
			seen[u.File] = true
			files = append(files, u.File)
		}
	}

	answers := make([]reviewerAnswer, len(revs))
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	progress("reviewers", 0, len(revs))
	for i, rv := range revs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i] = t.runReviewer(ctx, rv, dir, own, r.PR.BaseOid, files, o)
			mu.Lock()
			done++
			progress("reviewers", done, len(revs))
			mu.Unlock()
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}

	tp := r.tierPolicy()
	sum, err := newReviewerCritic(o)
	if err != nil {
		log.Printf("reviewers %s: no critic: %v", r.Key, err)
	}
	var ws *llm.Workspace
	if o.reviewTools {
		ws = &llm.Workspace{Dir: dir}
	}
	limit := o.reviewConcurrency
	if limit <= 0 {
		limit = o.concurrency
	}
	runs := make([]extraRun, len(revs))
	progress("checking", 0, 0)
	for i, rv := range revs {
		a := answers[i]
		runs[i] = extraRun{Name: rv.Name, Model: a.model, At: time.Now()}
		if a.err != nil {
			runs[i].Error = a.err.Error()
			continue
		}
		placed, lost := triage.PlaceFindings(units, rv.Name, a.findings)
		runs[i].Found, runs[i].Unplaced = len(a.findings), lost
		if sum != nil {
			sum.AddExtraIssues(ctx, ws, placed, limit)
		} else {
			for u, is := range placed {
				u.Issues = append(u.Issues, is...)
			}
		}
		for _, u := range units {
			for _, is := range u.Issues {
				if is.Source == rv.Name && is.CriticRejected == "" {
					runs[i].Kept++
				}
			}
		}
	}
	if sum != nil {
		progress("dedupe", 0, 0)
		sum.Dedupe(ctx, units, tp)
	}
	for _, u := range units {
		tp.ApplyIssues(u)
	}

	byID := map[string]*triage.Unit{}
	for _, u := range units {
		byID[u.ID] = u
	}
	set := func(r *PRResult) {
		units := resultUnits(r)
		for _, u := range units {
			if n := byID[u.ID]; n != nil {
				u.Issues, u.Score, u.Attention, u.Decision.Bucket = n.Issues, n.Score, n.Attention, n.Decision.Bucket
			}
			tp.ApplyDismissals(u)
		}
		r.Reviewers = mergeRuns(r.Reviewers, runs)
		rep := &triage.Report{Units: units}
		r.Counts = rep.Counts()
		r.Impact, r.Likelihood, r.Attention = rep.Scores()
	}
	if _, err := t.updateResult(r.Key, set); err != nil {
		return err
	}
	var failed []string
	for _, run := range runs {
		if run.Error != "" {
			failed = append(failed, run.Name+": "+run.Error)
		}
	}
	if len(failed) == len(runs) {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}

// reviewerAnswer is what one reviewer's call came back with.
type reviewerAnswer struct {
	findings []triage.ExtraFinding
	model    string
	err      error
}

// runReviewer runs one reviewer in dir: a CLI as the reader set it up
// (their skills, tools and rules; see llm.Workspace.Installed), any
// other provider reading dir through the app's tools.
func (t *triager) runReviewer(ctx context.Context, rv extraReviewer, dir string, own bool, base string, files []string, o options) (a reviewerAnswer) {
	l, err := newReviewerLLM(rv.Provider, rv.Model)
	if err != nil {
		a.err = err
		return
	}
	llm.SetEffort(l, orDefault(rv.Effort, o.reviewEffort))
	a.model = l.Name() + "/" + l.ModelID()
	ws := &llm.Workspace{Dir: dir}
	if llm.SupportsWorkspace(l) {
		ws.Installed, ws.Project = true, own
	}
	ctx, th := activity.Start(ctx, "llm", "reviewer %s (%s)", rv.Name, a.model)
	defer func() { th.Finish(a.err) }()
	args, _, err := llm.CallToolReading(ctx, l, ws, []llm.ChatMessage{
		{Role: "system", Content: "You review a pull request for its reader, with their own tools and skills, and report what the review found."},
		{Role: "user", Content: triage.ExtraReviewPrompt(rv.Skill, rv.Prompt, base, files)},
	}, triage.ExtraReviewTool, extraReviewMaxTokens)
	if err != nil {
		a.err = err
		return
	}
	a.findings, a.err = triage.DecodeFindings(args)
	return
}

// mergeRuns is old with each of runs in place of the run of the same
// name. A failed run keeps the counts of the last one that answered,
// whose findings are still on the result.
func mergeRuns(old, runs []extraRun) []extraRun {
	out := append([]extraRun(nil), old...)
	for _, run := range runs {
		i := 0
		for i < len(out) && out[i].Name != run.Name {
			i++
		}
		if i == len(out) {
			out = append(out, run)
			continue
		}
		if run.Error != "" {
			out[i].Error, out[i].At = run.Error, run.At
			continue
		}
		out[i] = run
	}
	return out
}
