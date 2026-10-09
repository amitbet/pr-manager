package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/internal/proc"
	"github.com/amitbet/pr-manager/triage"
)

// The chat agent can ask the UI to act: open a unit, draft or post a
// comment, dismiss an issue, fix issues, change the code as asked,
// analyze parts of the change again. It answers with a list of actions next to its text; the UI's
// agent API (ui/js/agentapi.js) runs them, after the reader approves the
// ones that change something, and sends back what happened, with the time,
// as an event turn. This file has the catalog the agent is told about, and
// the endpoints the UI had no use for before: re-reviewing some units,
// writing the overview or the sequence again, refreshing the review
// comments alone, and replying to or resolving a review thread.

// chatActions is the catalog, in the order the agent is told about it.
// risk is how the UI treats a proposal: view runs at once, local and job
// wait for the reader unless Settings lets them run, outward always waits.
var chatActions = []struct{ name, risk, args, does string }{
	{"open_unit", "view", `{"unit": ID}`, "show a unit in the Review tab"},
	{"snapshot", "view", `{}`, "take a picture of the page as the reader sees it, without the chat panel, when what is on screen matters (a layout, a chart, where something is); you get it in your next turn, with the page's text"},
	{"open_tab", "view", `{"tab": "review"|"issues"|"sequence"|"map"}`, "switch tab"},
	{"fill_field", "view", `{"field": "comment"|"dismiss_reason"|"review_summary", "text": text} (comment: also "path", "line", "side": "RIGHT"|"LEFT" to open one where none is open)`, "write into a text box on the page (the open ones are listed with the reader's turn, with what they say): replaces its text, which the reader then edits and saves; use it when they ask you to write or rewrite what they are typing"},
	{"draft_comment", "local", `{"unit": ID, "line": n, "side": "RIGHT"|"LEFT" (LEFT: a line of the old file), "body": text, "id": draft id to replace}`, "save a pending review comment on a changed line (or within 3 lines of one), or rewrite one; nothing is posted until the reader submits the review"},
	{"delete_draft", "local", `{"id": draft id}`, "remove a pending comment"},
	{"dismiss", "local", `{"unit": ID, "kind": "issue"|"lint", "index": n, "reason": text}`, "dismiss a review issue or lint finding the reader agrees is wrong, with the reason"},
	{"restore", "local", `{"unit": ID, "kind": "issue"|"lint", "index": n}`, "undo a dismissal"},
	{"mark_reviewed", "local", `{"units": [ID...], "reviewed": true|false}`, "tick units off in the walkthrough"},
	{"fix", "job", `{"targets": [{"unit": ID, "issue": n} | {"unit": ID, "thread": thread id}]} or {"all": true, "comments": bool}`, "fix issues or confirmed review comments in a separate checkout, then re-check and re-triage the result; takes minutes"},
	{"change", "job", `{"instructions": text, "files": [path...], "unit": ID}` + " (files: every file it may change, relative to the repository, 1 to 20; unit: optional)", "change the code as the reader asks, by an agent in the fix checkout, then re-check and re-triage the result, like fix; the reader's words, written out in full, are its instructions; takes minutes; push_fix pushes it"},
	{"commit_edits", "job", `{"title": text, "unit": ID}` + " (title: the commit's first line; unit: optional)", "make the edits in your working directory a commit of the fix checkout, then re-check (a review, not a build or tests: run those yourself first) and re-triage the result, like a change; takes minutes; push_fix pushes it"},
	{"reanalyze_units", "job", `{"units": [ID...]}`, "review these units again with the reviewer model, keeping every other unit's review"},
	{"retriage", "job", `{"fresh": bool}`, "triage the change again: the latest code, comments and code map; units whose code didn't change keep their review unless fresh"},
	{"refresh_comments", "job", `{}`, "fetch the PR's review comments from GitHub again and judge the new ones"},
	{"regenerate", "job", `{"what": "overview"|"sequence"}`, "write the overview or the sequence diagram again"},
	{"reindex_codemap", "job", `{}`, "rebuild the code map of the reader's repos (impact scores)"},
	{"reply_thread", "outward", `{"thread": thread id, "body": text}`, "post a reply on a GitHub review thread"},
	{"resolve_thread", "outward", `{"thread": thread id}`, "resolve a GitHub review thread"},
	{"submit_review", "outward", `{"event": "COMMENT"|"APPROVE"|"REQUEST_CHANGES", "body": text}`, "submit the pending comments as one GitHub review"},
	{"push_fix", "outward", `{}`, "push the fix commits of this result to the PR's branch"},
}

