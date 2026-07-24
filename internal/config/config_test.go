package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultIsValidAndWarningFree(t *testing.T) {
	c := Default()
	if err := c.validate(); err != nil {
		t.Fatalf("the default configuration must be valid: %v", err)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("the default configuration must not warn: %v", c.Warnings)
	}
}

func TestEmptyFileYieldsTheDefaults(t *testing.T) {
	c, err := Load(write(t, ""))
	if err != nil {
		t.Fatalf("an empty config must load: %v", err)
	}
	if c.Server.HTTPS != ":443" || !c.Hints.Enabled || !c.Preload.Enabled {
		t.Fatalf("defaults not applied: %+v", c.Server)
	}
	if c.Backend.Address != "" {
		t.Fatalf("backend.address must default to auto-detection, got %q", c.Backend.Address)
	}
}

func TestSparseFileOverridesOnlyWhatItSets(t *testing.T) {
	c, err := Load(write(t, "hints:\n  max_links: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Hints.MaxLinks != 3 {
		t.Fatalf("max_links = %d", c.Hints.MaxLinks)
	}
	if c.Hints.MinConfidence != 0.6 {
		t.Fatalf("untouched fields must keep their default: %+v", c.Hints)
	}
}

func TestDurationsAcceptHumanValues(t *testing.T) {
	c, err := Load(write(t, "learn:\n  half_life: \"90m\"\npreload:\n  timeout: \"5s\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Learn.HalfLife.Std() != 90*time.Minute || c.Preload.Timeout.Std() != 5*time.Second {
		t.Fatalf("durations = %v / %v", c.Learn.HalfLife.Std(), c.Preload.Timeout.Std())
	}
	if _, err := Load(write(t, "preload:\n  timeout: \"tomorrow\"\n")); err == nil {
		t.Fatal("an invalid duration must fail the load")
	}
}

func TestInvalidValuesAreRejected(t *testing.T) {
	cases := map[string]string{
		"no listener":        "server:\n  http: \"\"\n  https: \"\"\n",
		"bad listen":         "server:\n  https: \"nope\"\n",
		"bad tls version":    "server:\n  tls_min_version: \"1.1\"\n",
		"bad backend":        "backend:\n  address: \"nope\"\n",
		"empty nginx config": "nginx:\n  config: \"\"\n",
		"confidence range":   "hints:\n  min_confidence: 2\n",
		"refresh order":      "preload:\n  min_refresh: \"2h\"\n  max_refresh: \"1m\"\n",
		"tiny queue":         "learn:\n  queue_size: 8\n",
	}
	for name, body := range cases {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestInvalidTrustedProxyIsSkippedNotFatal(t *testing.T) {
	c, err := Load(write(t, "realip:\n  trusted_proxies: [\"10.0.0.0/8\", \"garbage\", \"192.0.2.7\"]\n"))
	if err != nil {
		t.Fatalf("a bad list entry must never prevent the load: %v", err)
	}
	if len(c.RealIP.TrustedProxies) != 2 {
		t.Fatalf("trusted_proxies = %v", c.RealIP.TrustedProxies)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "garbage") {
		t.Fatalf("warnings = %v", c.Warnings)
	}
}

func TestManagerReloadKeepsLastGoodOnError(t *testing.T) {
	p := write(t, "hints:\n  max_links: 4\n")
	m, err := NewManager(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.Get().Hints.MaxLinks != 4 {
		t.Fatal("initial load")
	}
	os.WriteFile(p, []byte("hints:\n  max_links: [oops\n"), 0o644)
	if err := m.Reload(); err == nil {
		t.Fatal("expected a reload error")
	}
	if m.Get().Hints.MaxLinks != 4 {
		t.Fatal("a broken file must not replace the running configuration")
	}
	if m.LastError() == "" {
		t.Fatal("the pending error must be visible to --status")
	}
	os.WriteFile(p, []byte("hints:\n  max_links: 7\n"), 0o644)
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if m.Get().Hints.MaxLinks != 7 || m.LastError() != "" {
		t.Fatal("a good reload must clear the error and swap the config")
	}
}
