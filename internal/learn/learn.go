// Package learn is the traffic model of rukh: what pages exist, which
// static resources each page pulls in, where visitors go next, and how
// slow the origin is for each page.
//
// Everything lives in memory. There is no database, no persistence and
// no export: the model is rebuilt from live traffic within minutes of
// a restart, every counter decays exponentially so recent traffic
// always dominates, and hard caps plus a periodic sweep keep the
// footprint bounded on a machine that never restarts.
//
// Concurrency: the model is guarded by one RWMutex. Requests only take
// the read lock (hint lookup, prefetch lookup); every mutation happens
// in the single ingest goroutine, which drains the observation queue
// in batches. Observations are best-effort: if the queue is full they
// are dropped rather than slowing a request down.
package learn

import (
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Kind classifies an observation.
type Kind uint8

const (
	// KindPage is a navigation request that returned an HTML page.
	KindPage Kind = iota
	// KindAsset is a subresource request (script, style, font, image).
	KindAsset
	// KindPreload is the preloader reporting that it warmed a page.
	KindPreload
)

// Event is one observation coming off the request path. It is a value
// type on purpose: the hot path fills it on the stack and hands it to
// the queue without allocating.
type Event struct {
	Kind    Kind
	Host    string
	Path    string // normalized request target
	Ref     string // referring page path on the same host, "" if unknown
	As      string // preload "as" type for assets: style|script|font|image
	Client  string // client address, used to attribute referrer-less assets
	Status  int
	Latency time.Duration
	// Cacheable is false when the response looked personalized
	// (Set-Cookie, no-store, private): such pages are never preloaded.
	Cacheable bool
	At        time.Time
}

// Params are the tunables, mirrored from the configuration so this
// package does not depend on it. Swapped atomically on reload.
type Params struct {
	HalfLife         time.Duration
	MaxHosts         int
	MaxPagesPerHost  int
	MaxAssetsPerPage int
	MaxNextPerPage   int
	PruneInterval    time.Duration
	RebuildInterval  time.Duration
	MinScore         float64
	QueueSize        int

	HintsEnabled      bool
	HintMinConfidence float64
	HintMinSamples    float64
	HintMaxLinks      int
	HintMaxAge        time.Duration

	PrefetchEnabled  bool
	PrefetchMinProb  float64
	PrefetchMaxLinks int

	PreloadMaxPages   int
	PreloadMinRefresh time.Duration
	PreloadMaxRefresh time.Duration
}

// Hint is one resource worth announcing in a 103 Early Hints response.
type Hint struct {
	URL         string  `json:"url"`
	As          string  `json:"as"`
	Confidence  float64 `json:"confidence"`
	CrossOrigin bool    `json:"crossorigin"`
}

// Target is one page the cache preloader may warm.
type Target struct {
	Host     string        `json:"host"`
	Path     string        `json:"path"`
	Score    float64       `json:"score"`
	Interval time.Duration `json:"interval"`
}

// Metrics is the subset of the metrics collector this package uses.
type Metrics interface {
	LearnEvent()
	LearnDropped()
}

// Engine owns the model and the ingest loop.
type Engine struct {
	mu    sync.RWMutex
	hosts map[string]*host

	params atomic.Pointer[Params]
	queue  chan Event
	m      Metrics
	log    *slog.Logger

	plan atomic.Pointer[[]Target]

	// recent maps a client address to the page it last navigated to,
	// used to attribute subresources when the browser sends no usable
	// Referer. Only touched by the ingest goroutine.
	recent map[string]recentPage

	dropped atomic.Int64
	events  atomic.Int64

	now func() time.Time // test hook
}

type recentPage struct {
	host string
	path string
	at   time.Time
}

// recentTTL is how long a navigation stays usable to attribute a
// referrer-less subresource to the page that pulled it in.
const recentTTL = 20 * time.Second

// maxRecent caps the client->last page table (memory, not accuracy).
const maxRecent = 8192

type host struct {
	pages    map[string]*page
	views    counter
	lastSeen time.Time
}

type page struct {
	views  counter
	assets map[string]*asset
	next   map[string]*counter

	lastSeen  time.Time // last real user request
	lastFetch time.Time // last time the page was warmed by the preloader
	latency   ewma
	status    int
	html      bool
	cacheable bool

	// hints is the precomputed Early Hints list, refreshed at most
	// once per RebuildInterval by the ingest goroutine, so the request
	// path only reads an immutable slice.
	hints    []Hint
	hintsAt  time.Time
	prefetch []string
}

type asset struct {
	hits     counter
	as       string
	lastSeen time.Time
}

// New returns an Engine. Start must be called to run the ingest loop.
func New(p Params, m Metrics, log *slog.Logger) *Engine {
	e := &Engine{
		hosts:  map[string]*host{},
		queue:  make(chan Event, p.QueueSize),
		m:      m,
		log:    log,
		recent: map[string]recentPage{},
		now:    time.Now,
	}
	e.params.Store(&p)
	empty := []Target{}
	e.plan.Store(&empty)
	return e
}

// SetParams swaps the tunables (configuration reload). The queue size
// is fixed at construction; everything else takes effect immediately.
func (e *Engine) SetParams(p Params) { e.params.Store(&p) }

func (e *Engine) p() *Params { return e.params.Load() }

// Observe queues an observation. Never blocks: under a flood the
// oldest information is simply not collected, which costs accuracy,
// never latency.
func (e *Engine) Observe(ev Event) {
	if ev.At.IsZero() {
		ev.At = e.now()
	}
	select {
	case e.queue <- ev:
		e.events.Add(1)
		if e.m != nil {
			e.m.LearnEvent()
		}
	default:
		e.dropped.Add(1)
		if e.m != nil {
			e.m.LearnDropped()
		}
	}
}

// Start runs the ingest loop until stop is closed.
func (e *Engine) Start(stop <-chan struct{}) {
	go func() {
		prune := time.NewTicker(e.p().PruneInterval)
		defer prune.Stop()
		plan := time.NewTicker(e.p().PreloadPlanInterval())
		defer plan.Stop()
		for {
			select {
			case <-stop:
				return
			case ev := <-e.queue:
				e.apply(ev)
				// Drain whatever else is already queued under the same
				// lock acquisition: batching keeps the write lock rare.
				for n := 0; n < 256; n++ {
					select {
					case ev := <-e.queue:
						e.apply(ev)
					default:
						n = 256
					}
				}
			case <-prune.C:
				e.prune()
			case <-plan.C:
				e.RebuildPlan()
			}
		}
	}()
}

// PreloadPlanInterval is how often the preload plan is recomputed. It
// follows the prune interval: the plan is only a ranking of what the
// model already knows.
func (p *Params) PreloadPlanInterval() time.Duration {
	if p.PruneInterval > 0 {
		return p.PruneInterval
	}
	return time.Minute
}

// apply folds one observation into the model.
func (e *Engine) apply(ev Event) {
	p := e.p()
	e.mu.Lock()
	defer e.mu.Unlock()

	switch ev.Kind {
	case KindPage:
		e.applyPage(p, ev)
	case KindAsset:
		e.applyAsset(p, ev)
	case KindPreload:
		if pg := e.lookup(ev.Host, ev.Path); pg != nil {
			pg.lastFetch = ev.At
			if ev.Latency > 0 {
				pg.latency.observe(float64(ev.Latency) / float64(time.Millisecond))
			}
		}
	}
}

func (e *Engine) applyPage(p *Params, ev Event) {
	h := e.hostFor(p, ev.Host, ev.At)
	if h == nil {
		return
	}
	pg := e.pageFor(p, h, ev.Path, ev.At)
	if pg == nil {
		return
	}
	pg.views.add(ev.At, p.HalfLife, 1)
	pg.lastSeen = ev.At
	pg.status = ev.Status
	pg.html = true
	pg.cacheable = ev.Cacheable
	if ev.Latency > 0 {
		pg.latency.observe(float64(ev.Latency) / float64(time.Millisecond))
	}
	h.views.add(ev.At, p.HalfLife, 1)
	h.lastSeen = ev.At

	// Navigation path: the page the visitor came from now points here.
	if ev.Ref != "" && ev.Ref != ev.Path {
		if from := h.pages[ev.Ref]; from != nil {
			c := from.next[ev.Path]
			if c == nil {
				if len(from.next) >= p.MaxNextPerPage {
					e.evictNext(p, from, ev.At)
				}
				c = &counter{}
				from.next[ev.Path] = c
			}
			c.add(ev.At, p.HalfLife, 1)
			e.refreshPrefetch(p, from, ev.At)
		}
	}

	if ev.Client != "" {
		e.remember(ev.Client, ev.Host, ev.Path, ev.At)
	}
}

func (e *Engine) applyAsset(p *Params, ev Event) {
	h := e.hosts[ev.Host]
	if h == nil {
		return // assets never create a host: pages do
	}
	ref := ev.Ref
	// No usable referrer (Referrer-Policy stripped it, or it only
	// carried the origin): fall back to the last page this client
	// navigated to, which is what pulled the subresource in.
	if ref == "" || ref == "/" {
		if r, ok := e.recent[ev.Client]; ok && r.host == ev.Host &&
			ev.At.Sub(r.at) <= recentTTL && (ref == "" || r.path != "/") {
			ref = r.path
		}
	}
	if ref == "" {
		return
	}
	pg := h.pages[ref]
	if pg == nil {
		return // the referring page was never seen as HTML: ignore
	}
	a := pg.assets[ev.Path]
	if a == nil {
		if len(pg.assets) >= p.MaxAssetsPerPage {
			e.evictAsset(p, pg, ev.At)
		}
		a = &asset{as: ev.As}
		pg.assets[ev.Path] = a
	}
	if a.as == "" {
		a.as = ev.As
	}
	a.hits.add(ev.At, p.HalfLife, 1)
	a.lastSeen = ev.At
	e.refreshHints(p, pg, ev.At)
}

// remember records the last navigation of a client.
func (e *Engine) remember(client, h, path string, at time.Time) {
	if len(e.recent) >= maxRecent {
		// Cheap bounded cleanup: drop everything already expired, and
		// if that was not enough, drop an arbitrary slice of entries.
		for k, v := range e.recent {
			if at.Sub(v.at) > recentTTL {
				delete(e.recent, k)
			}
		}
		for k := range e.recent {
			if len(e.recent) < maxRecent {
				break
			}
			delete(e.recent, k)
		}
	}
	e.recent[client] = recentPage{host: h, path: path, at: at}
}

func (e *Engine) hostFor(p *Params, name string, at time.Time) *host {
	h := e.hosts[name]
	if h != nil {
		return h
	}
	if len(e.hosts) >= p.MaxHosts {
		// Evict the least recently seen host: a machine hosting more
		// vhosts than the cap keeps modelling the active ones.
		var oldest string
		var oldestAt time.Time
		for n, hh := range e.hosts {
			if oldest == "" || hh.lastSeen.Before(oldestAt) {
				oldest, oldestAt = n, hh.lastSeen
			}
		}
		if oldest == "" {
			return nil
		}
		delete(e.hosts, oldest)
	}
	h = &host{pages: map[string]*page{}, lastSeen: at}
	e.hosts[name] = h
	return h
}

func (e *Engine) pageFor(p *Params, h *host, path string, at time.Time) *page {
	pg := h.pages[path]
	if pg != nil {
		return pg
	}
	if len(h.pages) >= p.MaxPagesPerHost {
		e.evictPages(p, h, at)
	}
	pg = &page{assets: map[string]*asset{}, next: map[string]*counter{}, lastSeen: at}
	h.pages[path] = pg
	return pg
}

// evictPages drops the weakest tenth of the pages of a host in one
// pass, so the scan cost is amortized over many insertions.
func (e *Engine) evictPages(p *Params, h *host, now time.Time) {
	type scored struct {
		path string
		s    float64
	}
	all := make([]scored, 0, len(h.pages))
	for path, pg := range h.pages {
		all = append(all, scored{path, pg.views.value(now, p.HalfLife)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].s < all[j].s })
	drop := len(all)/10 + 1
	for i := 0; i < drop && i < len(all); i++ {
		delete(h.pages, all[i].path)
	}
}

func (e *Engine) evictAsset(p *Params, pg *page, now time.Time) {
	var weakest string
	worst := -1.0
	for u, a := range pg.assets {
		if v := a.hits.value(now, p.HalfLife); worst < 0 || v < worst {
			weakest, worst = u, v
		}
	}
	if weakest != "" {
		delete(pg.assets, weakest)
	}
}

func (e *Engine) evictNext(p *Params, pg *page, now time.Time) {
	var weakest string
	worst := -1.0
	for u, c := range pg.next {
		if v := c.value(now, p.HalfLife); worst < 0 || v < worst {
			weakest, worst = u, v
		}
	}
	if weakest != "" {
		delete(pg.next, weakest)
	}
}

func (e *Engine) lookup(hostName, path string) *page {
	h := e.hosts[hostName]
	if h == nil {
		return nil
	}
	return h.pages[path]
}

// Hints returns the resources worth announcing for a page. Lock-free
// for the caller beyond one read lock: the slice is immutable.
func (e *Engine) Hints(hostName, path string) []Hint {
	if !e.p().HintsEnabled {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	pg := e.lookup(hostName, path)
	if pg == nil {
		return nil
	}
	return pg.hints
}

// Prefetch returns the paths a visitor on this page is most likely to
// request next.
func (e *Engine) Prefetch(hostName, path string) []string {
	if !e.p().PrefetchEnabled {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	pg := e.lookup(hostName, path)
	if pg == nil {
		return nil
	}
	return pg.prefetch
}

// refreshHints recomputes the Early Hints of a page, at most once per
// RebuildInterval. Called with the write lock held.
//
// Confidence is measured against the most requested asset of the page,
// not against the page views: browsers cache subresources, so a repeat
// visitor loads the HTML without re-fetching the CSS. Comparing assets
// with each other cancels that bias out and still ranks a resource
// that only some visitors need well below the ones everybody needs.
func (e *Engine) refreshHints(p *Params, pg *page, now time.Time) {
	if now.Sub(pg.hintsAt) < p.RebuildInterval {
		return
	}
	pg.hintsAt = now
	if !p.HintsEnabled || p.HintMaxLinks == 0 {
		pg.hints = nil
		return
	}

	leader := 0.0
	for _, a := range pg.assets {
		if v := a.hits.value(now, p.HalfLife); v > leader {
			leader = v
		}
	}
	if leader <= 0 {
		pg.hints = nil
		return
	}

	out := make([]Hint, 0, p.HintMaxLinks)
	for url, a := range pg.assets {
		if a.as == "" {
			continue // unknown resource type: nothing safe to preload as
		}
		if now.Sub(a.lastSeen) > p.HintMaxAge {
			continue // stale: the page probably does not use it anymore
		}
		v := a.hits.value(now, p.HalfLife)
		if v < p.HintMinSamples {
			continue // too rare to be trusted
		}
		conf := v / leader
		if conf < p.HintMinConfidence {
			continue
		}
		out = append(out, Hint{URL: url, As: a.as, Confidence: conf, CrossOrigin: a.as == "font"})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].URL < out[j].URL
	})
	// Stylesheets and fonts block rendering: announce them first.
	sort.SliceStable(out, func(i, j int) bool { return asRank(out[i].As) < asRank(out[j].As) })
	if len(out) > p.HintMaxLinks {
		out = out[:p.HintMaxLinks]
	}
	pg.hints = out
}

func asRank(as string) int {
	switch as {
	case "style":
		return 0
	case "font":
		return 1
	case "script":
		return 2
	default:
		return 3
	}
}

// refreshPrefetch recomputes the most probable next pages.
func (e *Engine) refreshPrefetch(p *Params, pg *page, now time.Time) {
	if !p.PrefetchEnabled || p.PrefetchMaxLinks == 0 {
		pg.prefetch = nil
		return
	}
	total := 0.0
	for _, c := range pg.next {
		total += c.value(now, p.HalfLife)
	}
	if total < 3 { // not enough navigations to predict anything
		pg.prefetch = nil
		return
	}
	type cand struct {
		path string
		prob float64
	}
	var cands []cand
	for path, c := range pg.next {
		if prob := c.value(now, p.HalfLife) / total; prob >= p.PrefetchMinProb {
			cands = append(cands, cand{path, prob})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].prob != cands[j].prob {
			return cands[i].prob > cands[j].prob
		}
		return cands[i].path < cands[j].path
	})
	if len(cands) > p.PrefetchMaxLinks {
		cands = cands[:p.PrefetchMaxLinks]
	}
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.path)
	}
	pg.prefetch = out
}

