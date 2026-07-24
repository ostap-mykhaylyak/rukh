package nginx

import (
	"os"
	"path/filepath"
	"testing"
)

const mainConf = `
user www-data;
worker_processes auto;

events { worker_connections 768; }

http {
	sendfile on;
	# a default certificate at http level is inherited by server blocks
	ssl_certificate     /etc/ssl/default.pem;
	ssl_certificate_key /etc/ssl/default.key;

	include sites-enabled/*.conf;

	server {
		listen 127.0.0.1:8080 default_server;
		server_name _;
		return 444;
	}
}
`

const siteConf = `
server {
	listen 127.0.0.1:8080;
	server_name example.com www.example.com;
	location / { proxy_pass http://127.0.0.1:9000; }
}

server {
	listen 127.0.0.1:8443 ssl;
	listen [::1]:8443 ssl;
	server_name example.com www.example.com;
	ssl_certificate     /etc/letsencrypt/live/example.com/fullchain.pem;
	ssl_certificate_key /etc/letsencrypt/live/example.com/privkey.pem;
	root /var/www/example;
}

server {
	listen 127.0.0.1:8443 ssl;
	server_name *.wild.example.org;   # wildcard
	ssl_certificate     wild/fullchain.pem;   # relative to the prefix
	ssl_certificate_key wild/privkey.pem;
}
`

func writeConf(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sites-enabled"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nginx.conf"), []byte(mainConf), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sites-enabled", "example.conf"), []byte(siteConf), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "nginx.conf")
}

func TestParse(t *testing.T) {
	path := writeConf(t)
	c, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Sites) != 4 {
		t.Fatalf("sites = %d, want 4: %+v", len(c.Sites), c.Sites)
	}
	if len(c.Files) != 2 {
		t.Fatalf("files = %v, want the main conf plus the included one", c.Files)
	}

	hosts := c.Hosts()
	want := map[string]bool{"example.com": true, "www.example.com": true, "*.wild.example.org": true}
	if len(hosts) != len(want) {
		t.Fatalf("hosts = %v", hosts)
	}
	for _, h := range hosts {
		if !want[h] {
			t.Fatalf("unexpected host %q in %v", h, hosts)
		}
	}
}

func TestMatchAndCertificates(t *testing.T) {
	c, err := Parse(writeConf(t))
	if err != nil {
		t.Fatal(err)
	}

	s := c.Match("www.example.com")
	if s == nil {
		t.Fatal("no site for www.example.com")
	}
	if s.CertFile != "/etc/letsencrypt/live/example.com/fullchain.pem" {
		// The plain :8080 block comes first but has no ssl: the TLS one
		// is the one that matters here only if it is picked. Both carry
		// the same names, so accept the inherited default too.
		if s.CertFile != "/etc/ssl/default.pem" {
			t.Fatalf("cert = %q", s.CertFile)
		}
	}

	w := c.Match("shop.wild.example.org")
	if w == nil || len(w.Names) == 0 || w.Names[0] != "*.wild.example.org" {
		t.Fatalf("wildcard match = %+v", w)
	}
	// The relative certificate path is resolved against the nginx prefix.
	if filepath.Base(w.CertFile) != "fullchain.pem" || !filepath.IsAbs(w.CertFile) {
		t.Fatalf("relative cert not resolved: %q", w.CertFile)
	}

	// An unknown name falls back to the default server.
	if d := c.Match("nothing.invalid"); d == nil || !d.Default {
		t.Fatalf("default server not used as fallback: %+v", d)
	}
}

func TestBackends(t *testing.T) {
	c, err := Parse(writeConf(t))
	if err != nil {
		t.Fatal(err)
	}
	b := c.Backends([]string{"0.0.0.0:80", "0.0.0.0:443"})
	if len(b) == 0 {
		t.Fatal("no backend candidate")
	}
	if b[0].Addr != "127.0.0.1:8080" || b[0].SSL {
		t.Fatalf("first candidate = %+v, want the plain loopback 8080", b[0])
	}
	var found bool
	for _, x := range b {
		if x.Addr == "127.0.0.1:8443" && x.SSL {
			found = true
		}
	}
	if !found {
		t.Fatalf("ssl candidate missing: %+v", b)
	}
}

