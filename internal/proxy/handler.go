// Package proxy is the request path: it forwards everything to nginx,
// hands nginx the real client address, and uses what the traffic model
// has learned to send 103 Early Hints and prefetch hints on the way.
//
// rukh never decides which virtual host serves a request: nginx does,
// exactly as it does today. rukh only terminates TLS (with nginx's own
// certificates), forwards, observes, and optimizes.
package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ostap-mykhaylyak/rukh/internal/config"
	"github.com/ostap-mykhaylyak/rukh/internal/learn"
	"github.com/ostap-mykhaylyak/rukh/internal/logging"
	"github.com/ostap-mykhaylyak/rukh/internal/metrics"
	"github.com/ostap-mykhaylyak/rukh/internal/nginx"
)

// PreloadHeader marks a warm-up request issued by rukh itself, so the
// backend (and rukh) can tell it apart from a real visitor. It is
// stripped from every inbound request: a client must not be able to
// impersonate the preloader.
const PreloadHeader = "X-Rukh-Preload"

const preloadHeader = PreloadHeader

// Backend is where nginx is reachable.
type Backend struct {
	Addr string
	TLS  bool
	// Auto records that the address was discovered in the nginx
	// configuration rather than configured by the operator.
	Auto bool
}

// URL returns the scheme://host form, for logs and status.
func (b Backend) URL() string {
	if b.TLS {
		return "https://" + b.Addr
	}
	return "http://" + b.Addr
}

// Proxy is the HTTP handler in front of nginx.
type Proxy struct {
	cfg    *config.Manager
	ng     *nginx.Store
	engine *learn.Engine
	m      *metrics.Metrics
	logs   *logging.Streams

	rip     atomic.Pointer[realIP]
	backend atomic.Pointer[Backend]
	tr      atomic.Pointer[http.Transport]
	sniTr   sync.Map // host -> *http.Transport, only when the backend is TLS

	rp *httputil.ReverseProxy
}

// stateKey carries the per-request observation state from
// ModifyResponse back to ServeHTTP.
type stateKey struct{}

type reqState struct {
	start time.Time
	// origin is the time nginx took to produce the response headers.
	// It is what the origin is responsible for; the total request
	// duration also contains sending the body to the client, which a
	// slow phone inflates and the backend cannot be blamed for.
	origin    time.Duration
	page      bool
	as        string
	status    int
	cacheable bool
}

// kind labels a request in the access log, so "which resources are
// slow" is one query away.
func (s *reqState) kind() string {
	switch {
	case s.page:
		return "page"
	case s.as != "":
		return s.as
	default:
		return "other"
	}
}

// New builds the handler. Reconfigure must be called once (and on
// every reload) to install the transport and the backend address.
func New(cfg *config.Manager, ng *nginx.Store, engine *learn.Engine, m *metrics.Metrics, logs *logging.Streams) *Proxy {
	p := &Proxy{cfg: cfg, ng: ng, engine: engine, m: m, logs: logs}
	p.rp = &httputil.ReverseProxy{
		Rewrite:        p.rewrite,
		Transport:      roundTripper{p},
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.errorHandler,
	}
	p.Reconfigure()
	return p
}

// Reconfigure rebuilds everything that depends on the configuration or
// on the discovered nginx layout: the client-address resolver, the
// upstream address and the HTTP transport.
func (p *Proxy) Reconfigure() {
	c := p.cfg.Get()
	p.rip.Store(newRealIP(c.RealIP.Header, c.RealIP.TrustedProxies))

	b := p.resolveBackend(c)
	old := p.backend.Load()
	p.backend.Store(&b)
	if old == nil || old.Addr != b.Addr || old.TLS != b.TLS {
		p.logs.Service.Info("upstream selected", "backend", b.URL(), "auto", b.Auto)
		p.sniTr.Range(func(k, _ any) bool { p.sniTr.Delete(k); return true })
	}

	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   c.Backend.MaxIdleConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: c.Backend.Timeout.Std(),
		ExpectContinueTimeout: time.Second,
		// The client's Accept-Encoding is forwarded untouched: nginx
		// keeps deciding what to compress, rukh never re-encodes.
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
	}
	if b.TLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: c.Backend.TLSSkipVerify} //nolint:gosec // loopback hop to nginx
	}
	if old := p.tr.Swap(tr); old != nil {
		go func() {
			time.Sleep(30 * time.Second) // let in-flight requests finish
			old.CloseIdleConnections()
		}()
	}
}

// resolveBackend picks the address of nginx: the configured one, or
// the first listener discovered in the nginx configuration that rukh
// does not bind itself.
func (p *Proxy) resolveBackend(c *config.Config) Backend {
	if c.Backend.Address != "" {
		return Backend{Addr: c.Backend.Address, TLS: c.Backend.TLS}
	}
	taken := []string{}
	if c.Server.HTTP != "" {
		taken = append(taken, c.Server.HTTP)
	}
	if c.Server.HTTPS != "" {
		taken = append(taken, c.Server.HTTPS)
	}
	if cands := p.ng.Get().Backends(taken); len(cands) > 0 {
		return Backend{Addr: cands[0].Addr, TLS: cands[0].SSL, Auto: true}
	}
	return Backend{Addr: "127.0.0.1:8080", Auto: true}
}