// prune decays every counter and forgets what has faded below
// MinScore. This is the only place memory is actually reclaimed, and
// it is what lets rukh run for months without growing.
func (e *Engine) prune() {
	p := e.p()
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()

	for name, h := range e.hosts {
		h.views.decay(now, p.HalfLife)
		for path, pg := range h.pages {
			pg.views.decay(now, p.HalfLife)
			for u, a := range pg.assets {
				a.hits.decay(now, p.HalfLife)
				if a.hits.val < p.MinScore || now.Sub(a.lastSeen) > p.HintMaxAge {
					delete(pg.assets, u)
				}
			}
			for u, c := range pg.next {
				c.decay(now, p.HalfLife)
				if c.val < p.MinScore {
					delete(pg.next, u)
				}
			}
			if pg.views.val < p.MinScore && len(pg.assets) == 0 && len(pg.next) == 0 {
				delete(h.pages, path)
				continue
			}
			// Keep the derived views in sync with the decayed model.
			e.refreshHintsForce(p, pg, now)
			e.refreshPrefetch(p, pg, now)
		}
		if len(h.pages) == 0 && h.views.val < p.MinScore {
			delete(e.hosts, name)
		}
	}
	for k, v := range e.recent {
		if now.Sub(v.at) > recentTTL {
			delete(e.recent, k)
		}
	}
}

