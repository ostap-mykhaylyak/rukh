package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ostap-mykhaylyak/rukh/internal/config"
	"github.com/ostap-mykhaylyak/rukh/internal/hints"
	"github.com/ostap-mykhaylyak/rukh/internal/learn"
	"github.com/ostap-mykhaylyak/rukh/internal/logging"
	"github.com/ostap-mykhaylyak/rukh/internal/metrics"
	"github.com/ostap-mykhaylyak/rukh/internal/nginx"
)

// The benchmarks below answer one question: what does rukh cost per
// request, on top of forwarding? They compare the full handler with a
// bare httputil.ReverseProxy against the same backend, so the backend
// and the loopback hop cancel out and what is left is rukh's own work.

// benchBackend answers instantly with a small asset, the kind of
// request that dominates a page load.
func benchBackend(b *testing.B) *httptest.Server {
	body := strings.Repeat("x", 3000)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprint(w, body)
	}))
	b.Cleanup(s.Close)
	return s
}

// benchProxy builds the real handler, with the access log going to a
// real file (as in production) unless logTo is empty.
func benchProxy(b *testing.B, backendAddr, logDir string, accessLog bool) *Proxy {
	b.Helper()
	dir := b.TempDir()
	ngPath := filepath.Join(dir, "nginx.conf")
	os.WriteFile(ngPath, []byte("http { server { listen 127.0.0.1:8080; server_name bench.test; } }"), 0o644)

	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf(`
server:
  http: "127.0.0.1:0"
  https: ""
backend:
  address: %q
nginx:
  config: %q
log:
  access: %v
`, backendAddr, filepath.ToSlash(ngPath), accessLog)), 0o644)

	mgr, err := config.NewManager(cfgPath)
	if err != nil {
		b.Fatal(err)
	}
	var logs *logging.Streams
	if logDir == "" {
		logs = logging.Discard()
	} else {
		logs, err = logging.Open(logDir)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { logs.Close() })
	}
	ng := nginx.NewStore(ngPath, logs.Service)
	ng.Load()

	engine := learn.New(learn.Params{
		HalfLife: time.Hour, MaxHosts: 8, MaxPagesPerHost: 1000, MaxAssetsPerPage: 60,
		MaxNextPerPage: 32, PruneInterval: time.Minute, RebuildInterval: 5 * time.Second,
		MinScore: 0.05, QueueSize: 8192, HintsEnabled: true, HintMinSamples: 5,
		HintMinConfidence: 0.6, HintMaxLinks: 10, HintMaxAge: 24 * time.Hour,
		PrefetchEnabled: true, PrefetchMinProb: 0.35, PrefetchMaxLinks: 2,
		PreloadMaxPages: 200, PreloadMinRefresh: time.Minute, PreloadMaxRefresh: time.Hour,
	}, metrics.New(), logs.Learn)
	stop := make(chan struct{})
	engine.Start(stop)
	b.Cleanup(func() { close(stop) })

	manual := hints.NewStore(filepath.Join(dir, "hints"), logs.Service)
	manual.LoadAll()
	return New(mgr, ng, engine, manual, metrics.New(), logs)
}

func benchRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/wp-content/themes/x/app.js?ver=1784486021", nil)
	r.Host = "bench.test"
	r.Header.Set("Referer", "https://bench.test/prodotto/qualcosa")
	r.Header.Set("Sec-Fetch-Dest", "script")
	r.Header.Set("Accept-Encoding", "gzip, deflate, br")
	r.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
	return r
}

func run(b *testing.B, h http.Handler) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), benchRequest())
	}
}

// BenchmarkBareReverseProxy is the floor: forwarding and nothing else.
func BenchmarkBareReverseProxy(b *testing.B) {
	be := benchBackend(b)
	u, _ := url.Parse(be.URL)
	run(b, httputil.NewSingleHostReverseProxy(u))
}

// BenchmarkRukh is the whole handler as production runs it: access log
// on a real file, learning, hint lookup, metrics.
func BenchmarkRukh(b *testing.B) {
	be := benchBackend(b)
	run(b, benchProxy(b, strings.TrimPrefix(be.URL, "http://"), b.TempDir(), true))
}

// BenchmarkRukhNoAccessLog isolates the cost of the access log.
func BenchmarkRukhNoAccessLog(b *testing.B) {
	be := benchBackend(b)
	run(b, benchProxy(b, strings.TrimPrefix(be.URL, "http://"), b.TempDir(), false))
}

// BenchmarkRukhParallel checks the handler scales across cores: a
// per-request write serialized on one mutex would show up here as a
// result worse than the serial one.
func BenchmarkRukhParallel(b *testing.B) {
	be := benchBackend(b)
	p := benchProxy(b, strings.TrimPrefix(be.URL, "http://"), b.TempDir(), true)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p.ServeHTTP(httptest.NewRecorder(), benchRequest())
		}
	})
}

// BenchmarkRukhLayer measures only the work rukh adds around the
// forwarding — client resolution, classification, the hint lookup, the
// observation and the access line — with no network in the way. This
// is the number that answers "how much does rukh cost per request".
func BenchmarkRukhLayer(b *testing.B) {
	be := benchBackend(b)
	p := benchProxy(b, strings.TrimPrefix(be.URL, "http://"), b.TempDir(), true)
	c := p.cfg.Get()
	r := benchRequest()
	rip := p.rip.Load()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		host := hostOnly(r.Host)
		st := &reqState{
			start:     time.Now(),
			target:    r.URL.EscapedPath() + "?" + r.URL.RawQuery,
			client:    rip.clientIP(r),
			forwarded: rip.forwardedFor(r),
			origin:    time.Millisecond,
			as:        "script",
			status:    200,
		}
		p.earlyHints(discardWriter{}, r, c, host, st)
		p.observe(r, st, c, host, 2*time.Millisecond)
		p.logs.Access.Info("request",
			"host", host, "method", r.Method, "path", st.target,
			"status", 200, "bytes", int64(3000),
			"duration_ms", 2.0, "origin_ms", 1.0,
			"kind", st.kind(), "speculative", false,
			"client", st.client, "scheme", "https", "proto", "HTTP/2.0", "hints", 0)
	}
}

// discardWriter stands in for the client connection.
type discardWriter struct{}

func (discardWriter) Header() http.Header         { return http.Header{} }
func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriter) WriteHeader(int)             {}
