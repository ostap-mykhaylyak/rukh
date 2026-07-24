// Package status implements the daemon's status snapshot, the local
// Unix socket that serves it, and the CLI client behind `rukh status`.
//
// The daemon is the single source of truth about its own state: the
// client never reconstructs state from disk (beyond a minimal "is the
// config on disk valid" hint when the daemon is down).
package status

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/ostap-mykhaylyak/rukh/internal/certs"
	"github.com/ostap-mykhaylyak/rukh/internal/config"
	"github.com/ostap-mykhaylyak/rukh/internal/learn"
	"github.com/ostap-mykhaylyak/rukh/internal/metrics"
	"github.com/ostap-mykhaylyak/rukh/internal/nginx"
	"github.com/ostap-mykhaylyak/rukh/internal/preload"
)

// Check statuses, ordered by severity. Exit codes follow the Nagios
// convention: 0 OK, 1 WARNING, 2 CRITICAL, 3 UNKNOWN.
const (
	OK       = "ok"
	Warn     = "warn"
	Crit     = "crit"
	Unknown  = "unknown"
	ExitOK   = 0
	ExitWarn = 1
	ExitCrit = 2
	ExitUnk  = 3
)

// Check is a single named health check; monitors can alert on
// individual checks as well as on the aggregate status.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | crit
	Detail string `json:"detail"`
}

// ServiceInfo describes the running daemon.
type ServiceInfo struct {
	Active        bool    `json:"active"`
	PID           int     `json:"pid,omitempty"`
	UptimeSeconds float64 `json:"uptime_seconds,omitempty"`
}

// ConfigInfo describes the loaded (or on-disk) configuration.
type ConfigInfo struct {
	Path     string   `json:"path"`
	Valid    bool     `json:"valid"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// NginxSection describes what was discovered in the nginx setup.
type NginxSection struct {
	Config     string       `json:"config"`
	Sites      int          `json:"sites"`
	Hosts      []string     `json:"hosts,omitempty"`
	Backend    string       `json:"backend"`
	BackendAut bool         `json:"backend_auto"`
	LastLoad   time.Time    `json:"last_load,omitempty"`
	Error      string       `json:"error,omitempty"`
	Warnings   []string     `json:"warnings,omitempty"`
	Certs      []certs.Info `json:"certs,omitempty"`
}

// Snapshot is the full status document served over the socket.
// Field names are stable across versions.
type Snapshot struct {
	Status    string            `json:"status"` // ok | warn | crit | unknown
	Version   string            `json:"version"`
	Service   ServiceInfo       `json:"service"`
	Config    ConfigInfo        `json:"config"`
	Nginx     *NginxSection     `json:"nginx,omitempty"`
	Learn     *learn.Stats      `json:"learn,omitempty"`
	Preload   *preload.Stats    `json:"preload,omitempty"`
	Checks    []Check           `json:"checks"`
	Live      *metrics.Snapshot `json:"live,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
}

// ExitCode maps the aggregate status onto the Nagios exit codes.
func ExitCode(status string) int {
	switch status {
	case OK:
		return ExitOK
	case Warn:
		return ExitWarn
	case Crit:
		return ExitCrit
	default:
		return ExitUnk
	}
}

// worst aggregates check statuses; the worst one wins.
func worst(checks []Check) string {
	agg := OK
	for _, c := range checks {
		switch c.Status {
		case Crit:
			return Crit
		case Warn:
			agg = Warn
		}
	}
	return agg
}

// Sources are everything the collector reads. All of it is state the
// daemon already holds: building a snapshot never touches the network
// except for one loopback dial to check the backend is alive.
type Sources struct {
	Version    string
	Config     *config.Manager
	Nginx      *nginx.Store
	Certs      *certs.Store
	Learn      *learn.Engine
	Preload    *preload.Warmer
	Metrics    *metrics.Metrics
	LogDir     string
	BackendURL func() (addr string, auto bool)
	TopPages   int
}

