package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/ostap-mykhaylyak/rukh/internal/certs"
	"github.com/ostap-mykhaylyak/rukh/internal/config"
	"github.com/ostap-mykhaylyak/rukh/internal/logging"
	"github.com/ostap-mykhaylyak/rukh/internal/nginx"
)

// Server owns the public listeners: :80 and :443, the ports nginx used
// to hold. TLS is terminated with the certificates declared in the
// nginx configuration, looked up per SNI at handshake time.
type Server struct {
	p     *Proxy
	cfg   *config.Manager
	ng    *nginx.Store
	certs *certs.Store
	logs  *logging.Streams

	httpSrv  *http.Server
	httpsSrv *http.Server
	httpLn   net.Listener
	httpsLn  net.Listener

	h3       *http3.Server
	h3Ln     net.PacketConn
	h3Active atomic.Bool

	lastCertLog atomic.Int64
}

// NewServer wires the listeners around a Proxy.
func NewServer(p *Proxy, cfg *config.Manager, ng *nginx.Store, cs *certs.Store, logs *logging.Streams) *Server {
	return &Server{p: p, cfg: cfg, ng: ng, certs: cs, logs: logs}
}

// Start binds and serves both entrypoints. It returns as soon as the
// listeners are up; serving continues in the background.
func (s *Server) Start() error {
	c := s.cfg.Get()

	if c.Server.HTTP != "" {
		ln, err := net.Listen("tcp", c.Server.HTTP)
		if err != nil {
			return bindError("server.http", c.Server.HTTP, err)
		}
		s.httpLn = ln
		s.httpSrv = s.newHTTPServer(c)
		go func() {
			if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.logs.Service.Error("http listener stopped", "error", err)
			}
		}()
		s.logs.Service.Info("listening", "entrypoint", "http", "addr", ln.Addr().String())
	}

	if c.Server.HTTPS != "" {
		ln, err := net.Listen("tcp", c.Server.HTTPS)
		if err != nil {
			s.Close()
			return bindError("server.https", c.Server.HTTPS, err)
		}
		s.httpsLn = ln
		s.httpsSrv = s.newHTTPServer(c)
		s.httpsSrv.TLSConfig = &tls.Config{
			GetCertificate: s.getCertificate,
			MinVersion:     tlsVersion(c.Server.TLSMinVersion),
			NextProtos:     []string{"h2", "http/1.1"},
		}
		go func() {
			// Certificates come from GetCertificate, so no files here.
			if err := s.httpsSrv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.logs.Service.Error("https listener stopped", "error", err)
			}
		}()
		s.logs.Service.Info("listening", "entrypoint", "https", "addr", ln.Addr().String())

		if c.Server.HTTP3 {
			s.startHTTP3(c, ln.Addr())
		}
	}
	return nil
}

// startHTTP3 serves QUIC on the same port as HTTPS, over UDP, with the
// same certificates. Failing to bind is never fatal: the TCP
// entrypoints keep serving and browsers simply never see the Alt-Svc
// advertisement, which is exactly the graceful degradation HTTP/3 is
// designed around.
func (s *Server) startHTTP3(c *config.Config, tcpAddr net.Addr) {
	host, _, err := net.SplitHostPort(c.Server.HTTPS)
	if err != nil {
		return
	}
	// The TCP listener's port, not the configured one: with :0 (tests)
	// they would otherwise differ, and the advertised port must be the
	// one visitors reached.
	port := strconv.Itoa(tcpAddr.(*net.TCPAddr).Port)
	addr := net.JoinHostPort(host, port)

	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		s.logs.Service.Error("http3 disabled: cannot bind UDP", "addr", addr, "error", err)
		return
	}
	s.h3Ln = pc
	s.h3 = &http3.Server{
		Handler: s.p,
		TLSConfig: http3.ConfigureTLSConfig(&tls.Config{
			GetCertificate: s.getCertificate,
			MinVersion:     tls.VersionTLS13, // QUIC is TLS 1.3 only
		}),
		// An asset-heavy page opens many concurrent streams; the
		// quic-go default (100) stalls them.
		QUICConfig: &quic.Config{
			MaxIncomingStreams:    1000,
			MaxIncomingUniStreams: 1000,
		},
	}
	// Tell every TLS response that HTTP/3 is available here.
	s.p.SetAltSvc(`h3=":` + port + `"; ma=86400`)

	go func() {
		if err := s.h3.Serve(pc); err != nil && !errors.Is(err, http.ErrServerClosed) &&
			!errors.Is(err, quic.ErrServerClosed) {
			s.logs.Service.Error("http3 listener stopped", "error", err)
		}
	}()
	s.h3Active.Store(true)
	s.logs.Service.Info("listening", "entrypoint", "http3", "addr", addr, "proto", "udp")
}

