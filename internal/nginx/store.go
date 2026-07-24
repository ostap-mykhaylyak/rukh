package nginx

import (
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// Store keeps the last successfully parsed nginx configuration behind
// an atomic pointer and refreshes it when any of the files it was
// built from changes on disk (mtime/size), so a new virtual host or a
// renewed certificate is picked up without touching rukh.
//
// A failed re-parse never replaces the running configuration: the last
// good one keeps serving and the error is reported by `rukh status`.
type Store struct {
	path string
	log  *slog.Logger

	cur      atomic.Pointer[Config]
	stamps   atomic.Pointer[map[string]string]
	lastErr  atomic.Pointer[string]
	lastLoad atomic.Int64 // unix nanoseconds
}

// NewStore returns a Store for the nginx configuration at path.
func NewStore(path string, log *slog.Logger) *Store {
	return &Store{path: path, log: log}
}

// Load parses the configuration now. On error the previous
// configuration (if any) is left in place.
func (s *Store) Load() error {
	cfg, err := Parse(s.path)
	if err != nil {
		e := err.Error()
		s.lastErr.Store(&e)
		return err
	}
	stamps := fingerprint(cfg.Files)
	s.cur.Store(cfg)
	s.stamps.Store(&stamps)
	s.lastErr.Store(nil)
	s.lastLoad.Store(time.Now().UnixNano())
	return nil
}

// Get returns the current configuration, or an empty one before the
// first successful load (never nil: the hot path must not check).
func (s *Store) Get() *Config {
	if c := s.cur.Load(); c != nil {
		return c
	}
	return &Config{Path: s.path}
}

// LastError returns the error of the most recent failed parse, or "".
func (s *Store) LastError() string {
	if e := s.lastErr.Load(); e != nil {
		return *e
	}
	return ""
}

// LastLoad returns when the configuration was last parsed successfully.
func (s *Store) LastLoad() time.Time {
	n := s.lastLoad.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// Changed reports whether any file of the current configuration
// changed since the last load.
func (s *Store) Changed() bool {
	old := s.stamps.Load()
	if old == nil {
		return true
	}
	cur := fingerprint(keys(*old))
	if len(cur) != len(*old) {
		return true
	}
	for f, st := range cur {
		if (*old)[f] != st {
			return true
		}
	}
	return false
}

// Watch re-parses the configuration whenever its files change, every
// refresh interval, until stop is closed. onChange (may be nil) runs
// synchronously in the watch goroutine after a successful reload.
func (s *Store) Watch(stop <-chan struct{}, refresh time.Duration, onChange func(*Config)) {
	go func() {
		t := time.NewTicker(refresh)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if !s.Changed() {
					continue
				}
				if err := s.Load(); err != nil {
					s.log.Error("nginx config reload failed, keeping last good", "error", err)
					continue
				}
				cfg := s.Get()
				s.log.Info("nginx config reloaded",
					"sites", len(cfg.Sites), "hosts", len(cfg.Hosts()), "warnings", len(cfg.Warnings))
				if onChange != nil {
					onChange(cfg)
				}
			}
		}
	}()
}

func fingerprint(files []string) map[string]string {
	out := make(map[string]string, len(files))
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil {
			out[f] = "missing"
			continue
		}
		out[f] = fmt.Sprintf("%d/%d", fi.ModTime().UnixNano(), fi.Size())
	}
	return out
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