// NewCollector builds the snapshot function the daemon serves on the
// socket.
func NewCollector(s Sources) func() *Snapshot {
	start := time.Now()
	return func() *Snapshot {
		cfg := s.Config.Get()
		snap := &Snapshot{
			Version: s.Version,
			Service: ServiceInfo{
				Active:        true,
				PID:           os.Getpid(),
				UptimeSeconds: time.Since(start).Seconds(),
			},
			Config: ConfigInfo{
				Path:     s.Config.Path(),
				Valid:    true,
				Warnings: cfg.Warnings,
			},
			Timestamp: time.Now().UTC(),
		}

		var checks []Check
		if e := s.Config.LastError(); e != "" {
			checks = append(checks, Check{"config", Crit, "pending reload error: " + e})
			snap.Config.Error = e
		} else {
			checks = append(checks, Check{"config", OK, "loaded and valid"})
		}
		if len(cfg.Warnings) > 0 {
			checks = append(checks, Check{"config_warnings", Warn, cfg.Warnings[0]})
		}
		if err := checkWritable(s.LogDir); err != nil {
			checks = append(checks, Check{"log_dir", Crit, "not writable: " + err.Error()})
		} else {
			checks = append(checks, Check{"log_dir", OK, "writable"})
		}

		// nginx discovery
		ncfg := s.Nginx.Get()
		addr, auto := s.BackendURL()
		ng := &NginxSection{
			Config:     ncfg.Path,
			Sites:      len(ncfg.Sites),
			Hosts:      ncfg.Hosts(),
			Backend:    addr,
			BackendAut: auto,
			LastLoad:   s.Nginx.LastLoad(),
			Error:      s.Nginx.LastError(),
			Warnings:   ncfg.Warnings,
			Certs:      s.Certs.Loaded(),
		}
		snap.Nginx = ng
		switch {
		case ng.Error != "" && ng.Sites == 0:
			checks = append(checks, Check{"nginx", Crit, "cannot read the nginx configuration: " + ng.Error})
		case ng.Error != "":
			checks = append(checks, Check{"nginx", Warn, "reload failed, serving last good: " + ng.Error})
		case ng.Sites == 0:
			checks = append(checks, Check{"nginx", Warn, "no server block discovered"})
		default:
			checks = append(checks, Check{"nginx", OK,
				fmt.Sprintf("%d server block(s), %d host(s)", ng.Sites, len(ng.Hosts))})
		}
		if len(ng.Warnings) > 0 {
			checks = append(checks, Check{"nginx_warnings", Warn, ng.Warnings[0]})
		}
		if soon := expiringCerts(ng.Certs); soon != "" {
			checks = append(checks, Check{"certificates", Warn, soon})
		}

		// backend reachability: one local dial, no request
		if err := dialable(addr); err != nil {
			checks = append(checks, Check{"backend", Crit, addr + " unreachable: " + err.Error()})
		} else {
			checks = append(checks, Check{"backend", OK, addr + " reachable"})
		}

		st := s.Learn.Stats(s.TopPages)
		snap.Learn = &st
		if st.Dropped > 0 {
			checks = append(checks, Check{"learn", Warn,
				fmt.Sprintf("%d observation(s) dropped: queue saturated", st.Dropped)})
		} else {
			checks = append(checks, Check{"learn", OK,
				fmt.Sprintf("%d page(s), %d asset(s), %d transition(s)", st.Pages, st.Assets, st.Transitions)})
		}

		ps := s.Preload.Stats()
		snap.Preload = &ps
		if ps.Enabled && ps.Backoff {
			checks = append(checks, Check{"preload", Warn, "backing off after failed warm-up requests"})
		}

		live := s.Metrics.Snapshot()
		snap.Live = &live
		snap.Checks = checks
		snap.Status = worst(checks)
		return snap
	}
}

func expiringCerts(list []certs.Info) string {
	for _, c := range list {
		if c.Error != "" {
			return c.CertFile + ": " + c.Error
		}
		if !c.NotAfter.IsZero() && time.Until(c.NotAfter) < 7*24*time.Hour {
			return fmt.Sprintf("%s expires %s", c.CertFile, c.NotAfter.Format(time.RFC3339))
		}
	}
	return ""
}

func dialable(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return err
	}
	return c.Close()
}

func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".rukh-writecheck-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// notRunning builds the fallback snapshot when the socket is
// unreachable: the service is considered down; the only extra hint is
// whether a config exists on disk and parses, to distinguish
// "installed but stopped" from "not installed".
func notRunning(version, cfgPath string) *Snapshot {
	snap := &Snapshot{
		Status:    Crit,
		Version:   version,
		Service:   ServiceInfo{Active: false},
		Config:    ConfigInfo{Path: cfgPath},
		Timestamp: time.Now().UTC(),
	}
	snap.Checks = append(snap.Checks, Check{"service", Crit, "not running (status socket unreachable)"})

	if _, err := os.Stat(cfgPath); err != nil {
		snap.Checks = append(snap.Checks, Check{"config_on_disk", Warn, "absent (not installed?)"})
		return snap
	}
	if _, err := config.Load(cfgPath); err != nil {
		snap.Config.Error = err.Error()
		snap.Checks = append(snap.Checks, Check{"config_on_disk", Crit, err.Error()})
		return snap
	}
	snap.Config.Valid = true
	snap.Checks = append(snap.Checks, Check{"config_on_disk", OK, "valid (installed but stopped)"})
	return snap
}

// socketDir returns the directory that must exist before listening.
func socketDir(sock string) string { return filepath.Dir(sock) }
