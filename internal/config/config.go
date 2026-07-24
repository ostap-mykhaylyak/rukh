// Package config loads and validates the rukh configuration
// (/etc/rukh/config.yaml) and provides hot-reload via fsnotify.
//
// Every field has a production default (see Default), so the
// operator's config.yaml may be sparse or even empty: rukh is meant to
// work with almost no configuration.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ostap-mykhaylyak/rukh/internal/paths"
)

// Duration wraps time.Duration to accept human-friendly YAML values
// such as "30m", "24h", "5s".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler via time.ParseDuration.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML renders the duration back in its string form.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Std returns the value as a standard time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Config is the whole configuration document.
type Config struct {
	Server   Server   `yaml:"server"`
	Backend  Backend  `yaml:"backend"`
	Nginx    Nginx    `yaml:"nginx"`
	RealIP   RealIP   `yaml:"realip"`
	Learn    Learn    `yaml:"learn"`
	Hints    Hints    `yaml:"hints"`
	Prefetch Prefetch `yaml:"prefetch"`
	Preload  Preload  `yaml:"preload"`
	Log      Log      `yaml:"log"`

	// Warnings collects non-fatal issues found by validate()
	// (e.g. invalid list entries that were skipped). Never fatal.
	Warnings []string `yaml:"-"`
}

// Server holds the public entrypoints rukh takes over from nginx.
type Server struct {
	HTTP              string   `yaml:"http"`  // "" disables the plain listener
	HTTPS             string   `yaml:"https"` // "" disables the TLS listener
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	IdleTimeout       Duration `yaml:"idle_timeout"`
	TLSMinVersion     string   `yaml:"tls_min_version"` // "1.2" or "1.3"
	RedirectHTTPS     bool     `yaml:"redirect_https"`  // 301 http -> https
}

// Backend describes how rukh reaches nginx on the same machine.
type Backend struct {
	// Address is "host:port"; empty means auto-detect from the nginx
	// configuration (the first loopback listener that is not one of
	// the ports rukh itself binds).
	Address             string   `yaml:"address"`
	TLS                 bool     `yaml:"tls"`
	TLSSkipVerify       bool     `yaml:"tls_skip_verify"`
	Timeout             Duration `yaml:"timeout"`
	MaxIdleConnsPerHost int      `yaml:"max_idle_conns_per_host"`
}

// Nginx configures the discovery of virtual hosts and certificates
// from the running nginx configuration: rukh never duplicates the
// certificate setup, it reads nginx's own.
type Nginx struct {
	Config  string   `yaml:"config"`  // entry point, includes followed
	Refresh Duration `yaml:"refresh"` // re-scan interval (mtime based)
}

// RealIP configures client address extraction when something else
// (a CDN, another proxy) sits in front of rukh. With an empty
// trusted_proxies list the peer address is always the client.
type RealIP struct {
	Header         string   `yaml:"header"`
	TrustedProxies []string `yaml:"trusted_proxies"`
}

// Learn tunes the in-memory traffic model. Everything lives in RAM:
// no database, no persistence, bounded by the caps below.
type Learn struct {
	HalfLife         Duration `yaml:"half_life"` // decay of old observations
	MaxHosts         int      `yaml:"max_hosts"`
	MaxPagesPerHost  int      `yaml:"max_pages_per_host"`
	MaxAssetsPerPage int      `yaml:"max_assets_per_page"`
	MaxNextPerPage   int      `yaml:"max_next_per_page"`
	PruneInterval    Duration `yaml:"prune_interval"`
	RebuildInterval  Duration `yaml:"rebuild_interval"`
	MinScore         float64  `yaml:"min_score"` // below this an entry is forgotten
	QueueSize        int      `yaml:"queue_size"`
}

// Hints tunes the automatic 103 Early Hints.
type Hints struct {
	Enabled       bool     `yaml:"enabled"`
	MinConfidence float64  `yaml:"min_confidence"` // P(asset | page view)
	MinSamples    float64  `yaml:"min_samples"`    // decayed hits required
	MaxLinks      int      `yaml:"max_links"`
	MaxAge        Duration `yaml:"max_age"` // ignore assets unseen for longer
	HTTP1         bool     `yaml:"http1"`   // also send 103 on HTTP/1.1
}

