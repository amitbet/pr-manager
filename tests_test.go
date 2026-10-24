package main

import (
	"strings"
	"testing"

	"github.com/amitbet/pr-manager/triage"
)

func TestTesterForTrust(t *testing.T) {
	fork := &triage.PRInfo{Author: "stranger", Fork: true}
	own := &triage.PRInfo{Author: "member"}
	local := &triage.PRInfo{LocalPath: "/src/app", Fork: true}
	for _, c := range []struct {
		mode, trusted string
		info          *triage.PRInfo
		runs          bool
		why           string
	}{
		{"off", "", local, false, "turned off"},
		{"on", "", local, true, ""},
		{"on", "", own, true, ""},
		{"on", "", fork, false, "@stranger isn't a trusted author"},
		{"on", "Other, @Stranger", fork, true, ""},
		{"all", "", fork, true, ""},
		{"on", "", nil, true, ""},
	} {
		tr, why := testerFor(options{tests: c.mode, trustedAuthors: c.trusted, testCmd: " make test "}, c.info)
		if (tr != nil) != c.runs || !strings.Contains(why, c.why) {
			t.Errorf("%s %q %+v: tester %v, %q", c.mode, c.trusted, c.info, tr != nil, why)
		}
		if tr != nil && tr.Policy.Command != "make test" {
			t.Errorf("command %q", tr.Policy.Command)
		}
	}
	if r := testsResult(nil, "why"); r == nil || r.Ran || r.Skipped != "why" {
		t.Fatalf("skipped result: %+v", r)
	}
	if testsResult(nil, "") != nil {
		t.Fatal("no result expected")
	}
}