func chatActionNames() []string {
	names := make([]string, len(chatActions))
	for i, a := range chatActions {
		names[i] = a.name
	}
	return names
}

// chatActionsDoc tells the agent what it can do and how. The reader's own
// agent (installed) edits the code itself and commits its edits; the
// read-only one asks for a change.
func chatActionsDoc(installed bool) string {
	var b strings.Builder
	b.WriteString(`## Actions

Besides answering, you can act, by listing actions in your reply. The reader sees each one with your reason and runs it with a click; navigation runs at once. Propose an action when the reader asks for it, or when it plainly answers the question ("show me" → open_unit); don't act on your own initiative otherwise, and never propose posting to GitHub unless asked to. When you propose one, say in your answer what it will do. Arguments are a JSON object, as a string. Units are named by their id, issues and lint findings by their index in the unit (Issue #n, Lint #n below), review threads by their thread id.

`)
	for _, a := range chatActions {
		if a.name == "change" && installed || a.name == "commit_edits" && !installed {
			continue
		}
		fmt.Fprintf(&b, "- %s %s: %s.\n", a.name, a.args, a.does)
	}
	b.WriteString(`
What happens comes back as an [event] turn with its time: the result, or why it failed or was declined. Jobs take a while; when one finishes the material below is the new result. Then continue: report what changed, briefly, and propose the next step only if one is needed. Don't repeat an action that already ran. A snapshot's event names its picture as [snapshot: <file>]: read that file to see it, unless it is attached to the turn already; without either, go by the page's text the event carries.`)
	return b.String()
}

// chatTurn is one turn of the conversation as the UI keeps it: the
// reader's, the agent's, or an event (an action's outcome). At is when.
type chatTurn struct {
	Role    string `json:"role"` // user | assistant | event
	Content string `json:"content"`
	At      string `json:"at,omitempty"` // RFC 3339
}

// chatAction is an action the agent proposes; Args is its JSON object.
type chatAction struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
	Why  string         `json:"why"`
}

// parseActions reads the reply's actions, leaving out the ones the
// catalog doesn't have or whose arguments aren't an object.
func parseActions(v any) []chatAction {
	known := map[string]bool{}
	for _, n := range chatActionNames() {
		known[n] = true
	}
	list, _ := v.([]any)
	var out []chatAction
	for _, x := range list {
		m, _ := x.(map[string]any)
		name, _ := m["name"].(string)
		if !known[name] {
			continue
		}
		args := map[string]any{}
		switch a := m["args"].(type) {
		case string:
			if strings.TrimSpace(a) != "" && json.Unmarshal([]byte(a), &args) != nil {
				continue
			}
		case map[string]any:
			args = a
		}
		why, _ := m["why"].(string)
		out = append(out, chatAction{Name: name, Args: args, Why: why})
	}
	return out
}

// atText is " at <time>", or "" without one.
func atText(at string) string {
	if at == "" {
		return ""
	}
	return " at " + at
}

// chatNow is the clock line at the top of the material: how old its parts
// are. Now is the time of the reader's newest turn, so the material stays
// the same from turn to turn while the result does.
func chatNow(r *PRResult, _ time.Time) string {
	s := fmt.Sprintf("Result %s was triaged %s", r.Key, r.CreatedAt.Format(time.RFC3339))
	if r.Threads != nil && !r.Threads.FetchedAt.IsZero() {
		s += fmt.Sprintf("; its GitHub review comments were fetched %s", r.Threads.FetchedAt.Format(time.RFC3339))
	}
	return s + "."
}

// rerun is whether a triage job must run rather than reopen a saved
// result: a forced run, or a re-review of some units.
func (jo jobOptions) rerun() bool { return jo.Force || len(jo.Rereview) > 0 }

