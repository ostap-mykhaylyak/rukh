// rukh - a learning reverse proxy that sits in front of nginx.
//
// Command convention: the service verbs are bare words (start, stop,
// reload, restart, status), everything else takes a leading double
// dash (--init, --purge, --check-config, --version).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ostap-mykhaylyak/rukh/internal/bootstrap"
	"github.com/ostap-mykhaylyak/rukh/internal/certs"
	"github.com/ostap-mykhaylyak/rukh/internal/config"
	"github.com/ostap-mykhaylyak/rukh/internal/hints"
	"github.com/ostap-mykhaylyak/rukh/internal/learn"
	"github.com/ostap-mykhaylyak/rukh/internal/logging"
	"github.com/ostap-mykhaylyak/rukh/internal/metrics"
	"github.com/ostap-mykhaylyak/rukh/internal/nginx"
	"github.com/ostap-mykhaylyak/rukh/internal/paths"
	"github.com/ostap-mykhaylyak/rukh/internal/preload"
	"github.com/ostap-mykhaylyak/rukh/internal/proc"
	"github.com/ostap-mykhaylyak/rukh/internal/proxy"
	"github.com/ostap-mykhaylyak/rukh/internal/status"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	args := os.Args[2:]
	cmd, err := normalizeCommand(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "rukh:", err)
		fmt.Fprintln(os.Stderr)
		usage(os.Stderr)
		os.Exit(2)
	}

	switch cmd {
	case "start":
		fs := flag.NewFlagSet("start", flag.ExitOnError)
		cfgPath := fs.String("config", paths.ConfigFile, "config file")
		pidfile := fs.String("pidfile", paths.Pidfile, "pidfile path")
		sock := fs.String("socket", paths.Socket, "status socket path")
		fs.Parse(args)
		fatalIf(runDaemon(*cfgPath, *pidfile, *sock))

	case "stop":
		fs := flag.NewFlagSet("stop", flag.ExitOnError)
		pidfile := fs.String("pidfile", paths.Pidfile, "pidfile path")
		fs.Parse(args)
		fatalIf(proc.Stop(*pidfile))

	case "reload":
		// SIGHUP: re-read the configuration and reopen the log files.
		fs := flag.NewFlagSet("reload", flag.ExitOnError)
		pidfile := fs.String("pidfile", paths.Pidfile, "pidfile path")
		fs.Parse(args)
		fatalIf(proc.Reload(*pidfile))

	case "restart":
		// Stop the running daemon, wait for it to release the listening
		// sockets, then become the new foreground daemon. Under systemd
		// use `systemctl restart rukh` instead; this is for running the
		// service by hand.
		fs := flag.NewFlagSet("restart", flag.ExitOnError)
		cfgPath := fs.String("config", paths.ConfigFile, "config file")
		pidfile := fs.String("pidfile", paths.Pidfile, "pidfile path")
		sock := fs.String("socket", paths.Socket, "status socket path")
		fs.Parse(args)
		fatalIf(proc.StopAndWait(*pidfile, 30*time.Second))
		fatalIf(runDaemon(*cfgPath, *pidfile, *sock))

	case "status":
		fs := flag.NewFlagSet("status", flag.ExitOnError)
		cfgPath := fs.String("config", paths.ConfigFile, "config file")
		sock := fs.String("socket", paths.Socket, "status socket path")
		jsonOut := fs.Bool("json", false, "machine-readable output")
		watch := fs.Duration("watch", 0, "refresh every interval (e.g. 2s), like top")
		fs.Parse(args)
		os.Exit(status.Run(version, *sock, *cfgPath, *jsonOut, *watch))

	case "init":
		fatalIf(bootstrap.Init(version, os.Stdout))

	case "purge":
		fs := flag.NewFlagSet("purge", flag.ExitOnError)
		assumeYes := fs.Bool("yes", false, "skip the confirmation prompt")
		fs.Parse(args)
		fatalIf(bootstrap.Purge(*assumeYes, os.Stdin, os.Stdout))

	case "check-config":
		fs := flag.NewFlagSet("check-config", flag.ExitOnError)
		cfgPath := fs.String("config", paths.ConfigFile, "config file")
		fs.Parse(args)
		fatalIf(checkConfig(*cfgPath))

	case "version":
		fmt.Println("rukh", version)

	default:
		// Unreachable: normalizeCommand rejects anything not handled
		// above. Kept as a guard against a new case being added to one
		// table but not to the switch.
		fmt.Fprintf(os.Stderr, "rukh: unhandled command %q\n", cmd)
		os.Exit(2)
	}
}

