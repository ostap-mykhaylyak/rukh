package learn

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func testParams() Params {
	return Params{
		HalfLife:         time.Hour,
		MaxHosts:         4,
		MaxPagesPerHost:  20,
		MaxAssetsPerPage: 5,
		MaxNextPerPage:   4,
		PruneInterval:    time.Minute,
		RebuildInterval:  0, // recompute on every observation in tests
		MinScore:         0.05,
		QueueSize:        128,

		HintsEnabled:      true,
		HintMinConfidence: 0.6,
		HintMinSamples:    3,
		HintMaxLinks:      5,
		HintMaxAge:        24 * time.Hour,

		PrefetchEnabled:  true,
		PrefetchMinProb:  0.35,
		PrefetchMaxLinks: 2,

		PreloadMaxPages:   10,
		PreloadMinRefresh: time.Minute,
		PreloadMaxRefresh: time.Hour,
	}
}

func newTestEngine(t *testing.T, p Params) (*Engine, *time.Time) {
	t.Helper()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	e := New(p, nil, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	e.now = func() time.Time { return now }
	return e, &now
}

// view feeds one page view.
func (e *Engine) view(host, path string, at time.Time) {
	e.apply(Event{Kind: KindPage, Host: host, Path: path, Status: 200, Cacheable: true, At: at,
		Latency: 100 * time.Millisecond})
}

// hit feeds one subresource request attributed to ref.
func (e *Engine) hit(host, path, ref, as string, at time.Time) {
	e.apply(Event{Kind: KindAsset, Host: host, Path: path, Ref: ref, As: as, Status: 200, At: at})
}

func TestCounterDecaysByHalfEveryHalfLife(t *testing.T) {
	now := time.Now()
	var c counter
	c.add(now, time.Hour, 1)
	if got := c.value(now.Add(time.Hour), time.Hour); got < 0.49 || got > 0.51 {
		t.Fatalf("after one half-life: %v, want ~0.5", got)
	}
	if got := c.value(now.Add(3*time.Hour), time.Hour); got > 0.13 {
		t.Fatalf("after three half-lives: %v, want ~0.125", got)
	}
}

func TestHintsRequireConfidenceAndSamples(t *testing.T) {
	e, now := newTestEngine(t, testParams())
	for i := 0; i < 10; i++ {
		e.view("example.com", "/", *now)
		e.hit("example.com", "/style.css", "/", "style", *now)
		e.hit("example.com", "/app.js", "/", "script", *now)
	}
	// A resource only one visitor in ten needs must not be announced.
	e.hit("example.com", "/rare.js", "/", "script", *now)

	hints := e.Hints("example.com", "/")
	if len(hints) != 2 {
		t.Fatalf("hints = %+v, want the two reliable ones", hints)
	}
	if hints[0].As != "style" {
		t.Fatalf("stylesheets must come first: %+v", hints)
	}
	for _, h := range hints {
		if h.URL == "/rare.js" {
			t.Fatalf("unreliable resource announced: %+v", hints)
		}
	}
}

func TestHintsIgnoreUnknownResourceTypes(t *testing.T) {
	e, now := newTestEngine(t, testParams())
	for i := 0; i < 10; i++ {
		e.view("example.com", "/", *now)
		e.hit("example.com", "/data.bin", "/", "", *now)
	}
	if h := e.Hints("example.com", "/"); len(h) != 0 {
		t.Fatalf("hints = %+v, want none: nothing to preload as", h)
	}
}

func TestHintsAreCappedAndFontsGetCrossOrigin(t *testing.T) {
	p := testParams()
	p.HintMaxLinks = 2
	e, now := newTestEngine(t, p)
	for i := 0; i < 10; i++ {
		e.view("example.com", "/", *now)
		e.hit("example.com", "/a.css", "/", "style", *now)
		e.hit("example.com", "/f.woff2", "/", "font", *now)
		e.hit("example.com", "/b.js", "/", "script", *now)
	}
	hints := e.Hints("example.com", "/")
	if len(hints) != 2 {
		t.Fatalf("hints = %+v, want 2", hints)
	}
	if hints[1].As != "font" || !hints[1].CrossOrigin {
		t.Fatalf("font must be announced with crossorigin: %+v", hints)
	}
	links := LinkPreload(hints)
	if links[0] != "</a.css>; rel=preload; as=style" {
		t.Fatalf("link = %q", links[0])
	}
	if links[1] != "</f.woff2>; rel=preload; as=font; crossorigin" {
		t.Fatalf("link = %q", links[1])
	}
}

func TestAssetWithoutRefererIsAttributedToTheLastNavigation(t *testing.T) {
	e, now := newTestEngine(t, testParams())
	for i := 0; i < 5; i++ {
		e.apply(Event{Kind: KindPage, Host: "example.com", Path: "/shop", Status: 200,
			Cacheable: true, Client: "203.0.113.7", At: *now})
		e.apply(Event{Kind: KindAsset, Host: "example.com", Path: "/shop.css", As: "style",
			Client: "203.0.113.7", Status: 200, At: now.Add(time.Second)})
	}
	hints := e.Hints("example.com", "/shop")
	if len(hints) != 1 || hints[0].URL != "/shop.css" {
		t.Fatalf("hints = %+v, want the stylesheet attributed to /shop", hints)
	}
}

func TestStaleClientAttributionIsNotUsed(t *testing.T) {
	e, now := newTestEngine(t, testParams())
	e.apply(Event{Kind: KindPage, Host: "example.com", Path: "/shop", Status: 200,
		Cacheable: true, Client: "203.0.113.7", At: *now})
	late := now.Add(recentTTL + time.Minute)
	for i := 0; i < 5; i++ {
		e.apply(Event{Kind: KindAsset, Host: "example.com", Path: "/late.css", As: "style",
			Client: "203.0.113.7", Status: 200, At: late})
	}
	if h := e.Hints("example.com", "/shop"); len(h) != 0 {
		t.Fatalf("a subresource arriving much later must not be attributed: %+v", h)
	}
}

func TestNavigationPathsFeedPrefetch(t *testing.T) {
	e, now := newTestEngine(t, testParams())
	e.view("example.com", "/", *now)
	for i := 0; i < 8; i++ {
		e.apply(Event{Kind: KindPage, Host: "example.com", Path: "/products", Ref: "/",
			Status: 200, Cacheable: true, At: *now})
	}
	e.apply(Event{Kind: KindPage, Host: "example.com", Path: "/contact", Ref: "/",
		Status: 200, Cacheable: true, At: *now})

	next := e.Prefetch("example.com", "/")
	if len(next) != 1 || next[0] != "/products" {
		t.Fatalf("prefetch = %v, want only the likely destination", next)
	}
	if got := LinkPrefetch(next)[0]; got != "</products>; rel=prefetch" {
		t.Fatalf("link = %q", got)
	}
}

func TestPreloadPlanRanksAndSkipsPersonalizedPages(t *testing.T) {
	e, now := newTestEngine(t, testParams())
	for i := 0; i < 20; i++ {
		e.view("example.com", "/", *now)
	}
	for i := 0; i < 3; i++ {
		e.view("example.com", "/rare", *now)
	}
	// A personalized page: never a preload target.
	e.apply(Event{Kind: KindPage, Host: "example.com", Path: "/account", Status: 200,
		Cacheable: false, At: *now})
	e.RebuildPlan()

	plan := e.Plan()
	if len(plan) != 2 {
		t.Fatalf("plan = %+v, want the two public pages", plan)
	}
	if plan[0].Path != "/" {
		t.Fatalf("the hottest page must come first: %+v", plan)
	}
	if plan[0].Interval > plan[1].Interval {
		t.Fatalf("the hotter page must be refreshed more often: %+v", plan)
	}
	for _, tg := range plan {
		if tg.Path == "/account" {
			t.Fatal("a personalized page must never be preloaded")
		}
	}
}

func TestDueSkipsPagesRealVisitorsJustRequested(t *testing.T) {
	e, now := newTestEngine(t, testParams())
	for i := 0; i < 10; i++ {
		e.view("example.com", "/", *now)
	}
	e.RebuildPlan()

	if due := e.Due(*now, 10); len(due) != 0 {
		t.Fatalf("a page a visitor just loaded is already warm: %+v", due)
	}
	later := now.Add(2 * time.Hour)
	if due := e.Due(later, 10); len(due) != 1 {
		t.Fatalf("due after the refresh interval = %+v", due)
	}
	// Once warmed, it is not due again straight away.
	e.apply(Event{Kind: KindPreload, Host: "example.com", Path: "/", Status: 200, At: later})
	if due := e.Due(later.Add(time.Second), 10); len(due) != 0 {
		t.Fatalf("a page just warmed must not be warmed again: %+v", due)
	}
}

func TestPruneForgetsFadedEntries(t *testing.T) {
	e, now := newTestEngine(t, testParams())
	for i := 0; i < 5; i++ {
		e.view("example.com", "/gone", *now)
		e.hit("example.com", "/gone.css", "/gone", "style", *now)
	}
	if st := e.Stats(0); st.Pages != 1 || st.Assets != 1 {
		t.Fatalf("stats before prune = %+v", st)
	}
	// A day later, with a one-hour half-life, nothing is left.
	*now = now.Add(24 * time.Hour)
	e.prune()
	if st := e.Stats(0); st.Pages != 0 || st.Hosts != 0 {
		t.Fatalf("stats after prune = %+v, want an empty model", st)
	}
}

func TestCapsBoundTheModel(t *testing.T) {
	p := testParams()
	e, now := newTestEngine(t, p)

	e.view("example.com", "/", *now)
	for i := 0; i < 50; i++ {
		e.hit("example.com", "/a"+string(rune('a'+i%26))+".css", "/", "style", *now)
	}
	e.mu.RLock()
	n := len(e.hosts["example.com"].pages["/"].assets)
	e.mu.RUnlock()
	if n > p.MaxAssetsPerPage {
		t.Fatalf("assets = %d, want at most %d", n, p.MaxAssetsPerPage)
	}

	for i := 0; i < 100; i++ {
		e.view("example.com", "/p"+string(rune('a'+i%26))+string(rune('a'+i/26)), *now)
	}
	if st := e.Stats(0); st.Pages > p.MaxPagesPerHost {
		t.Fatalf("pages = %d, want at most %d", st.Pages, p.MaxPagesPerHost)
	}

	for i := 0; i < 10; i++ {
		e.view("host"+string(rune('a'+i))+".example", "/", *now)
	}
	if st := e.Stats(0); st.Hosts > p.MaxHosts {
		t.Fatalf("hosts = %d, want at most %d", st.Hosts, p.MaxHosts)
	}
}

func TestObserveNeverBlocks(t *testing.T) {
	p := testParams()
	p.QueueSize = 64
	e, now := newTestEngine(t, p)
	for i := 0; i < 1000; i++ { // nothing is draining the queue
		e.Observe(Event{Kind: KindPage, Host: "example.com", Path: "/", At: *now})
	}
	if e.dropped.Load() == 0 {
		t.Fatal("expected observations to be dropped instead of blocking")
	}
}

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"":                                "/",
		"/":                               "/",
		"/page?utm_source=news&id=7":      "/page?id=7",
		"/page?fbclid=x":                  "/page",
		"/page?b=2&a=1":                   "/page?a=1&b=2",
		"/page#anchor":                    "/page",
		"/search?q=hello%20world&gclid=1": "/search?q=hello%20world",
	}
	for in, want := range cases {
		if got := NormalizePath(in); got != want {
			t.Errorf("NormalizePath(%q) = %q, want %q", in, got, want)
		}
	}
	long := "/" + string(make([]byte, 900))
	if got := NormalizePath(long); len(got) != 512 {
		t.Errorf("long path not capped: %d", len(got))
	}
}

func TestEscapeURLDropsHeaderInjection(t *testing.T) {
	h := []Hint{{URL: "/a.css\r\nX-Evil: 1", As: "style"}}
	got := LinkPreload(h)[0]
	if got != "</a.cssX-Evil:%201>; rel=preload; as=style" {
		t.Fatalf("link = %q", got)
	}
}
