package status

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/ostap-mykhaylyak/rukh/internal/learn"
)

// Query connects to the daemon's status socket and returns the
// snapshot it serves. Read-only, no side effects, safe to run in
// parallel with the daemon.
func Query(sock string, timeout time.Duration) (*Snapshot, error) {
	conn, err := net.DialTimeout("unix", sock, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(timeout))
	var snap Snapshot
	if err := json.NewDecoder(conn).Decode(&snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// Run implements the `rukh status` CLI (--json, --watch) and
// returns the process exit code (Nagios convention).
func Run(version, sock, cfgPath string, jsonOut bool, watch time.Duration) int {
	if watch <= 0 {
		snap := fetch(version, sock, cfgPath)
		report(os.Stdout, snap, jsonOut, nil, 0)
		return ExitCode(snap.Status)
	}

	// Live mode: redraw in a loop like top (or one JSON line per tick
	// with --json, suitable for piping to a collector).
	var prev *Snapshot
	var prevAt time.Time
	for {
		snap := fetch(version, sock, cfgPath)
		if !jsonOut {
			fmt.Print("\033[2J\033[H") // clear screen, home cursor
		}
		report(os.Stdout, snap, jsonOut, prev, time.Since(prevAt))
		prev, prevAt = snap, time.Now()
		time.Sleep(watch)
	}
}

func fetch(version, sock, cfgPath string) *Snapshot {
	snap, err := Query(sock, 2*time.Second)
	if err != nil {
		return notRunning(version, cfgPath)
	}
	return snap
}

// report renders a snapshot. Variable-length values (hostnames, paths,
// certificate subjects) always come last on their line, so nothing has
// to be truncated: a long product URL stays readable and copyable.
func report(w io.Writer, snap *Snapshot, jsonOut bool, prev *Snapshot, elapsed time.Duration) {
	if jsonOut {
		json.NewEncoder(w).Encode(snap)
		return
	}
	fmt.Fprintln(w, summaryLine(snap))
	if prev == nil {
		return
	}

	// watch mode: multi-line view with rates computed between ticks.
	fmt.Fprintf(w, "config:   %s (%s)\n", boolWord(snap.Config.Valid, "valid", "INVALID"), snap.Config.Path)
	if n := snap.Nginx; n != nil {
		fmt.Fprintf(w, "nginx:    %d site(s), backend %s%s\n", n.Sites, n.Backend, autoWord(n.BackendAut))
		for i, h := range n.Hosts {
			fmt.Fprintf(w, "%-10s%s\n", label(i, "hosts:"), h)
		}
		for _, c := range n.Certs {
			if c.Error != "" {
				fmt.Fprintf(w, "cert:     ERROR %s (%s)\n", c.Error, c.CertFile)
				continue
			}
			fmt.Fprintf(w, "cert:     until %s, %.0f days left  %s\n",
				c.NotAfter.Format("2006-01-02"), time.Until(c.NotAfter).Hours()/24, c.Subject)
		}
	}
	if h := snap.HTTP3; h != nil && h.Enabled {
		fmt.Fprintf(w, "http3:    %s\n",
			boolWord(h.Active, "listening (QUIC/UDP)", "NOT listening: UDP bind failed"))
	}
	if l := snap.Live; l != nil {
		var reqRate, errRate float64
		if prev.Live != nil && elapsed > 0 {
			reqRate = float64(l.RequestsTotal-prev.Live.RequestsTotal) / elapsed.Seconds()
			errRate = float64(l.ErrorsTotal-prev.Live.ErrorsTotal) / elapsed.Seconds()
		}
		fmt.Fprintf(w, "requests: %d total, %d in-flight, %.1f req/s, %.1f err/s\n",
			l.RequestsTotal, l.RequestsInFlight, reqRate, errRate)
		fmt.Fprintf(w, "latency:  p50 %.0fms, p95 %.0fms, p99 %.0fms\n",
			l.P50LatencyMs, l.P95LatencyMs, l.P99LatencyMs)
		fmt.Fprintf(w, "traffic:  %d page(s), %d asset(s), %s out\n",
			l.PageViews, l.AssetHits, humanBytes(l.BytesOutTotal))
		fmt.Fprintf(w, "hints:    %d response(s), %d link(s), %d prefetch link(s)\n",
			l.HintsSent, l.HintLinks, l.PrefetchLinks)
	}
	if lr := snap.Learn; lr != nil {
		fmt.Fprintf(w, "model:    %d host(s), %d page(s), %d asset(s), %d transition(s), %d hinted, %d dropped\n",
			lr.Hosts, lr.Pages, lr.Assets, lr.Transitions, lr.HintedPages, lr.Dropped)
		pageList(w, "busiest:", lr.TopPages)
		pageList(w, "slowest:", lr.SlowPages)
	}
	if p := snap.Preload; p != nil {
		state := "disabled"
		if p.Enabled {
			state = fmt.Sprintf("%d in plan, %d warmed last round, %d total, %d errors",
				p.PlanSize, p.LastCount, p.Requests, p.Errors)
			if p.Backoff {
				state += " (backing off)"
			}
		}
		fmt.Fprintf(w, "preload:  %s\n", state)
		for i, n := range p.Next {
			fmt.Fprintf(w, "%-10s%s\n", label(i, "  next:"), n)
		}
	}
	parts := make([]string, 0, len(snap.Checks))
	for _, c := range snap.Checks {
		parts = append(parts, c.Name+"="+c.Status)
	}
	fmt.Fprintf(w, "checks:   %s\n", strings.Join(parts, " "))
}

// pageList renders one ranking of pages, address last so it is never
// cut. Latency is what the origin took to answer, not what the
// visitor's connection added.
func pageList(w io.Writer, name string, pages []learn.TopPage) {
	for i, t := range pages {
		fmt.Fprintf(w, "%-10s%8.1f views %3d assets %3d hints %7.0fms  %s\n",
			label(i, name), t.Views, t.Assets, t.Hints, t.LatencyMs, t.Host+t.Path)
	}
}

// label prints a column heading on the first row of a list only, so a
// multi-line list reads as one block.
func label(i int, name string) string {
	if i == 0 {
		return name
	}
	return ""
}

func summaryLine(snap *Snapshot) string {
	label := map[string]string{OK: "OK", Warn: "WARNING", Crit: "CRITICAL"}[snap.Status]
	if label == "" {
		label = "UNKNOWN"
	}
	if !snap.Service.Active {
		detail := "config on disk: absent"
		for _, c := range snap.Checks {
			if c.Name == "config_on_disk" {
				switch c.Status {
				case OK:
					detail = "config on disk: valid"
				case Crit:
					detail = "config on disk: invalid"
				}
			}
		}
		return fmt.Sprintf("rukh %s - service not running (%s)", label, detail)
	}
	uptime := time.Duration(snap.Service.UptimeSeconds * float64(time.Second)).Round(time.Second)
	line := fmt.Sprintf("rukh %s - active, pid %d, uptime %s, config %s",
		label, snap.Service.PID, uptime, boolWord(snap.Config.Valid && snap.Config.Error == "", "valid", "reload pending"))
	if n := snap.Nginx; n != nil {
		line += fmt.Sprintf(", %d site(s) -> %s", n.Sites, n.Backend)
	}
	if l := snap.Learn; l != nil {
		line += fmt.Sprintf(", %d page(s) learned", l.Pages)
	}
	if l := snap.Live; l != nil {
		line += fmt.Sprintf(", %d requests, %d hinted", l.RequestsTotal, l.HintsSent)
	}
	// The worst check is the reason for the aggregate status: show it.
	for _, c := range snap.Checks {
		if c.Status == snap.Status && c.Status != OK {
			line += " [" + c.Name + ": " + c.Detail + "]"
			break
		}
	}
	return line
}

func boolWord(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

func autoWord(auto bool) string {
	if auto {
		return " (auto-detected)"
	}
	return ""
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