// HTTP3 reports whether HTTP/3 is configured and whether the QUIC
// listener is actually up: binding UDP can fail without stopping the
// daemon, and that difference must be visible.
func (s *Server) HTTP3() (enabled, active bool) {
	return s.cfg.Get().Server.HTTP3, s.h3Active.Load()
}

func (s *Server) newHTTPServer(c *config.Config) *http.Server {
	return &http.Server{
		Handler:           s.p,
		ReadHeaderTimeout: c.Server.ReadHeaderTimeout.Std(),
		IdleTimeout:       c.Server.IdleTimeout.Std(),
		ErrorLog:          slog.NewLogLogger(s.logs.Service.Handler(), slog.LevelWarn),
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			if a, ok := conn.LocalAddr().(*net.TCPAddr); ok {
				return context.WithValue(ctx, listenPortKey{}, fmt.Sprint(a.Port))
			}
			return ctx
		},
	}
}

// Addrs returns the bound addresses (empty when not listening); tests
// use them after binding on port 0.
func (s *Server) Addrs() (httpAddr, httpsAddr string) {
	if s.httpLn != nil {
		httpAddr = s.httpLn.Addr().String()
	}
	if s.httpsLn != nil {
		httpsAddr = s.httpsLn.Addr().String()
	}
	return
}

// getCertificate resolves the certificate for the requested server
// name using the nginx configuration: the same file nginx serves, so
// renewals are picked up with no extra configuration anywhere.
func (s *Server) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cfg := s.ng.Get()
	name := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))

	if site := cfg.Match(name); site != nil && site.CertFile != "" {
		cert, err := s.certs.GetPair(site.CertFile, site.KeyFile)
		if err == nil {
			return cert, nil
		}
		s.certLog("certificate unavailable", "server_name", name, "cert", site.CertFile, "error", err.Error())
	}

	// Fallback: the first site that has a usable certificate. A client
	// with no SNI, or a hostname nginx does not declare, still gets a
	// handshake (nginx will answer with its own default server).
	for i := range cfg.Sites {
		site := &cfg.Sites[i]
		if !site.SSL || site.CertFile == "" {
			continue
		}
		if cert, err := s.certs.GetPair(site.CertFile, site.KeyFile); err == nil {
			return cert, nil
		}
	}
	s.certLog("no certificate for handshake", "server_name", name)
	return nil, fmt.Errorf("no certificate for %q", name)
}

// certLog throttles handshake logging: a scanner hitting :443 with
// random SNI values must not fill the disk.
func (s *Server) certLog(msg string, args ...any) {
	now := time.Now().UnixNano()
	last := s.lastCertLog.Load()
	if now-last < int64(5*time.Second) {
		return
	}
	if !s.lastCertLog.CompareAndSwap(last, now) {
		return
	}
	s.logs.Service.Warn(msg, args...)
}

func tlsVersion(v string) uint16 {
	if v == "1.3" {
		return tls.VersionTLS13
	}
	return tls.VersionTLS12
}

// Shutdown drains the TCP servers and closes the QUIC one.
func (s *Server) Shutdown(ctx context.Context) {
	if s.httpSrv != nil {
		s.httpSrv.Shutdown(ctx)
	}
	if s.httpsSrv != nil {
		s.httpsSrv.Shutdown(ctx)
	}
	if s.h3 != nil {
		s.h3.Close()
	}
}

// Close drops the listeners without draining (startup failure path).
func (s *Server) Close() {
	if s.httpLn != nil {
		s.httpLn.Close()
	}
	if s.httpsLn != nil {
		s.httpsLn.Close()
	}
	if s.h3 != nil {
		s.h3.Close()
	}
	if s.h3Ln != nil {
		s.h3Ln.Close()
	}
}

// bindError explains the two failures an operator actually hits when
// putting rukh in front of nginx.
func bindError(field, addr string, err error) error {
	if strings.Contains(err.Error(), "address already in use") {
		return fmt.Errorf("%s: %s is already in use — something else holds that port. "+
			"Note that a wildcard bind collides with 127.0.0.1 on the SAME port, so moving "+
			"nginx to loopback is not enough: give nginx a different port "+
			"(e.g. listen 127.0.0.1:8080;) and reload it, or bind rukh to the public "+
			"address only (server.http: \"203.0.113.5:80\"): %w", field, addr, err)
	}
	if strings.Contains(err.Error(), "permission denied") {
		return fmt.Errorf("%s: cannot bind %s without privileges (ports below 1024 need root "+
			"or CAP_NET_BIND_SERVICE): %w", field, addr, err)
	}
	return fmt.Errorf("%s: %w", field, err)
}
