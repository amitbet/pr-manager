package triage

import "testing"

func TestParsePolicyLint(t *testing.T) {
	p, err := ParsePolicy([]byte("lint:\n  enabled: false\n"))
	if err != nil || p.Lint.Enabled {
		t.Fatalf("an explicit false should turn linting off: %+v %v", p.Lint, err)
	}
	p, err = ParsePolicy([]byte("lint:\n  tools: [ruff]\n  skip: [eslint]\n  secrets: false\n  max_per_unit: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Lint.Enabled || len(p.Lint.Tools) != 1 || p.Lint.Tools[0] != "ruff" || p.Lint.Secrets || p.Lint.MaxPerUnit != 0 {
		t.Fatalf("lint policy not merged over the defaults: %+v", p.Lint)
	}
	if p.Lint.TimeoutSec != DefaultLintTimeoutSec {
		t.Errorf("a field the file leaves out should keep its default, got %d", p.Lint.TimeoutSec)
	}
	if _, err := ParsePolicy([]byte("lint:\n  tools: [nope]\n")); err == nil {
		t.Error("an unknown linter in the policy should be an error, not a silent no-op")
	}
	// The example config in the repo has to parse.
	if _, err := LoadPolicy("../triage.example.yaml"); err != nil {
		t.Errorf("triage.example.yaml: %v", err)
	}
}
