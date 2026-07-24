// Package metrics keeps live in-memory counters, updated on the hot
// path at near-zero cost with sync/atomic. Snapshot() is the single
// source of truth consumed by the status socket.
//
// There is no metrics export: rukh is an optimizer, not a monitoring
// system. These counters exist so `rukh --status` can say what the
// daemon is doing right now.
package metrics

import (
	"sort"
	"sync/atomic"
	"time"
)

// latencyBoundsMs are the upper edges (milliseconds) of the fixed
// request-latency histogram buckets. A request is counted in the first
// bucket whose edge is >= its latency; anything slower lands in the
// overflow bucket.
var latencyBoundsMs = []float64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

// histogram is a lock-free fixed-bucket latency histogram.
type histogram struct {
	buckets []atomic.Int64 // len(latencyBoundsMs)+1 (last = overflow)
}

func newHistogram() *histogram {
	return &histogram{buckets: make([]atomic.Int64, len(latencyBoundsMs)+1)}
}

func (h *histogram) observe(d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	h.buckets[sort.SearchFloat64s(latencyBoundsMs, ms)].Add(1)
}

// percentiles returns, for each p in [0,1], the upper edge (ms) of the
// bucket where the cumulative count crosses p. Zero when no data.
func (h *histogram) percentiles(ps ...float64) []float64 {
	counts := make([]int64, len(h.buckets))
	var total int64
	for i := range h.buckets {
		counts[i] = h.buckets[i].Load()
		total += counts[i]
	}
	out := make([]float64, len(ps))
	if total == 0 {
		return out
	}
	for j, p := range ps {
		target := int64(float64(total) * p)
		if target < 1 {
			target = 1
		}
		var cum int64
		for i, c := range counts {
			cum += c
			if cum >= target {
				if i < len(latencyBoundsMs) {
					out[j] = latencyBoundsMs[i]
				} else {
					out[j] = latencyBoundsMs[len(latencyBoundsMs)-1] // overflow: report the max edge
				}
				break
			}
		}
	}
	return out
}

// Metrics is the daemon-wide collector.
type Metrics struct {
	start time.Time
	lat   *histogram

	requestsTotal atomic.Int64
	errorsTotal   atomic.Int64
	bytesOut      atomic.Int64
	inFlight      atomic.Int64
	pageViews     atomic.Int64
	assetHits     atomic.Int64

	hintsSent     atomic.Int64
	hintLinks     atomic.Int64
	prefetchLinks atomic.Int64

	learnEvents  atomic.Int64
	learnDropped atomic.Int64

	preloadRequests atomic.Int64
	preloadErrors   atomic.Int64
	preloadSkipped  atomic.Int64
}

// New returns a Metrics anchored at the current time.
func New() *Metrics { return &Metrics{start: time.Now(), lat: newHistogram()} }

// RequestStart records an incoming request and returns the completion
// callback, to be deferred by the handler. The callback measures the
// request latency itself, so callers need not time anything.
func (m *Metrics) RequestStart() func(bytes int64, failed bool) {
	m.requestsTotal.Add(1)
	m.inFlight.Add(1)
	t0 := time.Now()
	return func(bytes int64, failed bool) {
		m.inFlight.Add(-1)
		m.bytesOut.Add(bytes)
		if failed {
			m.errorsTotal.Add(1)
		}
		m.lat.observe(time.Since(t0))
	}
}

// Traffic classification counters.
func (m *Metrics) PageView() { m.pageViews.Add(1) }
func (m *Metrics) AssetHit() { m.assetHits.Add(1) }

// Hint counters.
func (m *Metrics) HintsSent(links int) {
	m.hintsSent.Add(1)
	m.hintLinks.Add(int64(links))
}

// PrefetchLinks counts Link rel=prefetch entries advertised.
func (m *Metrics) PrefetchLinks(n int) { m.prefetchLinks.Add(int64(n)) }

// Learning pipeline counters.
func (m *Metrics) LearnEvent()   { m.learnEvents.Add(1) }
func (m *Metrics) LearnDropped() { m.learnDropped.Add(1) }

// Preloader counters.
func (m *Metrics) PreloadRequest()      { m.preloadRequests.Add(1) }
func (m *Metrics) PreloadError()        { m.preloadErrors.Add(1) }
func (m *Metrics) PreloadSkipped(n int) { m.preloadSkipped.Add(int64(n)) }

// Snapshot is a coherent, JSON-serializable view of the live state.
// Field names are stable across versions.
type Snapshot struct {
	UptimeSeconds    float64   `json:"uptime_seconds"`
	RequestsTotal    int64     `json:"requests_total"`
	RequestsInFlight int64     `json:"requests_in_flight"`
	ErrorsTotal      int64     `json:"errors_total"`
	BytesOutTotal    int64     `json:"bytes_out_total"`
	PageViews        int64     `json:"page_views"`
	AssetHits        int64     `json:"asset_hits"`
	HintsSent        int64     `json:"hints_sent"`
	HintLinks        int64     `json:"hint_links"`
	PrefetchLinks    int64     `json:"prefetch_links"`
	LearnEvents      int64     `json:"learn_events"`
	LearnDropped     int64     `json:"learn_dropped"`
	PreloadRequests  int64     `json:"preload_requests"`
	PreloadErrors    int64     `json:"preload_errors"`
	PreloadSkipped   int64     `json:"preload_skipped"`
	P50LatencyMs     float64   `json:"p50_latency_ms"`
	P95LatencyMs     float64   `json:"p95_latency_ms"`
	P99LatencyMs     float64   `json:"p99_latency_ms"`
	Timestamp        time.Time `json:"timestamp"`
}

// Snapshot reads all counters atomically.
func (m *Metrics) Snapshot() Snapshot {
	lp := m.lat.percentiles(0.50, 0.95, 0.99)
	return Snapshot{
		UptimeSeconds:    time.Since(m.start).Seconds(),
		RequestsTotal:    m.requestsTotal.Load(),
		RequestsInFlight: m.inFlight.Load(),
		ErrorsTotal:      m.errorsTotal.Load(),
		BytesOutTotal:    m.bytesOut.Load(),
		PageViews:        m.pageViews.Load(),
		AssetHits:        m.assetHits.Load(),
		HintsSent:        m.hintsSent.Load(),
		HintLinks:        m.hintLinks.Load(),
		PrefetchLinks:    m.prefetchLinks.Load(),
		LearnEvents:      m.learnEvents.Load(),
		LearnDropped:     m.learnDropped.Load(),
		PreloadRequests:  m.preloadRequests.Load(),
		PreloadErrors:    m.preloadErrors.Load(),
		PreloadSkipped:   m.preloadSkipped.Load(),
		P50LatencyMs:     lp[0],
		P95LatencyMs:     lp[1],
		P99LatencyMs:     lp[2],
		Timestamp:        time.Now().UTC(),
	}
}
