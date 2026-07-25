package status

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ostap-mykhaylyak/rukh/internal/learn"
	"github.com/ostap-mykhaylyak/rukh/internal/metrics"
	"github.com/ostap-mykhaylyak/rukh/internal/preload"
)

// longPath is the kind of URL a WooCommerce catalogue produces: the
// watch view must show it whole, because an operator reads it to know
// which page is hot, and copies it to test it.
const longPath = "/categoria-prodotto/senza-categoria/prodotto/ammortizzatore-anteriore-vespa-px-125-150-200-completo"

func sampleSnapshot() *Snapshot {
	live := metrics.Snapshot{RequestsTotal: 256, PageViews: 80, AssetHits: 85}
	st := learn.Stats{
		Hosts: 1, Pages: 46, Assets: 78, Transitions: 18,
		TopPages: []learn.TopPage{
			{Host: "petralito.example", Path: "/", Views: 33, LatencyMs: 10},
			{Host: "petralito.example", Path: longPath, Views: 1.9, Assets: 3, Hints: 2, LatencyMs: 371},
		},
		SlowPages: []learn.TopPage{
			{Host: "petralito.example", Path: longPath, Views: 1.9, Assets: 3, Hints: 2, LatencyMs: 371},
			{Host: "petralito.example", Path: "/", Views: 33, LatencyMs: 10},
		},
	}
	ps := preload.Stats{Enabled: true, PlanSize: 33, Next: []string{
		"petralito.example" + longPath,
		"petralito.example/carrello",
	}}
	return &Snapshot{
		Status:  OK,
		Version: "test",
		Service: ServiceInfo{Active: true, PID: 1234, UptimeSeconds: 60},
		Config:  ConfigInfo{Path: "/etc/rukh/config.yaml", Valid: true},
		Nginx: &NginxSection{
			Sites:   1,
			Hosts:   []string{"petralito.example", "www.petralito.example"},
			Backend: "127.0.0.1:8080",
		},
		Learn:   &st,
		Preload: &ps,
		Live:    &live,
		Checks:  []Check{{"config", OK, "loaded and valid"}},
	}
}

func TestWatchViewShowsWholeAddresses(t *testing.T) {
	var buf bytes.Buffer
	// prev != nil selects the multi-line watch view.
	report(&buf, sampleSnapshot(), false, sampleSnapshot(), 2*time.Second)
	out := buf.String()

	for _, want := range []string{
		"petralito.example" + longPath, // top page, in full
		"www.petralito.example",        // every discovered host
		"petralito.example/carrello",   // preload targets
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing from the watch view: %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "…") {
		t.Errorf("something was truncated:\n%s", out)
	}
	// The address is the last column, so the numbers stay aligned.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, longPath) && !strings.HasSuffix(line, longPath) {
			t.Errorf("the address must end the line: %q", line)
		}
	}
}

func TestWatchViewListsBusiestAndSlowestPages(t *testing.T) {
	var buf bytes.Buffer
	report(&buf, sampleSnapshot(), false, sampleSnapshot(), 2*time.Second)
	out := buf.String()

	busiest := strings.Index(out, "busiest:")
	slowest := strings.Index(out, "slowest:")
	if busiest < 0 || slowest < 0 {
		t.Fatalf("both rankings must be labelled:\n%s", out)
	}
	if slowest < busiest {
		t.Error("the busiest pages come first, the slowest after")
	}
	// The slow list is ordered by the origin latency, worst first.
	slowBlock := out[slowest:]
	if i, j := strings.Index(slowBlock, "371ms"), strings.Index(slowBlock, "10ms"); i < 0 || j < 0 || i > j {
		t.Errorf("the slowest page must head the list:\n%s", slowBlock)
	}
}

func TestSingleLineViewStaysOneLine(t *testing.T) {
	var buf bytes.Buffer
	report(&buf, sampleSnapshot(), false, nil, 0)
	if n := strings.Count(strings.TrimSpace(buf.String()), "\n"); n != 0 {
		t.Fatalf("plain `rukh status` must print one line, got:\n%s", buf.String())
	}
}

func TestJSONOutputIsTheWholeSnapshot(t *testing.T) {
	var buf bytes.Buffer
	report(&buf, sampleSnapshot(), true, nil, 0)
	out := buf.String()
	for _, want := range []string{`"status":"ok"`, `"top_pages"`, longPath} {
		if !strings.Contains(out, want) {
			t.Errorf("missing from --json output: %q", want)
		}
	}
}
