// Package certs loads and caches the TLS certificates rukh serves.
//
// The file paths come from the nginx configuration, so certificates
// keep being issued and renewed by whatever already does it (certbot,
// acme.sh, a control panel): rukh never owns them, it only reads the
// same files nginx reads.
//
// Renewal hot-swap: every cached entry is re-stat'ed at most once per
// TTL (30s); a changed mtime triggers a reload. A renewed certificate
// is therefore picked up within TTL, with zero downtime and no
// fsnotify complexity. If a reload fails, the cached certificate keeps
// being served rather than breaking live handshakes.
package certs

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"time"
)

// Store is the certificate cache.
type Store struct {
	ttl time.Duration

	mu    sync.Mutex
	cache map[string]*entry // keyed by cert file path
}

type entry struct {
	cert    *tls.Certificate
	checked time.Time
	mtime   time.Time
	err     string
}

// New returns an empty Store.
func New() *Store {
	return &Store{ttl: 30 * time.Second, cache: map[string]*entry{}}
}

// GetPair returns the certificate for a cert/key file pair.
func (s *Store) GetPair(certFile, keyFile string) (*tls.Certificate, error) {
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("no certificate configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	e := s.cache[certFile]
	if e != nil && now.Sub(e.checked) < s.ttl {
		if e.cert != nil {
			return e.cert, nil
		}
		return nil, fmt.Errorf("%s", e.err)
	}

	fi, err := os.Stat(certFile)
	if err != nil {
		if e != nil && e.cert != nil {
			e.checked = now
			return e.cert, nil
		}
		s.cache[certFile] = &entry{checked: now, err: err.Error()}
		return nil, err
	}
	if e != nil && e.cert != nil && fi.ModTime().Equal(e.mtime) {
		e.checked = now
		return e.cert, nil
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		if e != nil && e.cert != nil {
			e.checked = now
			return e.cert, nil
		}
		s.cache[certFile] = &entry{checked: now, err: err.Error()}
		return nil, fmt.Errorf("load certificate: %w", err)
	}
	s.cache[certFile] = &entry{cert: &cert, checked: now, mtime: fi.ModTime()}
	return &cert, nil
}

// Info describes one cached certificate, for `rukh status`.
type Info struct {
	CertFile string    `json:"cert_file"`
	Subject  string    `json:"subject,omitempty"`
	NotAfter time.Time `json:"not_after,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// Loaded returns what the cache currently holds.
func (s *Store) Loaded() []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Info, 0, len(s.cache))
	for path, e := range s.cache {
		i := Info{CertFile: path, Error: e.err}
		if e.cert != nil && e.cert.Leaf != nil {
			i.Subject = e.cert.Leaf.Subject.CommonName
			i.NotAfter = e.cert.Leaf.NotAfter
		}
		out = append(out, i)
	}
	return out
}
