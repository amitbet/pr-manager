package triage

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func RenderJSON(w io.Writer, r *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func RenderMarkdown(w io.Writer, r *Report) {
	c := r.Counts()
	fmt.Fprintf(w, "## PR triage\n\n`%s...%s`: **%d** need human review, **%d** summarized, **%d** skipped.\n\n",
		r.Base, r.Head, c[BucketHuman], c[BucketSkim], c[BucketNone])
	d, l, att := r.Scores()
	var top []string
	if d != nil {
		top = append(top, fmt.Sprintf("Highest code-map impact: **%d (%s)** at `%s`.", d.Score, d.Level, d.Basis))
	}
	if l != nil && l.Score > 0 {
		s := fmt.Sprintf("Highest likelihood: **%d (%s)**", l.Score, l.Level)
		if len(l.Factors) > 0 {
			s += ", " + l.Factors[0].Detail
		}
		top = append(top, s+".")
	}
	if len(top) > 0 {
		fmt.Fprintf(w, "%s Highest review attention: **%d**.\n\n", strings.Join(top, " "), att)
	}

	section := func(b Bucket) []*Unit {
		var out []*Unit
		for _, u := range r.Units {
			if u.Decision.Bucket == b {
				out = append(out, u)
			}
		}
		return out
	}

	if us := section(BucketHuman); len(us) > 0 {
		fmt.Fprintf(w, "### Needs human review\n\n")
		for _, u := range us {
			fmt.Fprintf(w, "- %s: %s%s\n", unitLink(u), withHeadline(u, u.Decision.Reason), riskSuffix(u))
			if u.Summary != "" {
				fmt.Fprintf(w, "  - %s\n", u.Summary)
			}
			for _, f := range u.Focus {
				fmt.Fprintf(w, "  - check: %s\n", f)
			}
			writeIssues(w, u)
			if len(u.Decision.Escalated) > 0 {
				fmt.Fprintf(w, "  - escalated: %s\n", strings.Join(u.Decision.Escalated, "; "))
			}
			writeWhy(w, u)
		}
		fmt.Fprintln(w)
	}
	if us := section(BucketSkim); len(us) > 0 {
		fmt.Fprintf(w, "### Skim summaries\n\n")
		for _, u := range us {
			s := u.Summary
			if s == "" {
				s = u.Decision.Reason
			}
			fmt.Fprintf(w, "- %s: %s\n", unitLink(u), withHeadline(u, s))
			for _, f := range u.Focus {
				fmt.Fprintf(w, "  - check: %s\n", f)
			}
			writeIssues(w, u)
			writeWhy(w, u)
		}
		fmt.Fprintln(w)
	}
	if us := section(BucketNone); len(us) > 0 {
		fmt.Fprintf(w, "<details><summary>No review needed (%d)</summary>\n\n", len(us))
		for _, u := range us {
			fmt.Fprintf(w, "- %s: %s\n", unitLink(u), u.Decision.Reason)
			writeWhy(w, u)
		}
		fmt.Fprintf(w, "\n</details>\n")
	}
}

func unitLink(u *Unit) string {
	loc := u.File
	if u.Line > 0 {
		loc = fmt.Sprintf("%s:%d", u.File, u.Line)
	}
	add, del := u.Added()
	s := fmt.Sprintf("`%s`", loc)
	if u.Symbol != "" {
		s += fmt.Sprintf(" `%s`", u.Symbol)
	}
	if add+del > 0 {
		s += fmt.Sprintf(" (+%d/-%d)", add, del)
	}
	if u.Impact.Known() {
		s += fmt.Sprintf(" impact %d", u.Impact.Score)
	}
	if u.Likelihood != nil && !(u.Decision.Source == "rule" && u.Decision.Bucket == BucketNone) {
		s += fmt.Sprintf(" likelihood %d", u.Likelihood.Score)
	}
	return s + " [" + u.Decision.Source + "]"
}

func riskSuffix(u *Unit) string {
	if len(u.Decision.RiskSignals) == 0 {
		return ""
	}
	return " (" + strings.Join(u.Decision.RiskSignals, ", ") + ")"
}

// withHeadline prefixes detail text with the unit's one-line headline.
func withHeadline(u *Unit, detail string) string {
	h := u.Headline
	if h == "" {
		h = u.Decision.Headline
	}
	if h == "" || h == detail {
		return detail
	}
	return "**" + h + "**. " + detail
}

func writeIssues(w io.Writer, u *Unit) {
	for _, is := range u.Issues {
		if !is.Live() {
			continue
		}
		loc := ""
		if is.Line > 0 {
			loc = fmt.Sprintf(" (line %d)", is.Line)
		}
		fmt.Fprintf(w, "  - **%s**%s: %s", is.Severity, loc, is.Title)
		if is.Detail != "" {
			fmt.Fprintf(w, ". %s", is.Detail)
		}
		if is.Scenario != "" {
			fmt.Fprintf(w, " When: %s", is.Scenario)
		}
		if is.Claimed != "" {
			fmt.Fprintf(w, " _(claimed %s; %s)_", is.Claimed, is.Capped)
		}
		fmt.Fprintln(w)
	}
	writeLint(w, u)
}

// writeLint lists the static-analysis findings on the unit's added lines,
// kept apart from the review's issues: they are facts from a tool, not a
// judgement anyone made.
func writeLint(w io.Writer, u *Unit) {
	for _, f := range u.Lint {
		if f.Dismissed {
			continue
		}
		fmt.Fprintf(w, "  - **lint %s** (line %d): [%s] %s\n", f.Severity, f.Line, f.Label(), f.Message)
	}
}

func writeWhy(w io.Writer, u *Unit) {
	if u.Score != nil && u.Decision.Source != "rule" {
		fmt.Fprintf(w, "  - bucket: %s\n", u.Score.Why)
	}
}