func (e *Engine) refreshHintsForce(p *Params, pg *page, now time.Time) {
	pg.hintsAt = time.Time{}
	e.refreshHints(p, pg, now)
}

// RebuildPlan ranks the pages worth warming. Priority goes to pages
// that are requested often, were requested recently, and are slow at
// the origin: exactly the ones whose cache miss hurts most.
func (e *Engine) RebuildPlan() {
	p := e.p()
	now := e.now()
	e.mu.RLock()
	type entry struct {
		t     Target
		views float64
	}
	var all []entry
	maxViews, maxLat := 0.0, 0.0
	for name, h := range e.hosts {
		for path, pg := range h.pages {
			if !pg.html || !pg.cacheable || pg.status != 200 {
				continue
			}
			v := pg.views.value(now, p.HalfLife)
			if v < p.MinScore {
				continue
			}
			if v > maxViews {
				maxViews = v
			}
			if pg.latency.val > maxLat {
				maxLat = pg.latency.val
			}
			all = append(all, entry{Target{Host: name, Path: path}, v})
		}
	}
	// Second pass: normalize and score.
	for i := range all {
		pg := e.hosts[all[i].t.Host].pages[all[i].t.Path]
		freq := 0.0
		if maxViews > 0 {
			freq = all[i].views / maxViews
		}
		slow := 0.0
		if maxLat > 0 {
			slow = pg.latency.val / maxLat
		}
		recency := decayFactor(now.Sub(pg.lastSeen), p.HalfLife)
		all[i].t.Score = 0.6*freq + 0.25*recency + 0.15*slow
	}
	e.mu.RUnlock()

	sort.Slice(all, func(i, j int) bool { return all[i].t.Score > all[j].t.Score })
	if len(all) > p.PreloadMaxPages {
		all = all[:p.PreloadMaxPages]
	}
	plan := make([]Target, 0, len(all))
	for _, a := range all {
		t := a.t
		// The hotter the page, the more often it is refreshed; a page
		// that barely registers is warmed at the slowest cadence.
		span := p.PreloadMaxRefresh - p.PreloadMinRefresh
		t.Interval = p.PreloadMaxRefresh - time.Duration(t.Score*float64(span))
		if t.Interval < p.PreloadMinRefresh {
			t.Interval = p.PreloadMinRefresh
		}
		plan = append(plan, t)
	}
	e.plan.Store(&plan)
}

