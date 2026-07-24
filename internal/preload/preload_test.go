package preload

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ostap-mykhaylyak/rukh/internal/config"
	"github.com/ostap-mykhaylyak/rukh/internal/learn"
	"github.com/ostap-mykhaylyak/rukh/internal/metrics"
	"github.com/ostap-mykhaylyak/rukh/internal/proxy"
)

type seen struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (s *seen) add(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r.Clone(r.Context()))
}

func (s *seen) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

func newWarmer(t *testing.T, extra string) (*Warmer, *learn.Engine, *seen, *httptest.Server) {
	t.Helper()
	rec := &seen{}
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusMovedPermanently)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html></html>")
	}))
	t.Cleanup(be.Close)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte("preload:\n  interval: \"1s\"\n"+extra), 0o644)
	mgr, err := config.NewManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	engine := learn.New(learn.Params{
		HalfLife: time.Hour, MaxHosts: 8, MaxPagesPerHost: 100, MaxAssetsPerPage: 10,
		MaxNextPerPage: 8, PruneInterval: time.Minute, RebuildInterval: time.Millisecond,
		MinScore: 0.01, QueueSize: 256,
		PreloadMaxPages: 10, PreloadMinRefresh: time.Minute, PreloadMaxRefresh: time.Hour,
	}, nil, log)

	addr := strings.TrimPrefix(be.URL, "http://")
	backend := func() proxy.Backend { return proxy.Backend{Addr: addr} }
	transport := func() *http.Transport { return http.DefaultTransport.(*http.Transport) }
	w := New(engine, mgr, backend, transport, metrics.New(), log, "test")
	return w, engine, rec, be
}

// teach feeds n views of a page far enough in the past that the page
// is due for a warm-up.
func teach(e *learn.Engine, host, path string, n int) {
	at := time.Now().Add(-2 * time.Hour)
	for i := 0; i < n; i++ {
		e.Observe(learn.Event{Kind: learn.KindPage, Host: host, Path: path,
			Status: 200, Cacheable: true, At: at, Latency: 50 * time.Millisecond})
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestWarmsDuePagesWithTheMarkerHeader(t *testing.T) {
	w, engine, rec, _ := newWarmer(t, "")
	stop := make(chan struct{})
	defer close(stop)
	engine.Start(stop)

	teach(engine, "example.test", "/", 10)
	waitFor(t, func() bool { return engine.Stats(0).Pages == 1 })
	engine.RebuildPlan()

	w.round(stop)
	waitFor(t, func() bool { return rec.len() == 1 })

	rec.mu.Lock()
	got := rec.reqs[0]
	rec.mu.Unlock()
	if got.Host != "example.test" {
		t.Errorf("Host = %q, want the virtual host being warmed", got.Host)
	}
	if got.Header.Get(proxy.PreloadHeader) == "" {
		t.Errorf("warm-up request must carry %s", proxy.PreloadHeader)
	}
	if !strings.Contains(got.Header.Get("User-Agent"), "cache preloader") {
		t.Errorf("User-Agent = %q", got.Header.Get("User-Agent"))
	}
	if got.Header.Get("X-Forwarded-Proto") != "https" {
		t.Errorf("X-Forwarded-Proto = %q", got.Header.Get("X-Forwarded-Proto"))
	}

	// The page is now warm: the next round must not fetch it again.
	waitFor(t, func() bool { return len(engine.Due(time.Now(), 10)) == 0 })
	w.round(stop)
	time.Sleep(50 * time.Millisecond)
	if rec.len() != 1 {
		t.Fatalf("requests = %d, want no second warm-up of a fresh page", rec.len())
	}
}

func TestDisabledPreloaderDoesNothing(t *testing.T) {
	w, engine, rec, _ := newWarmer(t, "  enabled: false\n")
	stop := make(chan struct{})
	defer close(stop)
	engine.Start(stop)

	teach(engine, "example.test", "/", 10)
	waitFor(t, func() bool { return engine.Stats(0).Pages == 1 })
	engine.RebuildPlan()

	w.round(stop)
	time.Sleep(50 * time.Millisecond)
	if rec.len() != 0 {
		t.Fatalf("requests = %d, want none when preload is disabled", rec.len())
	}
	if w.Stats().Enabled {
		t.Error("stats must report the preloader as disabled")
	}
}

func TestBudgetLimitsTheRound(t *testing.T) {
	// 60 per minute over a 1s interval means one page per round.
	w, engine, rec, _ := newWarmer(t, "  max_per_minute: 60\n")
	stop := make(chan struct{})
	defer close(stop)
	engine.Start(stop)

	for i := 0; i < 5; i++ {
		teach(engine, "example.test", fmt.Sprintf("/p%d", i), 10-i)
	}
	waitFor(t, func() bool { return engine.Stats(0).Pages == 5 })
	engine.RebuildPlan()

	w.round(stop)
	time.Sleep(100 * time.Millisecond)
	if rec.len() != 1 {
		t.Fatalf("requests = %d, want the per-minute budget to allow exactly one", rec.len())
	}
}

func TestFollowsSameHostRedirect(t *testing.T) {
	w, engine, rec, _ := newWarmer(t, "")
	stop := make(chan struct{})
	defer close(stop)
	engine.Start(stop)

	teach(engine, "example.test", "/old", 10)
	waitFor(t, func() bool { return engine.Stats(0).Pages == 1 })
	engine.RebuildPlan()

	w.round(stop)
	waitFor(t, func() bool { return rec.len() == 2 })
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.reqs[1].URL.Path != "/new" {
		t.Fatalf("second request = %q, want the redirect target", rec.reqs[1].URL.Path)
	}
}

func TestBacksOffWhenTheBackendIsDown(t *testing.T) {
	w, engine, _, be := newWarmer(t, "")
	stop := make(chan struct{})
	defer close(stop)
	engine.Start(stop)

	teach(engine, "example.test", "/", 10)
	waitFor(t, func() bool { return engine.Stats(0).Pages == 1 })
	engine.RebuildPlan()
	be.Close()

	w.round(stop)
	if !w.Stats().Backoff {
		t.Fatal("a round where everything failed must switch to backoff")
	}
	if w.Stats().Errors == 0 {
		t.Fatal("errors not counted")
	}
}

func TestSameHostPath(t *testing.T) {
	cases := []struct {
		loc, host, want string
		ok              bool
	}{
		{"/new", "a.test", "/new", true},
		{"https://a.test/new", "a.test", "/new", true},
		{"http://a.test:8080/new", "a.test", "/new", true},
		{"https://other.test/new", "a.test", "", false},
		{"", "a.test", "", false},
	}
	for _, c := range cases {
		got, ok := sameHostPath(c.loc, c.host)
		if got != c.want || ok != c.ok {
			t.Errorf("sameHostPath(%q, %q) = %q, %v", c.loc, c.host, got, ok)
		}
	}
}
