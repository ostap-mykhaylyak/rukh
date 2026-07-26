package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ostap-mykhaylyak/rukh/internal/certs"
	"github.com/ostap-mykhaylyak/rukh/internal/config"
	"github.com/ostap-mykhaylyak/rukh/internal/hints"
	"github.com/ostap-mykhaylyak/rukh/internal/learn"
	"github.com/ostap-mykhaylyak/rukh/internal/logging"
	"github.com/ostap-mykhaylyak/rukh/internal/metrics"
	"github.com/ostap-mykhaylyak/rukh/internal/nginx"
)

// writeCert produces the self-signed pair a test nginx configuration
// points at, so the TLS listener has something to serve.
func writeCert(t *testing.T, dir, host string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		DNSNames:              []string{host},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certFile = filepath.Join(dir, "fullchain.pem")
	keyFile = filepath.Join(dir, "privkey.pem")
	certOut, _ := os.Create(certFile)
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	certOut.Close()
	kb, _ := x509.MarshalECPrivateKey(key)
	keyOut, _ := os.Create(keyFile)
	pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	keyOut.Close()
	return certFile, keyFile
}

// startTLSServer brings up the real Server on a TLS port, with the
// certificate discovered from a test nginx configuration.
func startTLSServer(t *testing.T, backendAddr string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir, "tls.test")

	ngPath := filepath.Join(dir, "nginx.conf")
	os.WriteFile(ngPath, []byte(fmt.Sprintf(`http {
		server {
			listen 127.0.0.1:8443 ssl;
			server_name tls.test;
			ssl_certificate %q;
			ssl_certificate_key %q;
		}
	}`, filepath.ToSlash(certFile), filepath.ToSlash(keyFile))), 0o644)

	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf(`
server:
  http: ""
  https: "127.0.0.1:0"
backend:
  address: %q
nginx:
  config: %q
`, backendAddr, filepath.ToSlash(ngPath))), 0o644)

	mgr, err := config.NewManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	logs := logging.Discard()
	ng := nginx.NewStore(ngPath, logs.Service)
	if err := ng.Load(); err != nil {
		t.Fatal(err)
	}
	engine := learn.New(learn.Params{QueueSize: 64, HalfLife: time.Hour}, nil, logs.Learn)
	p := New(mgr, ng, engine, hints.NewStore(filepath.Join(dir, "hints"), logs.Service), metrics.New(), logs)
	srv := NewServer(p, mgr, ng, certs.New(), logs)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	_, addr := srv.Addrs()
	return srv, addr
}

// Visitors must get the best protocol their client supports: over TLS
// that means HTTP/2, negotiated with ALPN.
func TestVisitorsGetHTTP2OverTLS(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html></html>")
	}))
	defer be.Close()

	_, addr := startTLSServer(t, strings.TrimPrefix(be.URL, "http://"))

	client := &http.Client{Transport: &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "tls.test",
			NextProtos:         []string{"h2", "http/1.1"},
		},
	}}
	req, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/", nil)
	req.Host = "tls.test"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.Proto != "HTTP/2.0" {
		t.Fatalf("proto = %q, want HTTP/2.0: the TLS listener must advertise h2 via ALPN", resp.Proto)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// A client that only speaks HTTP/1.1 must still work: ALPN falls back.
func TestHTTP11ClientsStillWork(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer be.Close()

	_, addr := startTLSServer(t, strings.TrimPrefix(be.URL, "http://"))
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "tls.test",
			NextProtos:         []string{"http/1.1"},
		},
	}}
	req, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/", nil)
	req.Host = "tls.test"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Proto != "HTTP/1.1" || resp.StatusCode != http.StatusOK {
		t.Fatalf("proto = %q, status = %d", resp.Proto, resp.StatusCode)
	}
}

// The hop to nginx must reuse connections: opening a TCP connection
// per request would add a handshake to every asset.
func TestBackendConnectionsAreReused(t *testing.T) {
	var opened atomic.Int64
	be := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	be.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	be.Start()
	defer be.Close()

	env := newEnv(t, "")
	env.proxy.backend.Store(&Backend{Addr: strings.TrimPrefix(be.URL, "http://")})

	for i := 0; i < 20; i++ {
		resp := env.get(t, fmt.Sprintf("/asset-%d.js", i), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}
	if n := opened.Load(); n != 1 {
		t.Fatalf("opened %d backend connections for 20 requests, want 1 (keep-alive)", n)
	}
}
