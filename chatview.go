package main

import (
	"fmt"
	"strings"

	"github.com/amitbet/pr-manager/llm"
)

// What the reader is looking at goes with their newest turn, not into the
// material: it changes with every question, and the material should only
// change when the result does (a resumed codex session is sent it again
// when it changes, see llm.Session).

// chatSelection is the text the reader selected on the page: in a diff,
// with its file and the lines on each side.
type chatSelection struct {
	Path     string `json:"path,omitempty"`
	OldStart int    `json:"old_start,omitempty"`
	OldEnd   int    `json:"old_end,omitempty"`
	NewStart int    `json:"new_start,omitempty"`
	NewEnd   int    `json:"new_end,omitempty"`
	Unit     string `json:"unit,omitempty"`
	Text     string `json:"text"`
}

// chatField is a text box open on the page, which the agent can fill
// (fill_field): a review comment being written, the reason of a
// dismissal, the review's summary.
type chatField struct {
	Name    string `json:"name"` // comment | dismiss_reason | review_summary
	Text    string `json:"text"`
	Focused bool   `json:"focused,omitempty"` // the reader was last typing in it
	// A comment's place, and its draft when it edits one.
	Path  string `json:"path,omitempty"`
	Side  string `json:"side,omitempty"`
	Line  int    `json:"line,omitempty"`
	Draft string `json:"draft,omitempty"`
	// A dismissal's claim: its unit, kind and index.
	Unit  string `json:"unit,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Index int    `json:"index,omitempty"`
}

const chatSelectionMax = 6000

// chatViewText says what is on screen, for the reader's newest turn.
func chatViewText(v chatView) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("[On screen: %s", orDefault(v.Where, "unknown"))
	if v.Path != "" {
		w("; open file %s", v.Path)
	}
	if len(v.Units) > 0 {
		w("; units %s", strings.Join(v.Units, ", "))
	}
	w(".]")
	if s := v.Selection; s != nil && strings.TrimSpace(s.Text) != "" {
		w("\n[Selected")
		if s.Path != "" {
			w(" in %s", s.Path)
			if s.NewStart > 0 {
				w(", new lines %s", lineRange(s.NewStart, s.NewEnd))
			}
			if s.OldStart > 0 {
				w(", old lines %s", lineRange(s.OldStart, s.OldEnd))
			}
		}
		if s.Unit != "" {
			w(", unit %s", s.Unit)
		}
		w(" (\"this\" most likely means it):]\n```\n%s\n```", clipText(strings.TrimRight(s.Text, "\n"), chatSelectionMax))
	}
	for _, f := range v.Fields {
		w("\n[Open text box %s", f.Name)
		switch f.Name {
		case "comment":
			side := "new"
			if f.Side == "LEFT" {
				side = "old"
			}
			w(": a review comment on %s:%d (%s file)", f.Path, f.Line, side)
			if f.Draft != "" {
				w(", editing pending comment %s", f.Draft)
			}
		case "dismiss_reason":
			w(": why %s #%d of unit %s is not a problem", f.Kind, f.Index, f.Unit)
		case "review_summary":
			w(": the summary of the review to submit")
		}
		if f.Focused {
			w("; the reader was typing in it")
		}
		if strings.TrimSpace(f.Text) == "" {
			w("; empty.]")
		} else {
			w("; it says:]\n```\n%s\n```", clipText(f.Text, chatSelectionMax))
		}
	}
	return b.String()
}

func lineRange(a, b int) string {
	if b <= a {
		return fmt.Sprint(a)
	}
	return fmt.Sprintf("%d-%d", a, b)
}

// chatDigest recalls the turns left out for length: the reader's
// questions and what the actions did, so the start of a long conversation
// isn't lost.
func chatDigest(dropped []llm.ChatMessage) string {
	if len(dropped) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[Earlier in this conversation, %d turns left out for length. The reader asked, and actions did:", len(dropped))
	n := 0
	for _, m := range dropped {
		if m.Role != "user" {
			continue
		}
		for _, part := range strings.Split(m.Content, "\n\n") {
			if part = strings.TrimSpace(part); part == "" || strings.HasPrefix(part, "[On screen:") {
				continue
			}
			if n++; n > 40 {
				break
			}
			fmt.Fprintf(&b, "\n- %s", clipText(strings.ReplaceAll(part, "\n", " "), 300))
		}
	}
	if n == 0 {
		return ""
	}
	b.WriteString("]")
	return b.String()
}