// planCarry is the carry-over of a triage run: with Rereview, everything
// of the result being re-reviewed but those units; otherwise the
// incremental plan.
func (t *triager) planCarry(ctx context.Context, key, base string, o options, jo jobOptions) *carryPlan {
	if len(jo.Rereview) > 0 {
		if cur, err := t.Load(orDefault(jo.RereviewOf, key)); err == nil {
			return rereviewPlan(cur, jo.Rereview)
		}
	}
	return t.carryFrom(ctx, key, base, o, jo.Force)
}

// rereviewPlan keeps the review of every unit of cur except ids, which
// are reviewed again, and any whose diff moved since.
func rereviewPlan(cur *PRResult, ids []string) *carryPlan {
	again := map[string]bool{}
	for _, id := range ids {
		again[id] = true
	}
	old := map[string]*triage.Unit{}
	for _, u := range resultUnitsWithHunks(cur) {
		old[u.ID] = u
	}
	from := orDefault(cur.PR.HeadOid, cur.Key)
	return &carryPlan{prev: cur, hook: func(fresh []*triage.Unit) *triage.ReviewCarry {
		c := &triage.ReviewCarry{From: from, Reuse: map[string]*triage.Unit{}, Why: map[string]string{}}
		for _, u := range fresh {
			o := old[u.ID]
			switch {
			case o == nil || !o.Reviewed:
			case again[u.ID]:
				c.Why[u.ID] = "asked to review again"
			case !triage.SameDiff(o, u):
				c.Why[u.ID] = "its own diff changed"
			default:
				c.Reuse[u.ID] = o
			}
		}
		return c
	}}
}

// reanalyzeRequest is POST /api/results/{key}/reanalyze.
type reanalyzeRequest struct {
	// What is units (Units again), all (a re-triage; Fresh reviews
	// everything), threads, overview or sequence.
	What  string   `json:"what"`
	Units []string `json:"units"`
	Fresh bool     `json:"fresh"`
	jobOptions
}

func (t *triager) reanalyze(r *PRResult, req reanalyzeRequest) (*job, error) {
	jo := req.jobOptions
	jo.Rereview, jo.RereviewOf, jo.Force = nil, "", false
	switch req.What {
	case "units", "all":
		if r.LocalFixDir != "" {
			return nil, errors.New("this is the result of a fix: analyze the PR itself again, or fix again")
		}
		if req.What == "units" {
			if len(req.Units) == 0 {
				return nil, errors.New("no units to review again")
			}
			have := map[string]bool{}
			for _, u := range resultUnits(r) {
				have[u.ID] = true
			}
			for _, id := range req.Units {
				if !have[id] {
					return nil, fmt.Errorf("no unit %q in this result", id)
				}
			}
			jo.Rereview, jo.RereviewOf = req.Units, r.Key
		} else {
			jo.Force = req.Fresh
		}
		if r.PR.LocalPath != "" {
			return t.startLocal(localSrcOf(r.PR), jo)
		}
		return t.start(r.PR.URL, jo)
	case "threads":
		if r.PR.LocalPath != "" {
			return nil, errors.New("a local review has no review comments")
		}
		o := t.options(jo)
		return t.startAux("comments", r, func(ctx context.Context, progress func(string, int, int)) error {
			t.withThreads(ctx, r, o, progress)
			return nil
		}), nil
	case "overview":
		return t.startAux("overview", r, func(ctx context.Context, progress func(string, int, int)) error {
			progress("overview", 0, 1)
			if _, err := t.updateResult(r.Key, func(r *PRResult) { r.Overview = nil }); err != nil {
				return err
			}
			_, err := t.overview(ctx, r.Key, jo)
			return err
		}), nil
	case "sequence":
		return t.startAux("sequence", r, func(ctx context.Context, progress func(string, int, int)) error {
			progress("sequence", 0, 1)
			if _, err := t.updateResult(r.Key, func(r *PRResult) { r.Sequence = nil }); err != nil {
				return err
			}
			_, err := t.sequence(ctx, r.Key, jo)
			return err
		}), nil
	}
	return nil, fmt.Errorf("can't analyze %q again (units, all, threads, overview or sequence)", req.What)
}

