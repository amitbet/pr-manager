package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/amitbet/pr-manager/triage"
)

// errIssuesLeft is the fix command's exit 3: issues at -fail-on or worse
// are still there after the rounds.
var errIssuesLeft = errors.New("review issues remain after fixing")

// fixSummary is what the fix command reports next to the triage report.
type fixSummary struct {
	Source string `json:"source"`
	// Rounds is how many fix rounds applied a patch.
	Rounds int    `json:"rounds"`
	Dir    string `json:"dir,omitempty"`
	Branch string `json:"branch,omitempty"`
	// Commit is the commit the fixes were committed as, with -commit.
	Commit  string `json:"commit,omitempty"`
	Warning string `json:"warning,omitempty"`
	// IssuesBefore and IssuesLeft count live review issues; Failing is how
	// many of those left are at -fail-on or worse.
	IssuesBefore int            `json:"issues_before"`
	IssuesLeft   int            `json:"issues_left"`
	Failing      int            `json:"failing"`
	Report       *triage.Report `json:"report"`
}

// runFixCmd reviews a local checkout (-C) or a GitHub PR (-pr), fixes
// every issue the review found, re-reviews what the fixes touched and
// fixes again for up to -rounds rounds: what the UI's Fix all issues
// does, with no UI, for a terminal before a PR or a CI job on one.
func runFixCmd(ctx context.Context, o options) error {
	if o.fixRounds < 1 || o.fixRounds > 10 {
		return errors.New("-rounds must be between 1 and 10")
	}
	if o.failOn != "off" && !slices.Contains([]string{"low", "medium", "high", "critical"}, o.failOn) {
		return fmt.Errorf("-fail-on must be low, medium, high, critical or off, not %q", o.failOn)
	}
	location := map[string]string{"place": "branch", "worktree": "worktree"}[o.fixIn]
	if location == "" {
		return fmt.Errorf("-in must be place or worktree, not %q", o.fixIn)
	}
	if o.pr != "" && o.fixInSet && location == "branch" {
		return errors.New("-in place needs a local checkout; a PR is fixed in a worktree under -cache")
	}
	if o.pr != "" {
		location = "worktree"
	}
	t, err := newTriager(o)
	if err != nil {
		return err
	}
	if t.options(jobOptions{}).summarizer == "off" {
		return errors.New("fixing needs a reviewer: set -summarizer, or log in to codex or claude, or set an API key")
	}
	progress := stageLogger(os.Stderr)

	var r *PRResult
	src := o.pr
	if o.pr != "" {
		ref, err := triage.ParsePRRef(o.pr)
		if err != nil {
			return err
		}
		r, err = t.Run(ctx, ref, jobOptions{Force: o.force}, progress)
		if err != nil {
			return err
		}
	} else {
		if o.baseSet {
			baseOverride = o.base
		}
		dir, err := checkoutRoot(o.dir)
		if err != nil {
			return err
		}
		src = dir
		if r, err = t.RunLocal(ctx, dir, jobOptions{Force: o.force}, progress); err != nil {
			return err
		}
	}
	if err := fixable(r, location); err != nil {
		return err
	}
	req := fixRequest{Key: r.Key, Location: location, All: true, Comments: o.fixComments, Recursive: o.fixRounds > 1, MaxRounds: o.fixRounds}
	if r.PR.LocalPath != "" {
		dirty, err := hasUncommitted(r.PR.LocalPath)
		if err != nil {
			return err
		}
		switch {
		case !dirty:
		case o.fixCommit:
			req.Uncommitted = "commit"
		case location == "branch":
			req.Uncommitted = "branch"
		default:
			return errors.New("the checkout has uncommitted changes, which a worktree would not have; commit them, pass -commit, or fix with -in place")
		}
	}

	sum := &fixSummary{Source: src, IssuesBefore: liveIssueCount(r, "")}
	final := r
	if len(fixTargets(r, req)) == 0 {
		fmt.Fprintln(os.Stderr, "fix: the review found nothing to fix")
	} else {
		var id [6]byte
		_, _ = rand.Read(id[:])
		res, err := t.runFix(ctx, hex.EncodeToString(id[:]), r, req, progress)
		if err != nil {
			return err
		}
		final = res
		sum.Rounds = res.FixRounds - r.FixRounds
		sum.Dir, sum.Branch, sum.Warning = res.LocalFixDir, res.LocalFixBranch, res.FixWarning
		if o.fixCommit && sum.Dir != "" {
			if sum.Commit, err = commitFixes(ctx, t.options(jobOptions{}), sum.Dir); err != nil {
				return err
			}
		}
	}
	sum.IssuesLeft = liveIssueCount(final, "")
	if o.failOn != "off" {
		sum.Failing = liveIssueCount(final, o.failOn)
	}
	if sum.Report, err = resultReport(final, o.reviewBudget); err != nil {
		return err
	}
	if err := writeFixReport(o, sum); err != nil {
		return err
	}
	printFixSummary(os.Stderr, sum, o.failOn)
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		if err := appendStepSummary(path, sum, o.failOn); err != nil {
			fmt.Fprintln(os.Stderr, "fix: write the step summary:", err)
		}
	}
	if sum.Failing > 0 {
		return errIssuesLeft
	}
	return nil
}