// Prefetch tunes the navigation-path prediction advertised to the
// browser as Link rel=prefetch on the final response.
type Prefetch struct {
	Enabled        bool    `yaml:"enabled"`
	MinProbability float64 `yaml:"min_probability"`
	MaxLinks       int     `yaml:"max_links"`
}

// Preload tunes the cache warmer.
type Preload struct {
	Enabled      bool     `yaml:"enabled"`
	Concurrency  int      `yaml:"concurrency"`
	MaxPerMinute int      `yaml:"max_per_minute"`
	Interval     Duration `yaml:"interval"`    // how often the plan is executed
	MinRefresh   Duration `yaml:"min_refresh"` // hottest page revisit period
	MaxRefresh   Duration `yaml:"max_refresh"` // coldest page revisit period
	MaxPages     int      `yaml:"max_pages"`   // plan size cap
	Timeout      Duration `yaml:"timeout"`
}

// Log tunes the log streams.
type Log struct {
	Access bool `yaml:"access"` // one line per proxied request
	Learn  bool `yaml:"learn"`  // hints/preload decisions
}

// Default returns the configuration with ALL production defaults.
func Default() *Config {
	return &Config{
		Server: Server{
			HTTP:              ":80", // ":port" binds IPv4 and IPv6
			HTTPS:             ":443",
			ReadHeaderTimeout: Duration(10 * time.Second),
			IdleTimeout:       Duration(120 * time.Second),
			TLSMinVersion:     "1.2",
			RedirectHTTPS:     false,
		},
		Backend: Backend{
			Address:             "", // auto-detect from nginx
			TLS:                 false,
			TLSSkipVerify:       true, // loopback hop to nginx
			Timeout:             Duration(60 * time.Second),
			MaxIdleConnsPerHost: 256,
		},
		Nginx: Nginx{
			Config:  paths.NginxConf,
			Refresh: Duration(time.Minute),
		},
		RealIP: RealIP{
			Header: "X-Forwarded-For",
		},
		Learn: Learn{
			HalfLife:         Duration(6 * time.Hour),
			MaxHosts:         64,
			MaxPagesPerHost:  5000,
			MaxAssetsPerPage: 60,
			MaxNextPerPage:   32,
			PruneInterval:    Duration(time.Minute),
			RebuildInterval:  Duration(5 * time.Second),
			MinScore:         0.05,
			QueueSize:        8192,
		},
		Hints: Hints{
			Enabled:       true,
			MinConfidence: 0.6,
			MinSamples:    5,
			MaxLinks:      10,
			MaxAge:        Duration(24 * time.Hour),
			HTTP1:         false,
		},
		Prefetch: Prefetch{
			Enabled:        true,
			MinProbability: 0.35,
			MaxLinks:       2,
		},
		Preload: Preload{
			Enabled:      true,
			Concurrency:  2,
			MaxPerMinute: 30,
			Interval:     Duration(30 * time.Second),
			MinRefresh:   Duration(time.Minute),
			MaxRefresh:   Duration(time.Hour),
			MaxPages:     200,
			Timeout:      Duration(20 * time.Second),
		},
		Log: Log{
			Access: true,
			Learn:  true,
		},
	}
}

