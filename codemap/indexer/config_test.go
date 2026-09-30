package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "codemap.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUserConfigFilledFromDefault(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "impact:\n  levels: { critical: 90 }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if l := cfg.Impact.Levels; l["critical"] != 90 || l["high"] != 55 || l["medium"] != 35 {
		t.Errorf("levels = %v", l)
	}
	if cfg.Output != "codemap" || cfg.Symbols.MinExternalCallers != 1 || len(cfg.rules) == 0 || cfg.Rollback.Default != 10 {
		t.Errorf("missing sections not filled: output=%q symbols=%+v rules=%d", cfg.Output, cfg.Symbols, len(cfg.rules))
	}

	cfg, err = loadConfig(writeConfig(t, "rollback:\n  default: 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Rollback.Default != 5 || len(cfg.rules) != 0 {
		t.Errorf("a rollback section the user wrote should stand: default=%d rules=%d", cfg.Rollback.Default, len(cfg.rules))
	}
}

func TestConfigDampingValidated(t *testing.T) {
	for _, d := range []string{"1", "1.5", "-0.2"} {
		_, err := loadConfig(writeConfig(t, "rank:\n  damping: "+d+"\n"))
		if err == nil || !strings.Contains(err.Error(), "rank.damping") {
			t.Errorf("damping %s: err = %v", d, err)
		}
	}
}

func TestTeleportClampedSumsToOne(t *testing.T) {
	real := []bool{true, true, false, false}
	tp := teleport(4, real, map[int]float64{2: 0.6, 3: 0.9})
	sum := 0.0
	for _, v := range tp {
		sum += v
	}
	if sum < 1-1e-9 || sum > 1+1e-9 {
		t.Errorf("teleport = %v, sum %v", tp, sum)
	}
	if r := tp[3] / tp[2]; r < 1.5-1e-9 || r > 1.5+1e-9 {
		t.Errorf("virtual ratio = %v, want 1.5", r)
	}
}