// Backend returns the current upstream (used by the preloader and by
// `rukh status`).
func (p *Proxy) Backend() Backend { return *p.backend.Load() }

// Transport returns the shared transport (used by the preloader, so
// warm-up requests reuse the same connection pool).
func (p *Proxy) Transport() *http.Transport { return p.tr.Load() }

// roundTripper indirects through the Proxy so a configuration reload
// can swap the transport without rebuilding the ReverseProxy, and so a
// TLS upstream gets the right SNI per virtual host.
type roundTripper struct{ p *Proxy }

func (rt roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return rt.p.transportFor(r.Host).RoundTrip(r)
}

func (p *Proxy) transportFor(host string) *http.Transport {
	base := p.tr.Load()
	b := p.backend.Load()
	if !b.TLS || host == "" {
		return base
	}
	name := hostOnly(host)
	if v, ok := p.sniTr.Load(name); ok {
		return v.(*http.Transport)
	}
	// Same settings as the shared transport, with the SNI nginx needs
	// to pick the right server block.
	tr := base.Clone()
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // loopback hop to nginx
	} else {
		tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	}
	tr.TLSClientConfig.ServerName = name
	actual, _ := p.sniTr.LoadOrStore(name, tr)
	return actual.(*http.Transport)
}

// rewrite prepares the request for nginx.
func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	b := p.backend.Load()
	rip := p.rip.Load()

	pr.Out.URL.Scheme = "http"
	if b.TLS {
		pr.Out.URL.Scheme = "https"
	}
	pr.Out.URL.Host = b.Addr
	pr.Out.Host = pr.In.Host // nginx routes on the original Host

	proto := "http"
	port := portOf(pr.In.Context())
	if pr.In.TLS != nil {
		proto = "https"
	}
	pr.Out.Header.Set("X-Forwarded-For", rip.forwardedFor(pr.In))
	pr.Out.Header.Set("X-Real-IP", rip.clientIP(pr.In))
	pr.Out.Header.Set("X-Forwarded-Proto", proto)
	pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
	if port != "" {
		pr.Out.Header.Set("X-Forwarded-Port", port)
	}
	// A client must never be able to impersonate the preloader.
	pr.Out.Header.Del(preloadHeader)
}

// listenPortKey carries the port of the accepting listener.
type listenPortKey struct{}

func portOf(ctx context.Context) string {
	if v, ok := ctx.Value(listenPortKey{}).(string); ok {
		return v
	}
	return ""
}

// ServeHTTP is the request path.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := p.cfg.Get()
	done := p.m.RequestStart()
	start := time.Now()

	host := hostOnly(r.Host)
	target := r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	key := learn.NormalizePath(target)

	if r.TLS == nil && c.Server.RedirectHTTPS && !strings.HasPrefix(r.URL.Path, "/.well-known/") {
		u := *r.URL
		u.Scheme, u.Host = "https", r.Host
		http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
		done(0, false)
		return
	}

	hints := p.earlyHints(w, r, c, host, key)

	st := &reqState{start: start}
	rec := &recorder{ResponseWriter: w}
	p.rp.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), stateKey{}, st)))

	elapsed := time.Since(start)
	done(rec.bytes, rec.status >= 500)

	p.observe(r, st, c, host, key, elapsed)

	if c.Log.Access {
		p.logs.Access.Info("request",
			"host", host,
			"method", r.Method,
			"path", target,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", float64(elapsed.Microseconds())/1000,
			"origin_ms", float64(st.origin.Microseconds())/1000,
			"kind", st.kind(),
			"speculative", isSpeculative(r),
			"client", p.rip.Load().clientIP(r),
			"proto", r.Proto,
			"hints", hints,
		)
	}
}

// earlyHints sends the 103 response when the model knows what this
// page pulls in, and returns how many resources were announced.
func (p *Proxy) earlyHints(w http.ResponseWriter, r *http.Request, c *config.Config, host, key string) int {
	if !c.Hints.Enabled || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return 0
	}
	// 103 travels badly over HTTP/1.1 through some intermediaries and
	// browsers only act on it over HTTP/2 and HTTP/3, so it is off by
	// default there.
	if r.ProtoMajor < 2 && !c.Hints.HTTP1 {
		return 0
	}
	if !wantsHTML(r) {
		return 0
	}
	// A speculative request (prefetch/prerender) is not a real visit:
	// hinting it would waste the visitor's bandwidth twice.
	if r.Header.Get("Sec-Purpose") != "" || r.Header.Get("Purpose") == "prefetch" {
		return 0
	}
	links := learn.LinkPreload(p.engine.Hints(host, key))
	if len(links) == 0 {
		return 0
	}
	h := w.Header()
	for _, l := range links {
		h.Add("Link", l)
	}
	w.WriteHeader(http.StatusEarlyHints)
	p.m.HintsSent(len(links))
	if c.Log.Learn {
		p.logs.Learn.Info("early hints", "host", host, "path", key, "links", len(links))
	}
	return len(links)
}

