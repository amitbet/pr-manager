package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/amitbet/pr-manager/triage"
)

// Running a change's tests runs its code on this machine. The desktop app
// runs on the reader's own machine, for repositories they chose, so it
// tests by default; serve and the CLI test only when asked (-tests on),
// as a server may be shared. Even then a PR from a fork is tested only
// with -tests all or when its author is on -trusted-authors: a branch in
// the repository itself needed write access to push, a fork's didn't.
// The command is the operator's (-test-cmd, or detected), never set by a
// request or the PR; a detected command such as npm test still runs the
// PR's own scripts.

// testsModes are -tests' values: off, on (local checkouts and PRs whose
// branch is in the repository or whose author is trusted), all.
var testsModes = []string{"off", "on", "all"}

// defaultTestsMode is -tests when it isn't given.
func defaultTestsMode() string {
	if desktopBuild {
		return "on"
	}
	return "off"
}

func checkTestsMode(mode string) error {
	if !slices.Contains(testsModes, mode) {
		return fmt.Errorf("-tests must be %s, not %q", strings.Join(testsModes, ", "), mode)
	}
	return nil
}

// testPolicy is the tests' settings from o, for a run that may test.
func (o options) testPolicy() triage.TestPolicy {
	return triage.TestPolicy{Enabled: true, Command: strings.TrimSpace(o.testCmd), TimeoutSec: o.testTimeout, Network: o.testNetwork}
}

// trustedAuthor reports whether login is on o's trusted authors list.
func (o options) trustedAuthor(login string) bool {
	if login == "" {
		return false
	}
	for _, a := range strings.Split(o.trustedAuthors, ",") {
		if strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(a), "@"), login) {
			return true
		}
	}
	return false
}

// testerFor is the tester for a change, or nil and why it isn't tested.
func testerFor(o options, info *triage.PRInfo) (*triage.Tester, string) {
	switch {
	case o.tests == "" || o.tests == "off":
		return nil, "turned off in Settings"
	case info == nil, info.LocalPath != "", o.tests == "all":
	case info.Fork && !o.trustedAuthor(info.Author):
		return nil, fmt.Sprintf("the PR is from a fork, and @%s isn't a trusted author", info.Author)
	}
	return &triage.Tester{Policy: o.testPolicy()}, ""
}

// testsResult is what a result records about its tests: the run, or why
// there was none.
func testsResult(t *triage.Tester, skipped string) *triage.TestRun {
	if t != nil && t.Result != nil {
		return t.Result
	}
	if skipped == "" {
		return nil
	}
	return &triage.TestRun{Skipped: skipped}
}

// runResultTests runs a saved result's tests now and puts what they found
// on its units: POST /api/results/{key}/tests. The operator's -tests and
// the fork rule still apply: any local web page can POST here.
func (t *triager) runResultTests(ctx context.Context, key string, jo jobOptions) (*PRResult, error) {
	r, err := t.Load(key)
	if err != nil {
		return nil, err
	}
	o := t.options(jo)
	tester, skipped := testerFor(o, r.PR)
	if tester == nil {
		return nil, fmt.Errorf("tests not run: %s", skipped)
	}
	dir, done, err := t.resultCheckout(r)
	if err != nil {
		return nil, err
	}
	defer done()
	src := &triage.Source{}
	for _, f := range r.Files {
		src.Files = append(src.Files, f.FileDiff)
	}
	run := tester.Run(ctx, dir, r.PR.BaseOid, src)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return t.updateResult(key, func(r *PRResult) {
		tp := r.tierPolicy()
		units := resultUnitsWithHunks(r)
		tester.Apply(units, tp)
		for _, u := range units {
			tp.ApplyDismissals(u)
		}
		r.Tests = run
		rep := &triage.Report{Units: units}
		r.Counts = rep.Counts()
		r.Impact, r.Likelihood, r.Attention = rep.Scores()
	})
}

func (t *triager) testsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/results/{key}/tests", func(w http.ResponseWriter, r *http.Request) {
		var jo jobOptions
		if err := json.NewDecoder(r.Body).Decode(&jo); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, 400, err)
			return
		}
		res, err := t.runResultTests(r.Context(), r.PathValue("key"), jo)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, res)
	})
}
