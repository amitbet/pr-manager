package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amitbet/pr-manager/triage"
)

func relatedTriager(t *testing.T) *triager {
	tr, err := newTriager(options{cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tr.fetcher = nil // no gh: links aren't looked up
	return tr
}

func relatedChanges(prs []relatedPR) []string {
	var out []string
	for _, p := range prs {
		out = append(out, p.Change+" "+p.By)
	}
	return out
}

// A link goes both ways, takes a new note when made again, and unlinking
// either side's link takes both off.
func TestLinkRelated(t *testing.T) {
	tr := relatedTriager(t)
	ctx := context.Background()
	a := "github.com/acme/web#7"
	if _, err := tr.linkPR(ctx, a, "", "https://github.com/acme/api/pull/41", "the server half", "reader"); err != nil {
		t.Fatal(err)
	}
	if got := relatedChanges(tr.relatedOf(a)); fmt.Sprint(got) != "[github.com/acme/api#41 reader]" {
		t.Fatalf("a: %v", got)
	}
	back := tr.relatedOf("github.com/acme/api#41")
	if len(back) != 1 || back[0].Change != a || back[0].By != "backlink" || back[0].Note != "the server half" || back[0].URL != "https://github.com/acme/web/pull/7" {
		t.Fatalf("back: %+v", back)
	}
	if _, err := tr.linkPR(ctx, a, "", "acme/api#41", "renames the user field", "agent"); err != nil {
		t.Fatal(err)
	}
	if prs := tr.relatedOf(a); len(prs) != 1 || prs[0].Note != "renames the user field" {
		t.Fatalf("relinked: %+v", prs)
	}
	// From the other side, by its short form.
	if err := tr.unlinkPR("github.com/acme/api#41", "", "acme/web#7"); err != nil {
		t.Fatal(err)
	}
	if len(tr.relatedOf(a)) != 0 || len(tr.relatedOf("github.com/acme/api#41")) != 0 {
		t.Fatalf("left: %v %v", tr.relatedOf(a), tr.relatedOf("github.com/acme/api#41"))
	}
	if err := tr.unlinkPR(a, "", "acme/api#41"); err == nil {
		t.Fatal("unlinked a PR that wasn't linked")
	}
}

// A change can't be related to itself, a local branch without a PR gets
// no link back, and a change has at most relatedMax.
func TestLinkRelatedLimits(t *testing.T) {
	tr := relatedTriager(t)
	ctx := context.Background()
	if _, err := tr.linkPR(ctx, "github.com/acme/web#7", "", "acme/web#7", "", ""); err == nil {
		t.Fatal("linked a PR to itself")
	}
	local := "local:/src/web"
	for i := range relatedMax {
		if _, err := tr.linkPR(ctx, local, "", fmt.Sprintf("acme/api#%d", i+1), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tr.linkPR(ctx, local, "", "acme/api#99", "", ""); err == nil {
		t.Fatal("linked more than the most")
	}
	if back := tr.relatedOf("github.com/acme/api#1"); len(back) != 0 {
		t.Fatalf("a local branch without a PR linked back: %+v", back)
	}
}

// The prompt has each related PR's facts, the app's review of it and a
// pointer to its diff; without a folder, the diff itself.
func TestWriteRelated(t *testing.T) {
	ref := triage.PRRef{Owner: "acme", Repo: "api", Number: 41}
	rm := &relatedMaterial{
		relatedPR: relatedPR{Change: prChange(ref), URL: ref.URL(), Note: "the server half", By: "agent", At: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)},
		ref:       ref,
		info:      &triage.PRInfo{PRRef: ref, Title: "Rename user.name", Author: "dana", State: "OPEN", BaseRef: "main", HeadRef: "rename", BaseOid: "b0b0b0b0b0", HeadOid: "h1h1h1h1h1", Body: "Renames the field.", Adds: 3, Dels: 1},
		files:     []relatedFile{{Path: "api/user.go", Status: "modified", Adds: 3, Dels: 1}},
		diff:      "diff --git a/api/user.go b/api/user.go\n+Name string `json:\"full_name\"`\n",
		result: &PRResult{Key: "acme__api__41__h1", PR: &triage.PRInfo{HeadOid: "h0h0h0h0h0"}, Files: []resultFile{{Units: []resultUnit{{Unit: &triage.Unit{File: "api/user.go", Issues: []triage.Issue{
			{Severity: "low", Title: "typo"}, {Severity: "high", Line: 12, Title: "breaks old clients"}, {Severity: "medium", Title: "gone", Dismissed: true},
		}}}}}}},
		code: "/c/related/acme__api__41",
	}
	var b strings.Builder
	writeRelated(&b, []*relatedMaterial{rm}, "/m", promptLimits)
	s := b.String()
	for _, want := range []string{
		"## github.com/acme/api#41: Rename user.name", "Linked by you, at the reader's request", "Why: the server half",
		"main@b0b0b0b0 ← rename@h1h1h1h1", "Its code at its head: /c/related/acme__api__41", "Its full diff: " + filepath.Join("/m", "related", "acme__api__41.diff"),
		"at an older head, h0h0h0h0", "Its open review issues (2):\n- high api/user.go:12: breaks old clients\n- low",
		"<related_pr_description pr=\"github.com/acme/api#41\">\nRenames the field.", "- api/user.go (modified, +3/−1)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("no %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "gone") || strings.Contains(s, "full_name") {
		t.Errorf("a dismissed issue, or the diff with a folder to read it in:\n%s", s)
	}
	b.Reset()
	writeRelated(&b, []*relatedMaterial{rm}, "", promptLimits)
	if !strings.Contains(b.String(), "<related_pr_diff pr=\"github.com/acme/api#41\">\ndiff --git") {
		t.Errorf("no diff without a folder:\n%s", b.String())
	}
	b.Reset()
	writeRelated(&b, []*relatedMaterial{{relatedPR: rm.relatedPR, err: errors.New("gh: not found")}}, "/m", promptLimits)
	if !strings.Contains(b.String(), "It could not be read just now: gh: not found") {
		t.Errorf("no error:\n%s", b.String())
	}
}

func TestRenamedPath(t *testing.T) {
	for in, want := range map[string]string{"a.go => b.go": "b.go", "pkg/{old => new}/f.go": "pkg/new/f.go", "pkg/{ => sub}/f.go": "pkg/sub/f.go"} {
		if got := renamedPath(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// A local branch with a PR reads the links kept under either id, each
// once; another local checkout only its own.
func TestRelatedForLocal(t *testing.T) {
	tr := relatedTriager(t)
	ctx := context.Background()
	if _, err := tr.linkPR(ctx, "local:/src/web", "", "acme/api#41", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.linkPR(ctx, "github.com/acme/docs#3", "", "acme/web#7", "", ""); err != nil { // links back to web#7
		t.Fatal(err)
	}
	if _, err := tr.linkPR(ctx, "github.com/acme/web#7", "", "acme/api#41", "", ""); err != nil {
		t.Fatal(err)
	}
	p := &triage.PRInfo{LocalPath: "/src/web", URL: "https://github.com/acme/web/pull/7"}
	if got := relatedChanges(tr.relatedFor(changeIDs(p)...)); fmt.Sprint(got) != "[github.com/acme/api#41 reader github.com/acme/docs#3 backlink]" {
		t.Fatalf("got %v", got)
	}
	if got := tr.relatedFor(changeIDs(&triage.PRInfo{LocalPath: "/src/other"})...); len(got) != 0 {
		t.Fatalf("other checkout: %v", got)
	}
}
