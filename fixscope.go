package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/amitbet/pr-manager/triage"
)

// A fix asked for a nil check can come back with the nil check, a new
// helper, an option "for flexibility" and a tidied-up neighbour: each part
// looks fine and the diff triples. patchScope already keeps a fix to the
// targeted files; fixScope keeps it near the targeted code and to its
// size, before the audit (fixaudit.go) looks at what is left. It runs on
// what a round's patch changed, in Go, and costs no model call. A patch
// it rejects goes back to the fixer once, with what was out of scope;
// what the second answer keeps is applied, and the audit judges it.
//
//   - locality: each changed hunk is inside a targeted unit's declaration,
//     or within scopeSlack lines of the targeted lines
//   - size: the lines added stay under twice a budget worked out from the
//     issues and the size of the code they are in
//   - new API: a new exported Go declaration or command-line flag is named,
//     with why, in the fixer's extra
//
// A change the reader asked for (fixChange) is new code by request: only
// patchScope applies to it.

const (
	scopeSlack       = 20  // lines around a targeted line a fix may touch
	scopeLinesIssue  = 15  // added lines each issue's fix is budgeted
	scopeUnitShare   = 0.3 // of the targeted units' changed lines
	scopeBudgetFloor = 30
	scopeBudgetCap   = 400
)

// fixScope checks what a round changed (changed, from the checkout in
// dir) against the issues it worked on, which r's units hold. extra is
// what the fixer said it added beyond the fix. It returns what was out of
// scope, as a message for the fixer, or "".
func fixScope(dir string, r *PRResult, issues []targetedIssue, changed []triage.FileDiff, extra string) string {
	if changeOf(issues) != nil || len(changed) == 0 {
		return ""
	}
	var problems []string
	if far := farHunks(dir, r, issues, changed); len(far) > 0 {
		problems = append(problems, fmt.Sprintf("it changes code away from the issues, at %s: keep the fix inside the functions the issues are in, or say in extra why that code had to change", strings.Join(far, ", ")))
	}
	if added, budget := addedLines(changed), scopeBudget(r, issues); added > 2*budget {
		problems = append(problems, fmt.Sprintf("it adds %d lines for %d issue(s), more than twice the %d expected: keep to the fix, and list in extra anything you can't do without", added, len(issues), budget))
	}
	if names := unnamedAPI(changed, extra); len(names) > 0 {
		problems = append(problems, fmt.Sprintf("it adds %s, not named in extra: drop it, or name it in extra with why the fix needs it", strings.Join(names, ", ")))
	}
	if len(problems) == 0 {
		return ""
	}
	return "The change goes beyond the issues: " + strings.Join(problems, "; ") + ". Make the smallest change that fixes them."
}

// scopeBudget is the lines a round's fix is expected to add.
func scopeBudget(r *PRResult, issues []targetedIssue) int {
	units := unitsByID(r)
	seen := map[string]bool{}
	unitLines := 0
	for _, x := range issues {
		if u := units[x.UnitID]; u != nil && !seen[u.ID] {
			seen[u.ID] = true
			add, del := u.Added()
			unitLines += add + del
		}
	}
	b := scopeLinesIssue*len(issues) + int(scopeUnitShare*float64(unitLines))
	return min(scopeBudgetCap, max(scopeBudgetFloor, b))
}

// farHunks are the changed hunks, as file:line, that are neither inside a
// targeted unit's declaration nor near a targeted line. A file whose
// targeted lines aren't known (a whole-file unit, an issue on no line) is
// all in scope.
func farHunks(dir string, r *PRResult, issues []targetedIssue, changed []triage.FileDiff) []string {
	units := unitsByID(r)
	type span struct{ from, to int }
	near := map[string][]span{}
	open := map[string]bool{} // files where anything goes
	// moved is where pre-fix line l of file is in the patched file: the
	// issues' and units' lines are r's, the changed hunks and src the
	// patch's.
	moved := func(file string, l int) int {
		shift := 0
		for _, f := range changed {
			if f.Path != file {
				continue
			}
			for _, h := range f.Hunks {
				if h.OldStart+max(h.OldLines, 1) <= l {
					shift += h.NewLines - h.OldLines
				}
			}
		}
		return l + shift
	}
	for _, x := range issues {
		var lines []int
		if x.Issue.Line > 0 {
			lines = append(lines, x.Issue.Line)
		}
		u := units[x.UnitID]
		if u != nil {
			for _, h := range u.Hunks {
				lines = append(lines, h.NewStart, h.NewStart+max(h.NewLines, 1)-1)
			}
		}
		if len(lines) == 0 || (u != nil && u.Symbol == "") {
			open[x.File] = true
			continue
		}
		src, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(x.File)))
		for _, l := range lines {
			l = moved(x.File, l)
			near[x.File] = append(near[x.File], span{l - scopeSlack, l + scopeSlack})
			if from, to, ok := triage.DeclSpan(x.File, src, l); ok {
				near[x.File] = append(near[x.File], span{from, to})
			}
		}
	}
	var out []string
	for _, f := range changed {
		if open[f.Path] {
			continue
		}
		for _, h := range f.Hunks {
			from := h.NewStart
			to := from + max(h.NewLines, 1) - 1
			ok := false
			for _, s := range near[f.Path] {
				if from <= s.to && to >= s.from {
					ok = true
					break
				}
			}
			if !ok {
				out = append(out, fmt.Sprintf("%s:%d", f.Path, from))
			}
		}
	}
	return out
}

var (
	newGoExported = regexp.MustCompile(`^\+\s*(?:func\s+(?:\([^)]*\)\s*)?|type\s+|var\s+|const\s+)([A-Z]\w*)`)
	newFlag       = regexp.MustCompile(`\b(?:flag|fs|flags)\.(?:String|Bool|Int|Int64|Uint|Float64|Duration|Func|Var)(?:Var)?\(\s*(?:&[\w.]+,\s*)?"([\w-]+)"`)
)

// unnamedAPI are the exported Go declarations and command-line flags the
// change adds that extra doesn't name.
func unnamedAPI(changed []triage.FileDiff, extra string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range changed {
		if !strings.HasSuffix(f.Path, ".go") || strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if !strings.HasPrefix(l, "+") {
					continue
				}
				what := ""
				if m := newGoExported.FindStringSubmatch(l); m != nil {
					what = m[1]
				} else if m := newFlag.FindStringSubmatch(l); m != nil {
					what = "-" + m[1]
				}
				if what == "" || seen[what] || strings.Contains(extra, strings.TrimPrefix(what, "-")) {
					continue
				}
				seen[what] = true
				label := "the exported " + what
				if strings.HasPrefix(what, "-") {
					label = "the flag " + what
				}
				out = append(out, label)
			}
		}
	}
	sort.Strings(out)
	return out
}

// scopeRubric is what the fixer prompts say a fix doesn't add.
const scopeRubric = `Keep the fix to what the issues need. Don't add, unless the fix can't be made without it: a new helper or abstraction, a new option, flag, parameter or config field, a check for a case that can't happen, a refactor, rename, reformat or tidy-up of nearby code, comments beyond what the fix needs, or tests beyond the issue. If you do add one, name it in extra with why the fix needs it. A patch that changes code away from the issues, adds an exported name or a flag it doesn't name in extra, or is much larger than the issues need, is sent back.`
