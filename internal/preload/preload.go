// Package preload is the cache warmer: it re-requests, straight from
// nginx, the pages the traffic model considers important, so the
// answer is already in nginx's (or the application's) cache when the
// next visitor asks for it.
//
// What makes it cheap: a page that a real visitor just requested is
// already warm and is skipped; the refresh cadence of each page comes
// from its own rank, so the homepage is revisited often and a page
// nobody reads is barely touched; personalized pages are never warmed;
// and the whole thing is capped by a per-minute budget, so the warm-up
// can never compete with real traffic.
package preload

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ostap-mykhaylyak/rukh/internal/config"
	"github.com/ostap-mykhaylyak/rukh/internal/learn"
	"github.com/ostap-mykhaylyak/rukh/internal/metrics"
	"github.com/ostap-mykhaylyak/rukh/internal/proxy"
)

// maxBody is how much of a warmed page is read before the connection
// is recycled. The body has to be consumed for the backend to consider
// the request complete, but rukh never keeps it.
const maxBody = 4 << 20

// maxRedirects bounds the hops followed inside the backend (a
// trailing-slash redirect is worth following, a redirect chain is not).
const maxRedirects = 2

// Warmer runs the preload loop.
type Warmer struct {
	engine    *learn.Engine
	cfg       *config.Manager
	backend   func() proxy.Backend
	transport func() *http.Transport
	m         *metrics.Metrics
	log       *slog.Logger
	version   string

	stats atomic.Pointer[Stats]
	skip  int // rounds to skip after repeated backend failures
	fails int
}

// Stats is what `rukh status` reports about the preloader.
type Stats struct {
	Enabled   bool      `json:"enabled"`
	PlanSize  int       `json:"plan_size"`
	LastRun   time.Time `json:"last_run,omitempty"`
	LastCount int       `json:"last_count"`
	Requests  int64     `json:"requests"`
	Errors    int64     `json:"errors"`
	Backoff   bool      `json:"backoff,omitempty"`
	Next      []string  `json:"next,omitempty"`
}

// New returns a Warmer. backend and transport are read on every round
// so a configuration reload is picked up without restarting the loop.
func New(engine *learn.Engine, cfg *config.Manager, backend func() proxy.Backend,
	transport func() *http.Transport, m *metrics.Metrics, log *slog.Logger, version string) *Warmer {
	w := &Warmer{engine: engine, cfg: cfg, backend: backend, transport: transport,
		m: m, log: log, version: version}
	w.stats.Store(&Stats{})
	return w
}

// Start runs the loop until stop is closed.
func (w *Warmer) Start(stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(w.cfg.Get().Preload.Interval.Std())
		defer t.Stop()
		interval := w.cfg.Get().Preload.Interval.Std()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if cur := w.cfg.Get().Preload.Interval.Std(); cur != interval {
					interval = cur
					t.Reset(interval)
				}
				w.round(stop)
			}
		}
	}()
}

