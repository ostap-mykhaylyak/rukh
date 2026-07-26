package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
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

// backendEcho is a stand-in for nginx: it reports the headers it
// received and serves an HTML page plus a stylesheet.
func backendEcho(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(25 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>slow</html>")
	})
	mux.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		fmt.Fprint(w, "body{}")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Seen-Host", r.Host)
		w.Header().Set("X-Seen-XFF", r.Header.Get("X-Forwarded-For"))
		w.Header().Set("X-Seen-Real-IP", r.Header.Get("X-Real-IP"))
		w.Header().Set("X-Seen-Proto", r.Header.Get("X-Forwarded-Proto"))
		w.Header().Set("X-Seen-Preload", r.Header.Get(PreloadHeader))
		fmt.Fprint(w, "<html><body>hello</body></html>")
	})
	return httptest.NewServer(mux)
}

type testEnv struct {
	proxy   *Proxy
	engine  *learn.Engine
	front   *httptest.Server
	backend *httptest.Server
	stop    chan struct{}
}

func newEnv(t *testing.T, extra string) *testEnv {
	return newEnvWith(t, "", extra, "")
}

// newEnvHints is newEnv with a manual hints file for example.test,
// written before the proxy is built: nothing may be mutated once the
// handler is serving.
func newEnvHints(t *testing.T, hintsBody string) *testEnv {
	return newEnvWith(t, "", "", hintsBody)
}