// stageLogger prints each new stage of a job, and each fix and check
// round, as the UI's banner shows them.
func stageLogger(w io.Writer) func(string, int, int) {
	last := ""
	return func(stage string, done, total int) {
		line := stage
		if (stage == "fix" || stage == "check") && total > 0 {
			line = fmt.Sprintf("%s round %d/%d", stage, done, total)
		}
		if line != last {
			fmt.Fprintf(w, "%s...\n", line)
			last = line
		}
	}
}

// liveIssueCount counts the undismissed review issues in r at min or
// worse; every one when min is "".
func liveIssueCount(r *PRResult, min string) int {
	n := 0
	for _, f := range r.Files {
		for _, u := range f.Units {
			for _, is := range u.Issues {
				if is.Live() && (min == "" || triage.SeverityAtLeast(is.Severity, min)) {
					n++
				}
			}
		}
	}
	return n
}

// commitFixes commits what the fix left uncommitted in dir and returns
// the new commit, or "" when the fix changed nothing.
func commitFixes(ctx context.Context, o options, dir string) (string, error) {
	dirty, err := hasUncommitted(dir)
	if err != nil || !dirty {
		return "", err
	}
	if err := commitLocal(ctx, o, dir); err != nil {
		return "", err
	}
	head, err := triage.Git(dir, "rev-parse", "HEAD")
	return strings.TrimSpace(head), err
}

// resultReport is r as the triage command reports it, bucketed under
// budget.
func resultReport(r *PRResult, budget string) (*triage.Report, error) {
	rep := &triage.Report{Base: shortOid(r.PR.BaseOid), Head: shortOid(r.PR.HeadOid)}
	for _, f := range r.Files {
		for _, u := range f.Units {
			rep.Units = append(rep.Units, u.Unit)
		}
	}
	return rep, triage.Rebucket(rep.Units, r.tierPolicy(), budget)
}

func shortOid(oid string) string {
	if len(oid) > 10 {
		return oid[:10]
	}
	return oid
}

func writeFixReport(o options, sum *fixSummary) error {
	w := io.Writer(os.Stdout)
	if o.outFile != "" {
		f, err := os.Create(o.outFile)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	if o.out == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(sum)
	}
	fmt.Fprint(w, fixMarkdown(sum, o.failOn))
	triage.RenderMarkdown(w, sum.Report)
	return nil
}

// fixMarkdown is the fix's own section, above the triage report.
func fixMarkdown(sum *fixSummary, failOn string) string {
	var b strings.Builder
	b.WriteString("## PR fix\n\n")
	if sum.Rounds == 0 {
		fmt.Fprintf(&b, "No fix applied; the review found %d issue(s).\n", sum.IssuesLeft)
	} else {
		fmt.Fprintf(&b, "%d fix round(s): %d issue(s) before, %d left.", sum.Rounds, sum.IssuesBefore, sum.IssuesLeft)
		if sum.Branch != "" {
			fmt.Fprintf(&b, " Changes are on `%s`", sum.Branch)
			if sum.Commit != "" {
				fmt.Fprintf(&b, ", committed as `%s`", shortOid(sum.Commit))
			} else {
				b.WriteString(", uncommitted")
			}
			fmt.Fprintf(&b, ", in `%s`.", sum.Dir)
		}
		b.WriteString("\n")
	}
	if failOn != "off" && sum.Failing > 0 {
		fmt.Fprintf(&b, "\n**%d issue(s) at %s or worse remain.**\n", sum.Failing, failOn)
	}
	if sum.Warning != "" {
		fmt.Fprintf(&b, "\n> %s\n", sum.Warning)
	}
	b.WriteString("\n")
	return b.String()
}

func printFixSummary(w io.Writer, sum *fixSummary, failOn string) {
	fmt.Fprintf(w, "fix: %d round(s), %d issue(s) before, %d left", sum.Rounds, sum.IssuesBefore, sum.IssuesLeft)
	if failOn != "off" {
		fmt.Fprintf(w, ", %d at %s or worse", sum.Failing, failOn)
	}
	fmt.Fprintln(w)
	if sum.Dir != "" {
		fmt.Fprintf(w, "fix: changes in %s (branch %s)\n", sum.Dir, sum.Branch)
	}
	if sum.Commit != "" {
		fmt.Fprintf(w, "fix: committed %s\n", shortOid(sum.Commit))
	}
	if sum.Warning != "" {
		fmt.Fprintln(w, "fix: warning:", sum.Warning)
	}
}

// appendStepSummary adds the report to a GitHub Actions job summary.
func appendStepSummary(path string, sum *fixSummary, failOn string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprint(f, fixMarkdown(sum, failOn))
	triage.RenderMarkdown(f, sum.Report)
	return nil
}