// round executes one pass of the plan.
func (w *Warmer) round(stop <-chan struct{}) {
	c := w.cfg.Get()
	plan := w.engine.Plan()
	st := &Stats{Enabled: c.Preload.Enabled, PlanSize: len(plan), LastRun: time.Now()}
	prev := w.stats.Load()
	st.Requests, st.Errors = prev.Requests, prev.Errors

	if !c.Preload.Enabled {
		w.stats.Store(st)
		return
	}
	if w.skip > 0 {
		w.skip--
		st.Backoff = true
		w.stats.Store(st)
		return
	}

	// Per-round budget derived from the per-minute allowance.
	budget := int(float64(c.Preload.MaxPerMinute) * c.Preload.Interval.Std().Seconds() / 60)
	if budget < 1 {
		budget = 1
	}
	targets := w.engine.Due(time.Now(), budget)
	st.LastCount = len(targets)
	// Everything in the plan that is not due is a request not made:
	// that is where most of the savings come from.
	if skipped := len(plan) - len(targets); skipped > 0 {
		w.m.PreloadSkipped(skipped)
	}
	for i, t := range targets {
		if i == 5 {
			break
		}
		st.Next = append(st.Next, t.Host+t.Path)
	}
	if len(targets) == 0 {
		w.stats.Store(st)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.Preload.Interval.Std())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	var (
		mu       sync.Mutex
		errCount int
		wg       sync.WaitGroup
	)
	jobs := make(chan learn.Target)
	for i := 0; i < c.Preload.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				if err := w.fetch(ctx, c, t); err != nil {
					mu.Lock()
					errCount++
					mu.Unlock()
					w.m.PreloadError()
					w.log.Warn("preload failed", "host", t.Host, "path", t.Path, "error", err.Error())
					continue
				}
				w.m.PreloadRequest()
			}
		}()
	}
	for _, t := range targets {
		select {
		case <-ctx.Done():
		case jobs <- t:
		}
	}
	close(jobs)
	wg.Wait()

	st.Requests += int64(len(targets) - errCount)
	st.Errors += int64(errCount)

	// A backend that fails everything is either down or reloading:
	// back off instead of hammering it.
	if errCount == len(targets) {
		w.fails++
		w.skip = min(1<<min(w.fails, 5), 32)
		st.Backoff = true
	} else {
		w.fails = 0
	}
	w.stats.Store(st)

	if c.Log.Learn {
		w.log.Info("preload round",
			"targets", len(targets), "errors", errCount, "plan", len(plan), "backend", w.backend().URL())
	}
}

// fetch warms one page.
func (w *Warmer) fetch(ctx context.Context, c *config.Config, t learn.Target) error {
	rctx, cancel := context.WithTimeout(ctx, c.Preload.Timeout.Std())
	defer cancel()

	b := w.backend()
	scheme := "http"
	if b.TLS {
		scheme = "https"
	}
	path := t.Path
	start := time.Now()

	for hop := 0; ; hop++ {
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, scheme+"://"+b.Addr+path, nil)
		if err != nil {
			return err
		}
		req.Host = t.Host
		req.Header.Set("User-Agent", "rukh/"+w.version+" (cache preloader)")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("Accept-Language", "*")
		// Look like the visitor rukh is warming the page for, so the
		// backend stores the entry under the key a visitor will hit.
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Host", t.Host)
		req.Header.Set("X-Forwarded-For", "127.0.0.1")
		req.Header.Set("X-Real-IP", "127.0.0.1")
		req.Header.Set(proxy.PreloadHeader, "1")

		resp, err := w.transport().RoundTrip(req)
		if err != nil {
			return err
		}
		io.CopyN(io.Discard, resp.Body, maxBody)
		resp.Body.Close()

		if isRedirect(resp.StatusCode) && hop < maxRedirects {
			loc := resp.Header.Get("Location")
			next, ok := sameHostPath(loc, t.Host)
			if ok && next != path {
				path = next
				continue
			}
		}

		w.engine.Observe(learn.Event{
			Kind:    learn.KindPreload,
			Host:    t.Host,
			Path:    t.Path,
			Status:  resp.StatusCode,
			Latency: time.Since(start),
		})
		if resp.StatusCode >= 500 {
			return fmt.Errorf("backend returned %d", resp.StatusCode)
		}
		return nil
	}
}

func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// sameHostPath returns the path of a Location header when it points at
// the same virtual host (absolute or relative), so a trailing-slash
// redirect can be followed without ever leaving the machine.
func sameHostPath(loc, host string) (string, bool) {
	if loc == "" {
		return "", false
	}
	if strings.HasPrefix(loc, "/") {
		return loc, true
	}
	for _, prefix := range []string{"http://", "https://"} {
		if rest, ok := strings.CutPrefix(loc, prefix); ok {
			h, path, _ := strings.Cut(rest, "/")
			if !strings.EqualFold(hostname(h), host) {
				return "", false
			}
			return "/" + path, true
		}
	}
	return "", false
}

func hostname(h string) string {
	if i := strings.IndexByte(h, ':'); i >= 0 {
		return h[:i]
	}
	return h
}

// Stats returns the last round's summary.
func (w *Warmer) Stats() Stats {
	s := *w.stats.Load()
	s.PlanSize = len(w.engine.Plan())
	return s
}