// serviceVerbs are driven as bare words, the way an operator and
// systemd manage a service.
var serviceVerbs = map[string]bool{
	"start": true, "stop": true, "reload": true, "restart": true, "status": true,
}

// flagCommands are the lifecycle and diagnostic actions. They take a
// leading --, so they can never be confused with a service verb.
var flagCommands = map[string]bool{
	"init": true, "purge": true, "check-config": true, "version": true,
}

// normalizeCommand maps the word the user typed to its internal name,
// enforcing the convention and explaining the two common mistakes: a
// -- on a service verb, or a missing -- on everything else.
func normalizeCommand(cmd string) (string, error) {
	if bare, ok := strings.CutPrefix(cmd, "--"); ok {
		switch {
		case flagCommands[bare]:
			return bare, nil
		case serviceVerbs[bare]:
			return "", fmt.Errorf("service commands take no leading --: use %q, not %q", bare, cmd)
		default:
			return "", fmt.Errorf("unknown command %q", cmd)
		}
	}
	switch {
	case serviceVerbs[cmd]:
		return cmd, nil
	case flagCommands[cmd]:
		return "", fmt.Errorf("this command takes a leading --: use %q, not %q", "--"+cmd, cmd)
	default:
		return "", fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `rukh - learning reverse proxy in front of nginx

Service (bare verbs, no dashes):
  start          run the daemon in the foreground (what systemd does)
  stop           signal the running daemon to shut down
  reload         re-read the configuration and reopen the log files
  restart        stop the running daemon, then start it again
  status         query the running daemon and print what it is doing
                 (--json, --watch 2s)

Everything else (leading -- required):
  --init         install layout, binary, systemd unit and logrotate policy
  --purge        remove config, logs and the binary (asks for confirmation)
  --check-config parse the configuration and the nginx setup, then exit
  --version      print the version and exit

Common flags: --config <file>, --socket <path>, --pidfile <path>

Exit codes of 'status' follow the Nagios convention:
  0 OK, 1 WARNING, 2 CRITICAL, 3 UNKNOWN
`)
}

