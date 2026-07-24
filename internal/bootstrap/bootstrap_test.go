package bootstrap

import (
	"os"
	"strings"
	"testing"

	"github.com/ostap-mykhaylyak/rukh/internal/config"
	"github.com/ostap-mykhaylyak/rukh/internal/paths"
)

func TestPurgeGuardsAgainstUnexpectedPaths(t *testing.T) {
	for _, bad := range []string{"", "/", "/etc", "/var/log", "/etc/rukhX", "/usr"} {
		if purgeAllowed(bad) {
			t.Errorf("purge must refuse %q", bad)
		}
	}
	for _, good := range PurgeTargets() {
		if !purgeAllowed(good) {
			t.Errorf("purge target %q rejected by its own guard", good)
		}
	}
}

func TestPurgeTargetsFollowThePathsPackage(t *testing.T) {
	want := map[string]bool{paths.ConfigDir: true, paths.LogDir: true, paths.RunDir: true}
	got := PurgeTargets()
	if len(got) != len(want) {
		t.Fatalf("targets = %v", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected purge target %q", g)
		}
	}
}

// The shipped config.yaml is the operator's starting point: it must
// parse and it must not produce warnings.
func TestSkelConfigIsValid(t *testing.T) {
	data, err := skelFS.ReadFile(skelConfig)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := dir + "/config.yaml"
	if err := writeFile(p, data); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("the shipped config must load: %v", err)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("the shipped config must not warn: %v", cfg.Warnings)
	}
	// Every documented default must match the code's default.
	def := config.Default()
	if cfg.Learn.HalfLife != def.Learn.HalfLife || cfg.Hints.MaxLinks != def.Hints.MaxLinks ||
		cfg.Preload.MaxPerMinute != def.Preload.MaxPerMinute {
		t.Fatal("the shipped config documents values that differ from the built-in defaults")
	}
}

func TestUnitFileRunsTheServiceVerb(t *testing.T) {
	unit := string(UnitFile)
	if !strings.Contains(unit, "ExecStart="+paths.Binary+" start ") {
		t.Fatal("the unit must start the daemon with the bare 'start' verb")
	}
	if !strings.Contains(unit, "ReadWritePaths="+paths.LogDir) {
		t.Fatal("the unit must keep the log directory writable under ProtectSystem=strict")
	}
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}