// modifyResponse classifies the response and, for a page, advertises
// the most likely next navigation as a prefetch link.
func (p *Proxy) modifyResponse(resp *http.Response) error {
	st, _ := resp.Request.Context().Value(stateKey{}).(*reqState)
	if st == nil {
		return nil
	}
	c := p.cfg.Get()
	// ModifyResponse runs as soon as the backend's headers arrive and
	// before the body is streamed: this is nginx's time to first byte.
	if !st.start.IsZero() {
		st.origin = time.Since(st.start)
	}
	ct := resp.Header.Get("Content-Type")
	st.status = resp.StatusCode
	st.cacheable = cacheableResponse(resp)

	switch {
	case isHTML(ct):
		st.page = true
	default:
		st.as = asType(resp.Request, ct)
	}

	// A page the browser fetched speculatively is never rendered, so it
	// gets no suggestions of its own: hinting it would chain one
	// speculation to the next.
	if st.page && resp.StatusCode == http.StatusOK && c.Prefetch.Enabled && !isSpeculative(resp.Request) {
		host := hostOnly(resp.Request.Host)
		target := resp.Request.URL.EscapedPath()
		if resp.Request.URL.RawQuery != "" {
			target += "?" + resp.Request.URL.RawQuery
		}
		key := learn.NormalizePath(target)
		// Never suggest the page the visitor just came from: going back
		// is the most common transition there is, and the browser
		// already has that page. Nor the page itself.
		back := refererPath(resp.Request, resp.Request.Host)
		if back != "" {
			back = learn.NormalizePath(back)
		}
		var next []string
		for _, n := range p.engine.Prefetch(host, key) {
			if n == key || n == back {
				continue
			}
			next = append(next, n)
		}
		if links := learn.LinkPrefetch(next); len(links) > 0 {
			for _, l := range links {
				resp.Header.Add("Link", l)
			}
			p.m.PrefetchLinks(len(links))
		}
	}
	return nil
}

// observe feeds the traffic model. Only successful, non-authenticated
// GET traffic teaches anything: an error page has no resources worth
// preloading and a logged-in page is personalized by definition.
func (p *Proxy) observe(r *http.Request, st *reqState, c *config.Config, host, key string, elapsed time.Duration) {
	// The preloader prioritizes slow pages: "slow" must mean slow at
	// the origin, otherwise a visitor on a bad connection would make a
	// fast page look expensive.
	latency := st.origin
	if latency <= 0 {
		latency = elapsed
	}
	if st.status != http.StatusOK || r.Method != http.MethodGet {
		return
	}
	if r.Header.Get("Authorization") != "" {
		return
	}
	if r.Header.Get(preloadHeader) != "" {
		return // never learn from our own warm-up traffic
	}
	if isSpeculative(r) {
		// Nor from a prefetch rukh itself asked for: a model trained on
		// its own predictions stops describing the visitors.
		return
	}
	client := peer(r)
	// An empty referrer must stay empty: normalizing it would turn it
	// into "/" and credit the homepage for resources it never loaded.
	ref := refererPath(r, r.Host)
	if ref != "" {
		ref = learn.NormalizePath(ref)
	}
	switch {
	case st.page:
		p.m.PageView()
		p.engine.Observe(learn.Event{
			Kind:      learn.KindPage,
			Host:      host,
			Path:      key,
			Ref:       ref,
			Client:    client,
			Status:    st.status,
			Latency:   latency,
			Cacheable: st.cacheable,
		})
	case st.as != "":
		p.m.AssetHit()
		target := r.URL.EscapedPath()
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		p.engine.Observe(learn.Event{
			Kind:   learn.KindAsset,
			Host:   host,
			Path:   target, // assets keep their version query
			Ref:    ref,
			As:     st.as,
			Client: client,
			Status: st.status,
		})
	}
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	p.logs.Service.Error("upstream error",
		"backend", p.backend.Load().URL(),
		"host", r.Host, "path", r.URL.Path, "error", err.Error())
	code := http.StatusBadGateway
	if errors.Is(err, context.Canceled) {
		code = 499 // client went away: nothing to report
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintln(w, http.StatusText(http.StatusBadGateway))
}

// recorder counts the bytes written and remembers the final status
// code. Informational responses (the 103 we send ourselves) pass
// straight through without being recorded as the final status.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		r.ResponseWriter.WriteHeader(code)
		return
	}
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Flush keeps streaming responses (SSE, chunked) working.
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets net/http reach the underlying writer for protocol
// upgrades (WebSocket) and for ResponseController.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