// checkConfig parses the configuration and the nginx setup and reports
// what rukh would do, without touching anything.
func checkConfig(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	for _, w := range cfg.Warnings {
		fmt.Println("warning:", w)
	}
	fmt.Printf("%s: config OK\n", cfgPath)

	ncfg, err := nginx.Parse(cfg.Nginx.Config)
	if err != nil {
		return fmt.Errorf("nginx: %w", err)
	}
	for _, w := range ncfg.Warnings {
		fmt.Println("nginx warning:", w)
	}
	fmt.Printf("nginx: %s, %d file(s), %d server block(s)\n",
		cfg.Nginx.Config, len(ncfg.Files), len(ncfg.Sites))
	for _, s := range ncfg.Sites {
		names := strings.Join(s.Names, " ")
		if names == "" {
			names = "(default server)"
		}
		fmt.Printf("  %-40s ssl=%-5v %s\n", names, s.SSL, s.CertFile)
	}
	manual := hints.NewStore(cfg.Hints.Dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manual.LoadAll()
	if files := manual.Snapshot(); len(files) > 0 {
		fmt.Printf("hints: %s\n", cfg.Hints.Dir)
		for _, f := range files {
			if f.Error != "" {
				fmt.Printf("  %-32s ERROR %s\n", f.File, f.Error)
				continue
			}
			fmt.Printf("  %-32s %d resource(s) for %s\n", f.File, f.Entries, strings.Join(f.Hosts, " "))
			for _, w := range f.Warnings {
				fmt.Println("    skipped:", w)
			}
		}
	}

	taken := []string{}
	if cfg.Server.HTTP != "" {
		taken = append(taken, cfg.Server.HTTP)
	}
	if cfg.Server.HTTPS != "" {
		taken = append(taken, cfg.Server.HTTPS)
	}
	if cfg.Backend.Address != "" {
		fmt.Printf("backend: %s (from config)\n", cfg.Backend.Address)
	} else if b := ncfg.Backends(taken); len(b) > 0 {
		fmt.Printf("backend: %s (auto-detected, ssl=%v)\n", b[0].Addr, b[0].SSL)
	} else {
		fmt.Printf("backend: none found — every nginx listener collides with what rukh binds (%s).\n",
			strings.Join(taken, ", "))
		fmt.Println("  Note that a wildcard bind takes the whole port, so moving nginx to 127.0.0.1")
		fmt.Println("  on the SAME port is not enough. Pick one of:")
		fmt.Println("    - give nginx a different port:  listen 127.0.0.1:8080;")
		fmt.Println("    - or bind rukh to the public address only:  server.http: \"203.0.113.5:80\"")
		fmt.Println("    - or set backend.address explicitly")
	}
	return nil
}

func runDaemon(cfgPath, pidfile, sock string) (err error) {
	// First execution without a config: auto-provision the default
	// layout from the embedded skel, warn on stderr and keep going.
	if cfgPath == paths.ConfigFile {
		if _, statErr := os.Stat(cfgPath); os.IsNotExist(statErr) {
			fmt.Fprintln(os.Stderr, "rukh: no config found, provisioning default layout")
			if err := bootstrap.EnsureLayout(os.Stderr); err != nil {
				return err
			}
		}
	}

	mgr, err := config.NewManager(cfgPath)
	if err != nil {
		return err
	}

	// The log directory is the one path the daemon must be able to
	// write to; --init creates it, but a hand-started daemon with a
	// custom config must not fail just because it is missing.
	if err := os.MkdirAll(paths.LogDir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", paths.LogDir, err)
	}
	logs, err := logging.Open(paths.LogDir)
	if err != nil {
		return err
	}
	defer logs.Close()
	// Surface a fatal startup error (e.g. a port still held by nginx)
	// in the service log too, not only on stderr.
	defer func() {
		if err != nil {
			logs.Service.Error("fatal error, exiting", "error", err.Error())
		}
	}()

	logs.Service.Info("starting", "version", version, "config", cfgPath, "pid", os.Getpid())
	for _, w := range mgr.Get().Warnings {
		logs.Service.Warn("config warning", "warning", w)
	}

	m := metrics.New()
	stop := make(chan struct{})

	// Discovery of virtual hosts and certificates from nginx. A broken
	// or unreadable nginx configuration is not fatal: rukh still
	// proxies, it just has no certificate to serve.
	ng := nginx.NewStore(mgr.Get().Nginx.Config, logs.Service)
	if err := ng.Load(); err != nil {
		logs.Service.Error("cannot read the nginx configuration", "error", err)
		fmt.Fprintln(os.Stderr, "rukh: warning:", err)
	} else {
		c := ng.Get()
		logs.Service.Info("nginx discovered",
			"sites", len(c.Sites), "hosts", len(c.Hosts()), "files", len(c.Files))
		for _, w := range c.Warnings {
			logs.Service.Warn("nginx config warning", "warning", w)
		}
	}
	certStore := certs.New()

	// The traffic model and its ingest loop.
	engine := learn.New(learnParams(mgr.Get()), m, logs.Learn)
	engine.Start(stop)

	// Manually configured Early Hints, one file per host. They matter
	// most behind a CDN, where the static resources never reach the
	// origin and traffic therefore cannot teach them.
	manual := hints.NewStore(mgr.Get().Hints.Dir, logs.Service)
	manual.LoadAll()
	if err := manual.Watch(stop); err != nil {
		logs.Service.Error("cannot watch the hints directory", "error", err)
	}
	if n := manual.Count(); n > 0 {
		logs.Service.Info("manual hints loaded", "hosts", n)
	}

	prx := proxy.New(mgr, ng, engine, manual, m, logs)
	srv := proxy.NewServer(prx, mgr, ng, certStore, logs)

	warmer := preload.New(engine, mgr, prx.Backend, prx.Transport, m, logs.Learn, version)
	warmer.Start(stop)

	// A change in the nginx setup can move the upstream or add a host.
	ng.Watch(stop, mgr.Get().Nginx.Refresh.Std(), func(*nginx.Config) { prx.Reconfigure() })

	err = mgr.Watch(stop,
		func(err error) { logs.Service.Error("config reload failed", "error", err) },
		func(cfg *config.Config) {
			logs.Service.Info("config reloaded", "warnings", len(cfg.Warnings))
			for _, w := range cfg.Warnings {
				logs.Service.Warn("config warning", "warning", w)
			}
			engine.SetParams(learnParams(cfg))
			prx.Reconfigure()
			manual.LoadAll()
		})
	if err != nil {
		return err
	}

	// Local status socket: the IPC channel behind `rukh status`. If it
	// fails the daemon still serves; status reports "not running".
	collect := status.NewCollector(status.Sources{
		Version:  version,
		Config:   mgr,
		Nginx:    ng,
		Certs:    certStore,
		Learn:    engine,
		Hints:    manual,
		Preload:  warmer,
		Metrics:  m,
		LogDir:   paths.LogDir,
		TopPages: 10,
		BackendURL: func() (string, bool) {
			b := prx.Backend()
			return b.Addr, b.Auto
		},
		HTTP3: srv.HTTP3,
	})
	statusSrv, err := status.Serve(sock, collect)
	if err != nil {
		logs.Service.Error("status socket unavailable", "error", err)
	}

	if err := srv.Start(); err != nil {
		return err
	}
	if err := proc.WritePidfile(pidfile); err != nil {
		logs.Service.Error("cannot write pidfile", "path", pidfile, "error", err)
	}
	defer proc.RemovePidfile(pidfile)

	// Single signal loop: SIGHUP reloads config and reopens the logs
	// (logrotate hook), SIGTERM/SIGINT shut down gracefully.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for s := range sig {
		if s == syscall.SIGHUP {
			logs.Service.Info("SIGHUP received, reloading config and reopening log files")
			if err := logs.Reopen(); err != nil {
				logs.Service.Error("log reopen failed", "error", err)
			}
			if err := mgr.Reload(); err != nil {
				logs.Service.Error("config reload failed", "error", err)
			} else {
				engine.SetParams(learnParams(mgr.Get()))
				prx.Reconfigure()
			}
			if err := ng.Load(); err != nil {
				logs.Service.Error("nginx config reload failed", "error", err)
			}
			manual.LoadAll()
			continue
		}
		logs.Service.Info("shutting down", "signal", s.String())
		close(stop)
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		srv.Shutdown(ctx)
		cancel()
		if statusSrv != nil {
			statusSrv.Close()
		}
		logs.Service.Info("shutdown complete")
		return nil
	}
	return nil
}