// Plan returns the current ranking of preload targets.
func (e *Engine) Plan() []Target { return *e.plan.Load() }

// Due returns the plan entries that are worth fetching now: a page
// touched by a real visitor (or by a previous warm-up) more recently
// than its refresh interval is already warm and is skipped, which is
// how the preloader avoids useless requests.
func (e *Engine) Due(now time.Time, max int) []Target {
	plan := e.Plan()
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Target, 0, max)
	for _, t := range plan {
		pg := e.lookup(t.Host, t.Path)
		if pg == nil {
			continue
		}
		last := pg.lastSeen
		if pg.lastFetch.After(last) {
			last = pg.lastFetch
		}
		if now.Sub(last) < t.Interval {
			continue
		}
		out = append(out, t)
		if len(out) >= max {
			break
		}
	}
	return out
}

// Stats is the summary the status socket reports.
type Stats struct {
	Hosts       int       `json:"hosts"`
	Pages       int       `json:"pages"`
	Assets      int       `json:"assets"`
	Transitions int       `json:"transitions"`
	HintedPages int       `json:"hinted_pages"`
	PlanSize    int       `json:"plan_size"`
	Events      int64     `json:"events"`
	Dropped     int64     `json:"dropped"`
	TopPages    []TopPage `json:"top_pages,omitempty"`
}

