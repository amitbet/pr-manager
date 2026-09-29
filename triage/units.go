package triage

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"github.com/amitbet/pr-manager/codemap/decls"
)

// Unit is the thing that gets a bucket: all hunks of a source file that
// fall inside the same declaration (git's hunk function context when a Go
// file can't be parsed), or the whole file for anything else. Go units
// follow top-level declarations, TS/JS units top-level names, Java, Python,
// C# and Rust units the innermost type, method, property, function or impl.
// Added files stay whole too unless their diff is over the per-unit limit.
type Unit struct {
	ID     string     `json:"id"`
	File   string     `json:"file"`
	Symbol string     `json:"symbol,omitempty"`
	Status FileStatus `json:"status"`
	Line   int        `json:"line"` // first changed line in the new file
	Hunks  []Hunk     `json:"-"`
	// Related lists the other units in the same change, so the model knows
	// what it can't see.
	Related []string `json:"-"`
	// ReviewContext is the rest of the PR as the review prompt shows it
	// (see setReviewContext).
	ReviewContext string `json:"-"`

	Decision Decision `json:"decision"`
	Summary  string   `json:"summary,omitempty"`
	Headline string   `json:"headline,omitempty"`
	// Focus lists what a reviewer should verify (human units only).
	Focus []string `json:"focus,omitempty"`

	// Impact is the code-map view of the touched code (nil without a map).
	Impact *Impact `json:"impact,omitempty"`
	// Likelihood is how likely the change is to go wrong.
	Likelihood *Likelihood `json:"likelihood,omitempty"`
	// Reviewed is set when the summarize/review step answered; Issues and
	// Attention (0-100, from issue severity) are only meaningful then.
	Reviewed  bool    `json:"reviewed,omitempty"`
	Issues    []Issue `json:"issues,omitempty"`
	Attention int     `json:"attention"`
	// Threads are the open review threads on GitHub that fall in this
	// unit (see Thread). They don't change its bucket.
	Threads []Thread `json:"threads,omitempty"`
	// Lint is what static analysis reported on the lines this unit adds
	// (see Linter). Deterministic, so it is shown whether or not the
	// review agrees with it.
	Lint []LintFinding `json:"lint,omitempty"`
	// Score is how the bucket was picked (nil for units never classified).
	Score *Score `json:"score,omitempty"`
	// CarriedFrom is set when the unit kept the review an earlier run of
	// the same change earned, because neither its diff nor anything it was
	// judged against moved. It names that run (see PlanCarryOver).
	CarriedFrom string `json:"carried_from,omitempty"`
}

func (u *Unit) Diff() string {
	parts := make([]string, len(u.Hunks))
	for i, h := range u.Hunks {
		parts[i] = h.String()
	}
	return strings.Join(parts, "\n")
}

func (u *Unit) Added() (add, del int) {
	for _, h := range u.Hunks {
		for _, l := range h.Lines {
			if strings.HasPrefix(l, "+") {
				add++
			} else if strings.HasPrefix(l, "-") {
				del++
			}
		}
	}
	return
}

// ContentFunc returns a file's contents at the head revision, or an error
// if unavailable (the unit then falls back to per-file grouping).
type ContentFunc func(path string) ([]byte, error)

// BuildUnits groups each file's hunks into units. An added file is one
// unit: its declarations have no map history to rank them apart and make
// sense only together. Added files whose diff exceeds maxChars (0: no
// limit) are split like modified ones so no part gets truncated.
func BuildUnits(files []FileDiff, content ContentFunc, maxChars int) []*Unit {
	var units []*Unit
	for _, f := range files {
		if len(f.Hunks) == 0 {
			units = append(units, &Unit{ID: f.Path, File: f.Path, Status: f.Status})
			continue
		}
		if f.Status == StatusAdded {
			u := &Unit{ID: f.Path, File: f.Path, Status: f.Status, Line: firstChangedLine(f.Hunks[0]), Hunks: f.Hunks}
			if maxChars <= 0 || len(u.Diff()) <= maxChars {
				units = append(units, u)
				continue
			}
		}
		isGo := strings.HasSuffix(f.Path, ".go")
		var ds []declRange
		if (isGo || decls.HasDecls(f.Path)) && f.Status != StatusDeleted && content != nil {
			if src, err := content(f.Path); err == nil {
				ds = declRanges(f.Path, src)
			}
		}
		byKey := map[string]*Unit{}
		var order []string
		for _, h := range f.Hunks {
			for _, part := range splitHunk(h, ds, isGo) {
				u, ok := byKey[part.sym]
				if !ok {
					id := f.Path
					if part.sym != "" {
						id = f.Path + ":" + part.sym
					}
					u = &Unit{ID: id, File: f.Path, Symbol: part.sym, Status: f.Status, Line: firstChangedLine(part.hunk)}
					byKey[part.sym] = u
					order = append(order, part.sym)
				}
				u.Hunks = append(u.Hunks, part.hunk)
			}
		}
		for _, k := range order {
			units = append(units, byKey[k])
		}
	}
	return units
}

