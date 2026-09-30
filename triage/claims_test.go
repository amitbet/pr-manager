package triage

import (
	"strings"
	"testing"
)

func TestAbsoluteClaims(t *testing.T) {
	u := &Unit{File: "store/client.go", Hunks: []Hunk{{Lines: []string{
		"+// fetch never returns a nil record without an error.",
		"+	x := load() // every caller holds mu here",
		"+	if never { // not a claim: code, no comment",
		"+	url := \"http://example.com/always\"",
		"+// Loads the record.",
		"-// removed lines always count for nothing",
		" // context lines always count for nothing",
		"+// fetch never returns a nil record without an error.",
	}}}}
	got := absoluteClaims(u)
	want := []string{"// fetch never returns a nil record without an error.", "// every caller holds mu here"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("claims = %q, want %q", got, want)
	}
	doc := &Unit{File: "README.md", Hunks: []Hunk{{Lines: []string{"+A failed call always moves the unit up.", "+Units are sorted."}}}}
	if got := absoluteClaims(doc); len(got) != 1 || got[0] != "A failed call always moves the unit up." {
		t.Errorf("doc claims = %q", got)
	}
}

func TestReviewContextAsksToBreakClaims(t *testing.T) {
	u := &Unit{ID: "a.go:f", File: "a.go", Hunks: []Hunk{{Lines: []string{"+// f cannot fail on empty input", "+func f() {}"}}}}
	setReviewContext([]*Unit{u}, nil, DefaultReviewContextChars)
	if !strings.Contains(u.ReviewContext, "make absolute claims: `// f cannot fail on empty input`. Try to break each one") {
		t.Errorf("context:\n%s", u.ReviewContext)
	}
}