// learnParams translates the configuration into the model's tunables.
func learnParams(c *config.Config) learn.Params {
	return learn.Params{
		HalfLife:         c.Learn.HalfLife.Std(),
		MaxHosts:         c.Learn.MaxHosts,
		MaxPagesPerHost:  c.Learn.MaxPagesPerHost,
		MaxAssetsPerPage: c.Learn.MaxAssetsPerPage,
		MaxNextPerPage:   c.Learn.MaxNextPerPage,
		PruneInterval:    c.Learn.PruneInterval.Std(),
		RebuildInterval:  c.Learn.RebuildInterval.Std(),
		MinScore:         c.Learn.MinScore,
		QueueSize:        c.Learn.QueueSize,

		HintsEnabled:      c.Hints.Enabled,
		HintMinConfidence: c.Hints.MinConfidence,
		HintMinSamples:    c.Hints.MinSamples,
		HintMaxLinks:      c.Hints.MaxLinks,
		HintMaxAge:        c.Hints.MaxAge.Std(),

		PrefetchEnabled:  c.Prefetch.Enabled,
		PrefetchMinProb:  c.Prefetch.MinProbability,
		PrefetchMaxLinks: c.Prefetch.MaxLinks,

		PreloadMaxPages:   c.Preload.MaxPages,
		PreloadMinRefresh: c.Preload.MinRefresh.Std(),
		PreloadMaxRefresh: c.Preload.MaxRefresh.Std(),
	}
}

func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "rukh:", err)
		os.Exit(1)
	}
}
