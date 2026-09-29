package triage

import (
	"strings"
	"testing"
)

func seqArgs() map[string]any {
	return map[string]any{
		"title": "Snapshot write during WAL replay",
		"participants": []any{
			map[string]any{"id": "DB", "label": "tsdb.DB"},
			map[string]any{"id": "Head", "label": "tsdb.Head"},
			map[string]any{"id": "Disk", "label": "chunk files", "kind": "external"},
			map[string]any{"id": "Ghost", "label": "never used"},
		},
		"steps": []any{
			map[string]any{"from": "DB", "to": "Head", "text": "replay WAL", "kind": "call"},
			map[string]any{"from": "Head", "to": "Disk", "text": "keep mapped chunks", "kind": "call", "changed": true, "unit": "head.go:(*Head).replay"},
			map[string]any{"from": "Disk", "to": "Head", "text": "chunk refs", "kind": "return"},
			map[string]any{"from": "Head", "to": "Nowhere", "text": "dangling participant"},
			map[string]any{"from": "Head", "to": "Head", "text": "", "kind": "note"},
			map[string]any{"from": "Head", "to": "DB", "text": "replay done", "kind": "return", "changed": true, "unit": "made-up-unit"},
		},
		"note":    "Check the chunk refs survive a restart.",
		"problem": "",
	}
}

func TestDecodeSequenceDropsWhatItCannotDraw(t *testing.T) {
	units := []*Unit{{ID: "head.go:(*Head).replay"}}
	sq := decodeSequence(seqArgs(), units)

	if len(sq.Steps) != 4 {
		t.Fatalf("want 4 usable steps, got %d: %+v", len(sq.Steps), sq.Steps)
	}
	for _, s := range sq.Steps {
		if s.To == "Nowhere" || s.Text == "" {
			t.Errorf("kept an undrawable step: %+v", s)
		}
	}
	for _, a := range sq.Participants {
		if a.ID == "Ghost" {
			t.Error("a column no step goes through should be dropped")
		}
	}
	if len(sq.Participants) != 3 {
		t.Errorf("want 3 columns, got %d", len(sq.Participants))
	}
	if sq.Steps[1].Unit != "head.go:(*Head).replay" {
		t.Errorf("a real unit id should survive, got %q", sq.Steps[1].Unit)
	}
	if last := sq.Steps[len(sq.Steps)-1]; last.Unit != "" {
		t.Errorf("a unit id nothing matches should be cleared, got %q", last.Unit)
	}
}

func TestDecodeSequenceNeedsTwoColumns(t *testing.T) {
	sq := decodeSequence(map[string]any{
		"participants": []any{map[string]any{"id": "Only", "label": "one"}},
		"steps":        []any{map[string]any{"from": "Only", "to": "Only", "text": "does a thing"}},
	}, nil)
	if len(sq.Steps) != 0 {
		t.Error("one column is not a sequence diagram")
	}
}

func TestMermaidIsWellFormed(t *testing.T) {
	sq := decodeSequence(seqArgs(), []*Unit{{ID: "head.go:(*Head).replay"}})
	m := sq.Mermaid()
	if !strings.HasPrefix(m, "sequenceDiagram\n") {
		t.Fatalf("not a Mermaid diagram:\n%s", m)
	}
	if strings.Count(m, "rect rgb") != strings.Count(m, "\n    end\n")+strings.Count(m, "\n        end\n") && !strings.HasSuffix(m, "    end\n") {
		t.Errorf("unbalanced rect blocks:\n%s", m)
	}
	for _, p := range sq.Participants {
		if !strings.Contains(m, "participant "+p.ID+" as ") {
			t.Errorf("participant %s missing:\n%s", p.ID, m)
		}
	}
	if strings.Contains(m, ";") {
		t.Errorf("a semicolon would end the statement early:\n%s", m)
	}
}

func TestMermaidSafeStripsBreakers(t *testing.T) {
	if got := mermaidSafe("a; b <c> #1\nd"); strings.ContainsAny(got, ";<>#\n") {
		t.Errorf("mermaidSafe left a breaker in %q", got)
	}
}

func TestSeqIDKeepsIdentifiersSimple(t *testing.T) {
	for in, want := range map[string]string{
		"tsdb.Head":    "tsdb.Head",
		"my service!":  "my_service",
		"  _weird_  ":  "weird",
		"(*Head).next": "Head_.next",
	} {
		if got := seqID(in); got != want {
			t.Errorf("seqID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestViewSplitsBeforeAndAfter(t *testing.T) {
	units := []*Unit{{ID: "u1"}}
	sq := decodeSequence(map[string]any{
		"participants": []any{
			map[string]any{"id": "A", "label": "a"},
			map[string]any{"id": "B", "label": "b"},
			map[string]any{"id": "Old", "label": "old cache"},
			map[string]any{"id": "New", "label": "new cache"},
		},
		"steps": []any{
			map[string]any{"from": "A", "to": "B", "text": "request", "change": "none"},
			map[string]any{"from": "B", "to": "Old", "text": "read old cache", "change": "removed", "unit": "u1"},
			map[string]any{"from": "B", "to": "New", "text": "read new cache", "change": "added", "unit": "u1"},
			map[string]any{"from": "B", "to": "A", "text": "reply", "kind": "return", "change": "none"},
		},
	}, units)

	before, after := sq.View(true), sq.View(false)
	texts := func(v *Sequence) (out []string) {
		for _, s := range v.Steps {
			if s.Removed {
				t.Errorf("a view should not keep Removed: %+v", s)
			}
			mark := ""
			if s.Changed {
				mark = "*"
			}
			out = append(out, mark+s.Text)
		}
		return out
	}
	if got := strings.Join(texts(before), ","); got != "request,*read old cache,reply" {
		t.Errorf("before = %s", got)
	}
	if got := strings.Join(texts(after), ","); got != "request,*read new cache,reply" {
		t.Errorf("after = %s", got)
	}
	for _, a := range before.Participants {
		if a.ID == "New" {
			t.Error("the column only the PR adds should not be drawn before it")
		}
	}
	if m := sq.Mermaid(); strings.Contains(m, "old cache") {
		t.Errorf("Mermaid should draw the flow after the PR:\n%s", m)
	}
}