// TopPage is one entry of the "what is hot right now" list.
type TopPage struct {
	Host      string  `json:"host"`
	Path      string  `json:"path"`
	Views     float64 `json:"views"`
	Assets    int     `json:"assets"`
	Hints     int     `json:"hints"`
	LatencyMs float64 `json:"latency_ms"`
}

// Stats returns a snapshot of the model size and its hottest pages.
func (e *Engine) Stats(topN int) Stats {
	p := e.p()
	now := e.now()
	e.mu.RLock()
	defer e.mu.RUnlock()

	st := Stats{
		Hosts:    len(e.hosts),
		Events:   e.events.Load(),
		Dropped:  e.dropped.Load(),
		PlanSize: len(*e.plan.Load()),
	}
	var top []TopPage
	for name, h := range e.hosts {
		for path, pg := range h.pages {
			st.Pages++
			st.Assets += len(pg.assets)
			st.Transitions += len(pg.next)
			if len(pg.hints) > 0 {
				st.HintedPages++
			}
			if topN > 0 {
				top = append(top, TopPage{
					Host: name, Path: path,
					Views:     pg.views.value(now, p.HalfLife),
					Assets:    len(pg.assets),
					Hints:     len(pg.hints),
					LatencyMs: pg.latency.val,
				})
			}
		}
	}
	sort.Slice(top, func(i, j int) bool { return top[i].Views > top[j].Views })
	if len(top) > topN {
		top = top[:topN]
	}
	st.TopPages = top
	return st
}