// newEnvWith builds the test environment; serverExtra is merged into
// the server section, extra is appended as a new top-level section.
func newEnvWith(t *testing.T, serverExtra, extra, hintsBody string) *testEnv {
	t.Helper()
	be := backendEcho(t)
	dir := t.TempDir()

	hintsDir := filepath.Join(dir, "hints")
	os.MkdirAll(hintsDir, 0o755)
	if hintsBody != "" {
		if err := os.WriteFile(filepath.Join(hintsDir, "example.test.yaml"), []byte(hintsBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ngPath := filepath.Join(dir, "nginx.conf")
	os.WriteFile(ngPath, []byte("http { server { listen 127.0.0.1:8080; server_name example.test; } }"), 0o644)

	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`
server:
  http: "127.0.0.1:0"
  https: ""
%s
backend:
  address: %q
nginx:
  config: %q
hints:
  http1: true
  min_samples: 2
learn:
  rebuild_interval: "1ms"
  half_life: "1h"
%s
`, serverExtra, strings.TrimPrefix(be.URL, "http://"), filepath.ToSlash(ngPath), extra)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr, err := config.NewManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	logs := logging.Discard()
	ng := nginx.NewStore(ngPath, logs.Service)
	if err := ng.Load(); err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	c := mgr.Get()
	engine := learn.New(learn.Params{
		HalfLife: c.Learn.HalfLife.Std(), MaxHosts: 8, MaxPagesPerHost: 100,
		MaxAssetsPerPage: 20, MaxNextPerPage: 8,
		PruneInterval: time.Minute, RebuildInterval: c.Learn.RebuildInterval.Std(),
		MinScore: 0.01, QueueSize: 1024,
		HintsEnabled: true, HintMinConfidence: c.Hints.MinConfidence,
		HintMinSamples: c.Hints.MinSamples, HintMaxLinks: c.Hints.MaxLinks,
		HintMaxAge:      c.Hints.MaxAge.Std(),
		PrefetchEnabled: true, PrefetchMinProb: c.Prefetch.MinProbability,
		PrefetchMaxLinks: c.Prefetch.MaxLinks,
		PreloadMaxPages:  10, PreloadMinRefresh: time.Minute, PreloadMaxRefresh: time.Hour,
	}, m, logs.Learn)
	stop := make(chan struct{})
	engine.Start(stop)

	manual := hints.NewStore(hintsDir, logs.Service)
	manual.LoadAll()

	p := New(mgr, ng, engine, manual, m, logs)
	front := httptest.NewServer(p)

	env := &testEnv{proxy: p, engine: engine, front: front, backend: be, stop: stop}
	t.Cleanup(func() {
		close(stop)
		front.Close()
		be.Close()
	})
	return env
}

func (e *testEnv) get(t *testing.T, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.front.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.test"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestForwardsRealClientAddressAndHost(t *testing.T) {
	env := newEnv(t, "")
	resp := env.get(t, "/", nil)

	if got := resp.Header.Get("X-Seen-Host"); got != "example.test" {
		t.Errorf("backend saw Host %q, want the original one", got)
	}
	if got := resp.Header.Get("X-Seen-Real-IP"); got != "127.0.0.1" {
		t.Errorf("X-Real-IP = %q", got)
	}
	if got := resp.Header.Get("X-Seen-XFF"); got != "127.0.0.1" {
		t.Errorf("X-Forwarded-For = %q", got)
	}
	if got := resp.Header.Get("X-Seen-Proto"); got != "http" {
		t.Errorf("X-Forwarded-Proto = %q", got)
	}
}

func TestUntrustedClientCannotForgeItsAddress(t *testing.T) {
	env := newEnv(t, "")
	resp := env.get(t, "/", map[string]string{"X-Forwarded-For": "1.2.3.4"})
	if got := resp.Header.Get("X-Seen-XFF"); got != "127.0.0.1" {
		t.Fatalf("X-Forwarded-For = %q, want the peer only: the client is not a trusted proxy", got)
	}
	if got := resp.Header.Get("X-Seen-Real-IP"); got != "127.0.0.1" {
		t.Fatalf("X-Real-IP = %q, want the peer", got)
	}
}

func TestTrustedProxyChainIsPreserved(t *testing.T) {
	env := newEnv(t, "realip:\n  trusted_proxies: [\"127.0.0.0/8\"]\n")
	resp := env.get(t, "/", map[string]string{"X-Forwarded-For": "1.2.3.4"})
	if got := resp.Header.Get("X-Seen-XFF"); got != "1.2.3.4, 127.0.0.1" {
		t.Fatalf("X-Forwarded-For = %q, want the inbound chain plus the peer", got)
	}
	if got := resp.Header.Get("X-Seen-Real-IP"); got != "1.2.3.4" {
		t.Fatalf("X-Real-IP = %q, want the client behind the trusted proxy", got)
	}
}

func TestPreloadHeaderCannotBeSpoofed(t *testing.T) {
	env := newEnv(t, "")
	resp := env.get(t, "/", map[string]string{PreloadHeader: "1"})
	if got := resp.Header.Get("X-Seen-Preload"); got != "" {
		t.Fatalf("backend saw %s=%q from a client", PreloadHeader, got)
	}
}

// waitForHints polls until the ingest goroutine has folded the
// observations into the model.
func waitForHints(t *testing.T, e *learn.Engine, host, path string) []learn.Hint {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h := e.Hints(host, path); len(h) > 0 {
			return h
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no hints learned for %s%s", host, path)
	return nil
}

func TestLearnsPageResourcesAndSendsEarlyHints(t *testing.T) {
	env := newEnv(t, "")

	// Three visits, each loading the page and then its stylesheet.
	for i := 0; i < 3; i++ {
		env.get(t, "/", nil)
		env.get(t, "/style.css", map[string]string{
			"Referer":        "http://example.test/",
			"Sec-Fetch-Dest": "style",
		})
	}
	hints := waitForHints(t, env.engine, "example.test", "/")
	if hints[0].URL != "/style.css" || hints[0].As != "style" {
		t.Fatalf("hints = %+v", hints)
	}

	// The next navigation must receive a 103 with the Link header.
	var informational []string
	req, _ := http.NewRequest(http.MethodGet, env.front.URL+"/", nil)
	req.Host = "example.test"
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Sec-Fetch-Dest", "document")
	trace := &httptrace.ClientTrace{
		Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
			if code == http.StatusEarlyHints {
				informational = append(informational, h.Get("Link"))
			}
			return nil
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(context.Background(), trace))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(informational) != 1 {
		t.Fatalf("early hints responses = %v, want exactly one", informational)
	}
	if !strings.Contains(informational[0], "</style.css>; rel=preload; as=style") {
		t.Fatalf("103 Link = %q", informational[0])
	}
}

func TestPrefetchLinkOnPageResponse(t *testing.T) {
	env := newEnv(t, "")
	// Four visitors go from / to /next.
	env.get(t, "/", nil)
	for i := 0; i < 4; i++ {
		env.get(t, "/next", map[string]string{"Referer": "http://example.test/"})
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(env.engine.Prefetch("example.test", "/")) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp := env.get(t, "/", nil)
	found := false
	for _, l := range resp.Header.Values("Link") {
		if l == "</next>; rel=prefetch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Link headers = %v, want a prefetch for /next", resp.Header.Values("Link"))
	}
}

func TestNoHintsForUnknownPage(t *testing.T) {
	env := newEnv(t, "")
	var got int
	req, _ := http.NewRequest(http.MethodGet, env.front.URL+"/never-seen", nil)
	req.Host = "example.test"
	req.Header.Set("Sec-Fetch-Dest", "document")
	trace := &httptrace.ClientTrace{Got1xxResponse: func(int, textproto.MIMEHeader) error { got++; return nil }}
	req = req.WithContext(httptrace.WithClientTrace(context.Background(), trace))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != 0 {
		t.Fatalf("informational responses = %d, want none", got)
	}
}

// The access log must be able to answer "which resources are slow":
// that needs the kind of each request and the origin's own time.
func TestAccessLogRecordsKindAndOriginTime(t *testing.T) {
	var buf bytes.Buffer
	env := newEnv(t, "")
	env.proxy.logs.Access = slog.New(slog.NewJSONHandler(&buf, nil))

	env.get(t, "/slow", map[string]string{"Sec-Fetch-Dest": "document", "Accept": "text/html"})
	env.get(t, "/style.css", map[string]string{
		"Referer": "http://example.test/", "Sec-Fetch-Dest": "style",
	})

	var kinds []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var e struct {
			Kind     string  `json:"kind"`
			OriginMs float64 `json:"origin_ms"`
			Duration float64 `json:"duration_ms"`
			Path     string  `json:"path"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		kinds = append(kinds, e.Kind)
		// The origin's share can never exceed the whole request.
		if e.OriginMs > e.Duration {
			t.Errorf("%s: origin_ms %v > duration_ms %v", e.Path, e.OriginMs, e.Duration)
		}
		// The backend sleeps 25ms on /slow: that time must be charged
		// to the origin, not just to the total.
		if e.Path == "/slow" && e.OriginMs < 20 {
			t.Errorf("origin_ms = %v for a backend that took 25ms", e.OriginMs)
		}
	}
	if len(kinds) != 2 || kinds[0] != "page" || kinds[1] != "style" {
		t.Fatalf("kinds = %v, want [page style]", kinds)
	}
}

func TestBackendDownReturns502(t *testing.T) {
	env := newEnv(t, "")
	env.backend.Close()
	resp := env.get(t, "/", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestHTTPSRedirect(t *testing.T) {
	env := newEnvWith(t, "  redirect_https: true", "", "")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, _ := http.NewRequest(http.MethodGet, env.front.URL+"/page", nil)
	req.Host = "example.test"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://example.test/page" {
		t.Fatalf("Location = %q", got)
	}
	// The ACME webroot must never be redirected away.
	req, _ = http.NewRequest(http.MethodGet, env.front.URL+"/.well-known/acme-challenge/tok", nil)
	req.Host = "example.test"
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusMovedPermanently {
		t.Fatal("the ACME challenge path must be proxied, not redirected")
	}
}

func TestBackendAutoDetectionFromNginx(t *testing.T) {
	dir := t.TempDir()
	ngPath := filepath.Join(dir, "nginx.conf")
	os.WriteFile(ngPath, []byte(`http {
		server { listen 127.0.0.1:8080; server_name a.test; }
		server { listen 127.0.0.1:8443 ssl; server_name a.test; ssl_certificate c.pem; ssl_certificate_key c.key; }
	}`), 0o644)
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf("nginx:\n  config: %q\n", filepath.ToSlash(ngPath))), 0o644)

	mgr, err := config.NewManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	logs := logging.Discard()
	ng := nginx.NewStore(ngPath, logs.Service)
	if err := ng.Load(); err != nil {
		t.Fatal(err)
	}
	p := New(mgr, ng, learn.New(learn.Params{QueueSize: 16, HalfLife: time.Hour}, nil, logs.Learn),
		hints.NewStore(filepath.Join(dir, "hints"), logs.Service), metrics.New(), logs)
	b := p.Backend()
	if b.Addr != "127.0.0.1:8080" || b.TLS || !b.Auto {
		t.Fatalf("backend = %+v, want the loopback plain listener, auto-detected", b)
	}
}

// A prefetch rukh suggested comes back as an ordinary GET carrying the
// suggesting page as its Referer. If it were counted, the prediction
// would confirm itself and climb to certainty on its own.
func TestSpeculativeRequestsDoNotFeedTheModel(t *testing.T) {
	env := newEnv(t, "")

	// Real navigations: four visitors go from / to /next.
	env.get(t, "/", nil)
	for i := 0; i < 4; i++ {
		env.get(t, "/next", map[string]string{"Referer": "http://example.test/"})
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(env.engine.Prefetch("example.test", "/")) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := env.engine.Stats(0)

	// Now the browser acts on the hint, twenty times over.
	for i := 0; i < 20; i++ {
		env.get(t, "/next", map[string]string{
			"Referer":     "http://example.test/",
			"Sec-Purpose": "prefetch",
		})
	}
	time.Sleep(200 * time.Millisecond)

	after := env.engine.Stats(10)
	if after.Transitions != before.Transitions {
		t.Errorf("transitions %d -> %d: speculative traffic must not be learned",
			before.Transitions, after.Transitions)
	}
	for _, p := range after.TopPages {
		if p.Path == "/next" && p.Views > 4.5 {
			t.Errorf("/next has %.1f views: the prefetches were counted as visits", p.Views)
		}
	}
}

// A prefetched page is never rendered, so it must not carry hints of
// its own, and no page should ever suggest going back where the
// visitor came from.
func TestPrefetchLinksAreNotChainedOrSentBackwards(t *testing.T) {
	env := newEnv(t, "")

	// Teach the round trip: / -> /next and /next -> /.
	env.get(t, "/", nil)
	for i := 0; i < 4; i++ {
		env.get(t, "/next", map[string]string{"Referer": "http://example.test/"})
		env.get(t, "/", map[string]string{"Referer": "http://example.test/next"})
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(env.engine.Prefetch("example.test", "/next")) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Arriving on /next from /: the only candidate is / itself, which
	// is where the visitor came from.
	resp := env.get(t, "/next", map[string]string{"Referer": "http://example.test/"})
	for _, l := range resp.Header.Values("Link") {
		if strings.Contains(l, "rel=prefetch") {
			t.Errorf("suggested going back to the referring page: %q", l)
		}
	}

	// And a speculative request gets no suggestions at all.
	resp = env.get(t, "/", map[string]string{"Sec-Purpose": "prefetch"})
	for _, l := range resp.Header.Values("Link") {
		if strings.Contains(l, "rel=prefetch") {
			t.Errorf("chained a prefetch onto a prefetched page: %q", l)
		}
	}
}

// Behind a CDN the static resources never reach the origin, so the
// model cannot learn them: the manually configured ones must be
// announced from the very first request, and merged with whatever has
// been learned.
func TestManualHintsAreSentAndMergedWithLearnedOnes(t *testing.T) {
	env := newEnvHints(t, `
default:
  - /cdn/theme.css
paths:
  "/":
    - url: https://cdn.example.net
      rel: preconnect
`)

	// Nothing has been learned yet: the manual hints alone must fire.
	var got []string
	req, _ := http.NewRequest(http.MethodGet, env.front.URL+"/", nil)
	req.Host = "example.test"
	req.Header.Set("Sec-Fetch-Dest", "document")
	trace := &httptrace.ClientTrace{
		Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
			if code == http.StatusEarlyHints {
				got = h.Values("Link")
			}
			return nil
		},
	}
	resp, err := http.DefaultClient.Do(req.WithContext(httptrace.WithClientTrace(context.Background(), trace)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(got) != 2 || got[0] != "</cdn/theme.css>; rel=preload; as=style" {
		t.Fatalf("103 Link = %v", got)
	}
	if got[1] != "<https://cdn.example.net>; rel=preconnect; crossorigin" {
		t.Fatalf("path rule not applied: %v", got)
	}

	// Now teach it a resource that does reach the origin: both sources
	// must end up in the same response, manual first.
	for i := 0; i < 3; i++ {
		env.get(t, "/", nil)
		env.get(t, "/style.css", map[string]string{
			"Referer": "http://example.test/", "Sec-Fetch-Dest": "style",
		})
	}
	waitForHints(t, env.engine, "example.test", "/")

	got = nil
	resp, err = http.DefaultClient.Do(req.WithContext(httptrace.WithClientTrace(context.Background(), trace)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(got) != 3 {
		t.Fatalf("103 Link = %v, want the two manual entries plus the learned one", got)
	}
	if !strings.Contains(got[0], "/cdn/theme.css") {
		t.Errorf("manual hints must come first: %v", got)
	}
	if !strings.Contains(got[2], "/style.css") {
		t.Errorf("the learned resource is missing: %v", got)
	}
}