// startAux runs work on result r as a job, whose result is r again.
func (t *triager) startAux(kind string, r *PRResult, work func(ctx context.Context, progress func(string, int, int)) error) *job {
	j, ctx, progress := t.newJob(kind, orDefault(r.PR.URL, localSrcOf(r.PR)))
	go func() {
		defer t.saveJobLog(j)
		err := work(ctx, progress)
		j.finish(err)
		t.mu.Lock()
		defer t.mu.Unlock()
		j.Cancelable = false
		if j.cancelled {
			j.Status = "cancelled"
			return
		}
		if err != nil {
			j.Status, j.Error = "error", err.Error()
			log.Printf("%s %s: %v", kind, r.Key, err)
			return
		}
		j.Status, j.Key = "done", r.Key
	}()
	return j
}

// threadAction is POST /api/results/{key}/threads/{action}: a reply on, or
// the resolution of, one of the result's review threads.
func (t *triager) threadAction(ctx context.Context, r *PRResult, action, threadID, body string) (map[string]any, error) {
	if r.PR.LocalPath != "" && r.PR.URL == "" {
		return nil, errors.New("a local review has no review threads")
	}
	var th *triage.Thread
	for _, u := range resultUnits(r) {
		for i := range u.Threads {
			if u.Threads[i].ID == threadID {
				th = &u.Threads[i]
			}
		}
	}
	if th == nil {
		return nil, fmt.Errorf("no review thread %q on this PR", threadID)
	}
	var query string
	args := []string{"-f", "id=" + threadID}
	switch action {
	case "reply":
		if strings.TrimSpace(body) == "" {
			return nil, errors.New("the reply is empty")
		}
		query = `mutation($id: ID!, $body: String!) { addPullRequestReviewThreadReply(input: {pullRequestReviewThreadId: $id, body: $body}) { comment { url } } }`
		args = append(args, "-f", "body="+body)
	case "resolve":
		query = `mutation($id: ID!) { resolveReviewThread(input: {threadId: $id}) { thread { isResolved } } }`
	default:
		return nil, fmt.Errorf("unknown thread action %q", action)
	}
	if t.opts.reviewDryRun {
		return map[string]any{"dry_run": true, "thread": th.URL, "action": action, "body": body}, nil
	}
	cmd := proc.CommandContext(ctx, "gh", append([]string{"api", "graphql", "--hostname", r.PR.HostName(), "-f", "query=" + query}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if gerr := triage.GHError(err, stderr.Bytes()); gerr != nil {
			return nil, gerr
		}
		return nil, fmt.Errorf("gh api graphql: %w: %s %s", err, strings.TrimSpace(string(out)), strings.TrimSpace(stderr.String()))
	}
	var resp struct {
		Data struct {
			Reply struct {
				Comment struct {
					URL string `json:"url"`
				} `json:"comment"`
			} `json:"addPullRequestReviewThreadReply"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, err
	}
	if len(resp.Errors) > 0 {
		return nil, errors.New(resp.Errors[0].Message)
	}
	return map[string]any{"thread": th.URL, "action": action, "url": resp.Data.Reply.Comment.URL}, nil
}

func (t *triager) chatActionRoutes(mux *http.ServeMux) {
	t.chatConvRoutes(mux)
	t.usageRoutes(mux)
	mux.HandleFunc("POST /api/results/{key}/reanalyze", func(w http.ResponseWriter, r *http.Request) {
		var req reanalyzeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		res, err := t.Load(r.PathValue("key"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		j, err := t.reanalyze(res, req)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 202, t.snapshot(j))
	})
	mux.HandleFunc("POST /api/results/{key}/threads/{action}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Thread string `json:"thread"`
			Body   string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeErr(w, 400, err)
			return
		}
		res, err := t.Load(r.PathValue("key"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		out, err := t.threadAction(r.Context(), res, r.PathValue("action"), in.Thread, in.Body)
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		writeJSON(w, 200, out)
	})
}

// chatToolActions is the reply tool's actions property.
func chatToolActions() map[string]any {
	return map[string]any{
		"type":        "array",
		"description": "Actions to propose; [] for none.",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "enum": chatActionNames()},
				"args": map[string]any{"type": "string", "description": "The arguments, as a JSON object."},
				"why":  map[string]any{"type": "string", "description": "One short line for the reader: what it does and why."},
			},
			"required": []string{"name", "args", "why"},
		},
	}
}
