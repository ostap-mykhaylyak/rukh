package status

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorstCheckWins(t *testing.T) {
	cases := []struct {
		checks []Check
		want   string
		exit   int
	}{
		{nil, OK, ExitOK},
		{[]Check{{Status: OK}, {Status: OK}}, OK, ExitOK},
		{[]Check{{Status: OK}, {Status: Warn}}, Warn, ExitWarn},
		{[]Check{{Status: Warn}, {Status: Crit}, {Status: OK}}, Crit, ExitCrit},
	}
	for _, c := range cases {
		got := worst(c.checks)
		if got != c.want || ExitCode(got) != c.exit {
			t.Errorf("worst(%+v) = %s (exit %d), want %s (exit %d)", c.checks, got, ExitCode(got), c.want, c.exit)
		}
	}
	if ExitCode("nonsense") != ExitUnk {
		t.Error("an unknown status must map to UNKNOWN")
	}
}

func TestNotRunningDistinguishesInstalledFromMissing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.yaml")
	snap := notRunning("test", missing)
	if snap.Status != Crit || snap.Service.Active {
		t.Fatalf("snapshot = %+v", snap)
	}
	if findCheck(snap, "config_on_disk").Status != Warn {
		t.Fatalf("a missing config must read as 'not installed': %+v", snap.Checks)
	}

	good := filepath.Join(dir, "config.yaml")
	os.WriteFile(good, []byte("hints:\n  max_links: 3\n"), 0o644)
	snap = notRunning("test", good)
	if c := findCheck(snap, "config_on_disk"); c.Status != OK {
		t.Fatalf("a valid config must read as 'installed but stopped': %+v", c)
	}
	if ExitCode(snap.Status) != ExitCrit {
		t.Fatal("a stopped service is CRITICAL for a monitor either way")
	}

	broken := filepath.Join(dir, "broken.yaml")
	os.WriteFile(broken, []byte("server:\n  tls_min_version: \"1.1\"\n"), 0o644)
	if c := findCheck(notRunning("test", broken), "config_on_disk"); c.Status != Crit {
		t.Fatalf("an invalid config must be reported: %+v", c)
	}
}

func findCheck(s *Snapshot, name string) Check {
	for _, c := range s.Checks {
		if c.Name == name {
			return c
		}
	}
	return Check{}
}