// Load reads the YAML file at path on top of Default() and validates
// the result.
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate checks the minimal invariants. Invalid list entries are
// never fatal: they are skipped and collected in Warnings.
func (c *Config) validate() error {
	if c.Server.HTTP == "" && c.Server.HTTPS == "" {
		return fmt.Errorf("server: at least one of http/https must be set")
	}
	for name, addr := range map[string]string{"server.http": c.Server.HTTP, "server.https": c.Server.HTTPS} {
		if addr == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	switch c.Server.TLSMinVersion {
	case "1.2", "1.3":
	default:
		return fmt.Errorf("server.tls_min_version must be \"1.2\" or \"1.3\", got %q", c.Server.TLSMinVersion)
	}
	if c.Server.ReadHeaderTimeout.Std() <= 0 {
		return fmt.Errorf("server.read_header_timeout must be positive")
	}

	if c.Backend.Address != "" {
		if _, _, err := net.SplitHostPort(c.Backend.Address); err != nil {
			return fmt.Errorf("backend.address: %w", err)
		}
	}
	if c.Backend.Timeout.Std() <= 0 {
		return fmt.Errorf("backend.timeout must be positive")
	}
	if c.Backend.MaxIdleConnsPerHost < 1 {
		return fmt.Errorf("backend.max_idle_conns_per_host must be >= 1")
	}

	if c.Nginx.Config == "" {
		return fmt.Errorf("nginx.config is required")
	}
	if c.Nginx.Refresh.Std() <= 0 {
		return fmt.Errorf("nginx.refresh must be positive")
	}

	// Invalid trusted_proxies entries are skipped with a warning: a bad
	// line must not prevent the (re)load.
	valid := c.RealIP.TrustedProxies[:0]
	for _, e := range c.RealIP.TrustedProxies {
		if _, _, err := net.ParseCIDR(e); err == nil {
			valid = append(valid, e)
			continue
		}
		if net.ParseIP(e) != nil {
			valid = append(valid, e)
			continue
		}
		c.Warnings = append(c.Warnings, fmt.Sprintf("realip.trusted_proxies: skipping invalid entry %q", e))
	}
	c.RealIP.TrustedProxies = valid
	if len(c.RealIP.TrustedProxies) > 0 && c.RealIP.Header == "" {
		return fmt.Errorf("realip.header is required when realip.trusted_proxies is set")
	}

	if c.Learn.HalfLife.Std() <= 0 {
		return fmt.Errorf("learn.half_life must be positive")
	}
	if c.Learn.MaxHosts < 1 || c.Learn.MaxPagesPerHost < 1 ||
		c.Learn.MaxAssetsPerPage < 1 || c.Learn.MaxNextPerPage < 1 {
		return fmt.Errorf("learn.max_* limits must be >= 1")
	}
	if c.Learn.PruneInterval.Std() <= 0 || c.Learn.RebuildInterval.Std() <= 0 {
		return fmt.Errorf("learn.prune_interval and learn.rebuild_interval must be positive")
	}
	if c.Learn.MinScore < 0 {
		return fmt.Errorf("learn.min_score must be >= 0")
	}
	if c.Learn.QueueSize < 64 {
		return fmt.Errorf("learn.queue_size must be >= 64")
	}

	if c.Hints.MinConfidence < 0 || c.Hints.MinConfidence > 1 {
		return fmt.Errorf("hints.min_confidence must be between 0 and 1")
	}
	if c.Hints.MinSamples < 0 {
		return fmt.Errorf("hints.min_samples must be >= 0")
	}
	if c.Hints.MaxLinks < 0 {
		return fmt.Errorf("hints.max_links must be >= 0")
	}
	if c.Hints.MaxAge.Std() <= 0 {
		return fmt.Errorf("hints.max_age must be positive")
	}

	if c.Prefetch.MinProbability < 0 || c.Prefetch.MinProbability > 1 {
		return fmt.Errorf("prefetch.min_probability must be between 0 and 1")
	}
	if c.Prefetch.MaxLinks < 0 {
		return fmt.Errorf("prefetch.max_links must be >= 0")
	}

	if c.Preload.Concurrency < 1 {
		return fmt.Errorf("preload.concurrency must be >= 1")
	}
	if c.Preload.MaxPerMinute < 0 {
		return fmt.Errorf("preload.max_per_minute must be >= 0")
	}
	if c.Preload.Interval.Std() <= 0 || c.Preload.Timeout.Std() <= 0 {
		return fmt.Errorf("preload.interval and preload.timeout must be positive")
	}
	if c.Preload.MinRefresh.Std() <= 0 || c.Preload.MaxRefresh.Std() < c.Preload.MinRefresh.Std() {
		return fmt.Errorf("preload.min_refresh must be positive and <= preload.max_refresh")
	}
	if c.Preload.MaxPages < 1 {
		return fmt.Errorf("preload.max_pages must be >= 1")
	}

	return nil
}

// watchDir returns the directory to watch for changes: editors replace
// the file via atomic rename, which a file-level watch would lose.
func watchDir(path string) string { return filepath.Dir(path) }
