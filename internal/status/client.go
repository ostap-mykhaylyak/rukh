package status

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
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

// Run implements the --status / --status-json / --watch CLI and
// returns the process exit code (Nagios convention).
func Run(version, sock, cfgPath string, jsonOut bool, watch time.Duration) int {
	if watch <= 0 {
		snap := fetch(version, sock, cfgPath)
		print(snap, jsonOut, nil, 0)
		return ExitCode(snap.Status)
	}

	// Live mode: redraw in a loop like top (or one JSON line per tick
	// with --status-json, suitable for piping to a collector).
	var prev *Snapshot
	var prevAt time.Time
	for {
		snap := fetch(version, sock, cfgPath)
		if !jsonOut {
			fmt.Print("\033[2J\033[H") // clear screen, home cursor
		}
		print(snap, jsonOut, prev, time.Since(prevAt))
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

func print(snap *Snapshot, jsonOut bool, prev *Snapshot, elapsed time.Duration) {
	if jsonOut {
		json.NewEncoder(os.Stdout).Encode(snap)
		return
	}
	fmt.Println(summaryLine(snap))
	if prev == nil {
		return
	}

	// watch mode: multi-line view with rates computed between ticks.
	fmt.Printf("config:   %s (%s)\n", boolWord(snap.Config.Valid, "valid", "INVALID"), snap.Config.Path)
	if n := snap.Nginx; n != nil {
		fmt.Printf("nginx:    %d site(s), backend %s%s\n", n.Sites, n.Backend, autoWord(n.BackendAut))
		if len(n.Hosts) > 0 {
			fmt.Printf("hosts:    %s\n", strings.Join(n.Hosts, " "))
		}
		for _, c := range n.Certs {
			if c.Error != "" {
				fmt.Printf("cert:     %s ERROR %s\n", c.CertFile, c.Error)
				continue
			}
			fmt.Printf("cert:     %-40s %s (%.0f days)\n", c.Subject, c.NotAfter.Format("2006-01-02"),
				time.Until(c.NotAfter).Hours()/24)
		}
	}
	if l := snap.Live; l != nil {
		var reqRate, errRate float64
		if prev.Live != nil && elapsed > 0 {
			reqRate = float64(l.RequestsTotal-prev.Live.RequestsTotal) / elapsed.Seconds()
			errRate = float64(l.ErrorsTotal-prev.Live.ErrorsTotal) / elapsed.Seconds()
		}
		fmt.Printf("requests: %d total, %d in-flight, %.1f req/s, %.1f err/s\n",
			l.RequestsTotal, l.RequestsInFlight, reqRate, errRate)
		fmt.Printf("latency:  p50 %.0fms, p95 %.0fms, p99 %.0fms\n",
			l.P50LatencyMs, l.P95LatencyMs, l.P99LatencyMs)
		fmt.Printf("traffic:  %d page(s), %d asset(s), %s out\n",
			l.PageViews, l.AssetHits, humanBytes(l.BytesOutTotal))
		fmt.Printf("hints:    %d response(s), %d link(s), %d prefetch link(s)\n",
			l.HintsSent, l.HintLinks, l.PrefetchLinks)
	}
	if lr := snap.Learn; lr != nil {
		fmt.Printf("model:    %d host(s), %d page(s), %d asset(s), %d transition(s), %d hinted, %d dropped\n",
			lr.Hosts, lr.Pages, lr.Assets, lr.Transitions, lr.HintedPages, lr.Dropped)
		for _, t := range lr.TopPages {
			fmt.Printf("  %-45s %7.1f views  %2d assets  %2d hints  %6.0fms\n",
				trunc(t.Host+t.Path, 45), t.Views, t.Assets, t.Hints, t.LatencyMs)
		}
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
		fmt.Printf("preload:  %s\n", state)
		if len(p.Next) > 0 {
			fmt.Printf("  next:   %s\n", strings.Join(p.Next, " "))
		}
	}
	parts := make([]string, 0, len(snap.Checks))
	for _, c := range snap.Checks {
		parts = append(parts, c.Name+"="+c.Status)
	}
	fmt.Printf("checks:   %s\n", strings.Join(parts, " "))
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

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
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