type declRange struct {
	name       string
	start, end int
}

// declRanges names the declarations of a Go, TS/JS, Java, Python, C#, Rust
// or generic-parser (shell, PowerShell, C, C++, PHP, Scala, Kotlin, Ruby,
// Swift, Dart) file.
func declRanges(path string, src []byte) []declRange {
	if strings.HasSuffix(path, ".go") {
		return goDecls(src)
	}
	var out []declRange
	for _, d := range decls.Names(path, src) {
		out = append(out, declRange{name: d.Name, start: d.Start, end: d.End})
	}
	return out
}

func goDecls(src []byte) []declRange {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution|parser.ParseComments)
	if err != nil {
		return nil
	}
	var out []declRange
	for _, d := range file.Decls {
		start := d.Pos()
		var name string
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Doc != nil {
				start = d.Doc.Pos()
			}
			name = d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				name = fmt.Sprintf("(%s).%s", recvName(d.Recv.List[0].Type), name)
			}
		case *ast.GenDecl:
			if d.Doc != nil {
				start = d.Doc.Pos()
			}
			name = d.Tok.String()
			if len(d.Specs) > 0 {
				switch s := d.Specs[0].(type) {
				case *ast.TypeSpec:
					name = "type " + s.Name.Name
				case *ast.ValueSpec:
					name = d.Tok.String() + " " + s.Names[0].Name
				case *ast.ImportSpec:
					name = "imports"
				}
			}
		}
		out = append(out, declRange{name: name, start: fset.Position(start).Line, end: fset.Position(d.End()).Line})
	}
	return out
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return "*" + recvName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	}
	return "?"
}

type hunkPart struct {
	sym  string
	hunk Hunk
}

// splitHunk cuts a hunk at declaration boundaries so edits to two
// functions that git merged into one hunk become two units. A line belongs
// to the innermost declaration holding it (Java, Python, C# and Rust
// ranges nest); lines outside any declaration stay with the current part.
// Parts with no +/- lines are dropped. Files without declarations stay
// whole, except Go files that don't parse, which use the function context
// git prints after the hunk header.
func splitHunk(h Hunk, ds []declRange, isGo bool) []hunkPart {
	if ds == nil {
		if !isGo {
			return []hunkPart{{hunk: h}}
		}
		_, ctx, _ := strings.Cut(h.Header[2:], "@@")
		return []hunkPart{{sym: strings.TrimSpace(ctx), hunk: h}}
	}
	declAt := func(n int) (string, bool) {
		best := -1
		for i, d := range ds {
			if n >= d.start && n <= d.end && (best < 0 || d.end-d.start < ds[best].end-ds[best].start) {
				best = i
			}
		}
		if best < 0 {
			return "", false
		}
		return ds[best].name, true
	}
	var parts []hunkPart
	var cur *hunkPart
	changed := false
	flush := func() {
		if cur != nil && changed {
			cur.hunk.Header = fmt.Sprintf("@@ -%d,%d +%d,%d @@ %s", cur.hunk.OldStart, cur.hunk.OldLines, cur.hunk.NewStart, cur.hunk.NewLines, cur.sym)
			parts = append(parts, *cur)
		}
		cur, changed = nil, false
	}
	oldN, newN := h.OldStart, h.NewStart
	for _, l := range h.Lines {
		sym, inDecl := declAt(newN)
		if cur == nil || (inDecl && sym != cur.sym) {
			flush()
			cur = &hunkPart{sym: sym, hunk: Hunk{OldStart: oldN, NewStart: newN}}
		}
		cur.hunk.Lines = append(cur.hunk.Lines, l)
		switch {
		case strings.HasPrefix(l, "+"):
			changed = true
			cur.hunk.NewLines++
			newN++
		case strings.HasPrefix(l, "-"):
			changed = true
			cur.hunk.OldLines++
			oldN++
		case strings.HasPrefix(l, "\\"):
		default:
			cur.hunk.OldLines++
			cur.hunk.NewLines++
			oldN++
			newN++
		}
	}
	flush()
	return parts
}

// firstChangedLine maps the first +/- line of a hunk to a new-file line.
func firstChangedLine(h Hunk) int {
	n := h.NewStart
	for _, l := range h.Lines {
		switch {
		case strings.HasPrefix(l, "+"), strings.HasPrefix(l, "-"):
			return n
		case strings.HasPrefix(l, " "):
			n++
		}
	}
	return h.NewStart
}