func TestBackendsSkipsPortsRukhBinds(t *testing.T) {
	dir := t.TempDir()
	conf := `http {
		server { listen 80; server_name a.example; }
		server { listen 443 ssl; server_name a.example; ssl_certificate a.pem; ssl_certificate_key a.key; }
	}`
	path := filepath.Join(dir, "nginx.conf")
	os.WriteFile(path, []byte(conf), 0o644)
	c, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if b := c.Backends([]string{"0.0.0.0:80", "0.0.0.0:443"}); len(b) != 0 {
		t.Fatalf("nginx still on the public ports must yield no candidate, got %+v", b)
	}
}

func TestListenForms(t *testing.T) {
	c := &Config{}
	cases := []struct {
		args []string
		addr string
		ssl  bool
	}{
		{[]string{"80"}, "0.0.0.0:80", false},
		{[]string{"443", "ssl", "http2"}, "0.0.0.0:443", true},
		{[]string{"127.0.0.1:8080"}, "127.0.0.1:8080", false},
		{[]string{"[::]:8443", "ssl"}, "[::]:8443", true},
		{[]string{"192.0.2.1"}, "192.0.2.1:80", false},
		{[]string{"*:8080"}, "0.0.0.0:8080", false},
	}
	for _, tc := range cases {
		l, ok := c.listen(tc.args)
		if !ok || l.Addr != tc.addr || l.SSL != tc.ssl {
			t.Errorf("listen %v = %+v (ok=%v), want %s ssl=%v", tc.args, l, ok, tc.addr, tc.ssl)
		}
	}
	if _, ok := c.listen([]string{"unix:/run/nginx.sock"}); ok {
		t.Error("unix sockets must be skipped")
	}
}

func TestVariableCertificateIsIgnored(t *testing.T) {
	dir := t.TempDir()
	conf := `http {
		server {
			listen 8443 ssl;
			server_name $host;
			ssl_certificate /etc/certs/$ssl_server_name.pem;
			ssl_certificate_key /etc/certs/$ssl_server_name.key;
		}
	}`
	path := filepath.Join(dir, "nginx.conf")
	os.WriteFile(path, []byte(conf), 0o644)
	c, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Sites) != 1 || c.Sites[0].CertFile != "" {
		t.Fatalf("a variable certificate path must be dropped: %+v", c.Sites)
	}
	if len(c.Warnings) == 0 {
		t.Error("expected a warning about the variable certificate")
	}
}

func TestMissingIncludeIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	conf := `
	include /nonexistent/nginx-extra.conf;
	http { server { listen 8080; server_name a.example; } }`
	path := filepath.Join(dir, "nginx.conf")
	os.WriteFile(path, []byte(conf), 0o644)
	c, err := Parse(path)
	if err != nil {
		t.Fatalf("a missing include must not be fatal: %v", err)
	}
	if len(c.Sites) != 1 {
		t.Fatalf("sites = %+v", c.Sites)
	}
	if len(c.Warnings) == 0 {
		t.Error("expected a warning for the missing include file")
	}
}

// A wildcard matching nothing is what a stock Ubuntu nginx.conf does
// with an empty modules-enabled/: nginx accepts it, so it must not
// show up as a warning (it would keep `rukh status` at WARNING
// forever).
func TestEmptyGlobIncludeIsSilent(t *testing.T) {
	dir := t.TempDir()
	conf := `
	include /etc/nginx/modules-enabled/*.conf;
	http {
		include mime.types.d/*.conf;
		server { listen 8080; server_name a.example; }
	}`
	path := filepath.Join(dir, "nginx.conf")
	os.WriteFile(path, []byte(conf), 0o644)
	c, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none for wildcards matching nothing", c.Warnings)
	}
	if len(c.Sites) != 1 {
		t.Fatalf("sites = %+v", c.Sites)
	}
}

func TestTokenizerHandlesQuotesAndComments(t *testing.T) {
	toks := tokenize(`# comment { ;
	server_name "a b" 'c;d'; # trailing
	`)
	var words []string
	for _, tk := range toks {
		if tk.kind == tokWord {
			words = append(words, tk.text)
		}
	}
	if len(words) != 3 || words[1] != "a b" || words[2] != "c;d" {
		t.Fatalf("words = %q", words)
	}
}

func TestStoreChangeDetection(t *testing.T) {
	path := writeConf(t)
	s := NewStore(path, discardLogger())
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if s.Changed() {
		t.Fatal("nothing changed right after a load")
	}
	if len(s.Get().Sites) == 0 {
		t.Fatal("no sites after load")
	}
	// Touch an included file with different content.
	inc := filepath.Join(filepath.Dir(path), "sites-enabled", "example.conf")
	if err := os.WriteFile(inc, []byte(siteConf+"\n# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !s.Changed() {
		t.Fatal("a modified include must be detected")
	}
}