// TrackingParams are query parameters that identify a campaign or a
// click, not a page: they are stripped so the same page seen through
// ten campaigns stays one entry in the model.
var trackingParams = map[string]bool{
	"utm_source": true, "utm_medium": true, "utm_campaign": true,
	"utm_term": true, "utm_content": true, "utm_id": true,
	"gclid": true, "gbraid": true, "wbraid": true, "fbclid": true,
	"msclkid": true, "dclid": true, "yclid": true, "igshid": true,
	"mc_cid": true, "mc_eid": true, "_ga": true, "_gl": true,
	"ref": true, "referrer": true, "source": true,
}

// NormalizePath cleans a request target for use as a model key: query
// parameters used for tracking are dropped, the remaining ones are
// sorted so their order does not create duplicates, and the result is
// capped in length.
func NormalizePath(raw string) string {
	if raw == "" {
		return "/"
	}
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = raw[:i]
	}
	path, query, hasQuery := strings.Cut(raw, "?")
	if path == "" {
		path = "/"
	}
	if !hasQuery || query == "" {
		return cap512(path)
	}
	parts := strings.Split(query, "&")
	kept := parts[:0]
	for _, kv := range parts {
		k, _, _ := strings.Cut(kv, "=")
		if k == "" || trackingParams[strings.ToLower(k)] {
			continue
		}
		kept = append(kept, kv)
	}
	if len(kept) == 0 {
		return cap512(path)
	}
	sort.Strings(kept)
	return cap512(path + "?" + strings.Join(kept, "&"))
}

func cap512(s string) string {
	if len(s) > 512 {
		return s[:512]
	}
	return s
}
