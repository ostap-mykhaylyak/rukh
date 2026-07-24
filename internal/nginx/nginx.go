// Package nginx reads the running nginx configuration to discover, at
// startup and on every refresh, which virtual hosts exist, which
// certificate each one uses and on which loopback port nginx is
// listening.
//
// This is the whole point of rukh's "zero duplication" promise: the
// certificates stay configured (and renewed) exactly where they are
// today, in nginx; rukh only reads the paths out of nginx.conf and
// serves the same files.
//
// The parser is a small tokenizer plus a recursive block reader. It
// understands only what matters here (include, server, listen,
// server_name, ssl_certificate, ssl_certificate_key, ssl) and skips
// everything else, so an unknown or exotic directive can never break
// the discovery.
package nginx

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Listen is one normalized listen directive.
type Listen struct {
	Addr    string `json:"addr"` // host:port, "0.0.0.0:80" style
	SSL     bool   `json:"ssl"`
	Default bool   `json:"default"`
}

// Site is one server block worth serving in front of.
type Site struct {
	Names    []string `json:"names"` // server_name values, wildcards kept
	Listens  []Listen `json:"-"`
	SSL      bool     `json:"ssl"`
	Default  bool     `json:"default"`
	CertFile string   `json:"cert_file,omitempty"`
	KeyFile  string   `json:"key_file,omitempty"`
}

// Backend is a loopback listener of nginx that rukh can use as its
// upstream (i.e. one nginx kept for itself, not a public port).
type Backend struct {
	Addr string `json:"addr"`
	SSL  bool   `json:"ssl"`
}

// Config is the result of one discovery pass.
type Config struct {
	Path     string   `json:"path"`
	Sites    []Site   `json:"sites"`
	Files    []string `json:"-"` // every file read, for change detection
	Warnings []string `json:"warnings,omitempty"`
}

// maxIncludeDepth guards against an include cycle in a hand-written
// configuration.
const maxIncludeDepth = 16

// Parse reads the nginx configuration rooted at path, following
// includes, and returns the discovered sites.
func Parse(path string) (*Config, error) {
	c := &Config{Path: path}
	prefix := filepath.Dir(path)
	// nginx resolves relative includes against its prefix, which for a
	// distribution package is the directory holding nginx.conf.
	root, err := c.parseFile(path, prefix, 0)
	if err != nil {
		return nil, err
	}
	c.collect(root, prefix)
	return c, nil
}

// directive is one parsed directive: a name, its arguments and, for a
// block directive, its children.
type directive struct {
	name  string
	args  []string
	block []directive
}

func (c *Config) parseFile(path, prefix string, depth int) ([]directive, error) {
	if depth > maxIncludeDepth {
		return nil, fmt.Errorf("nginx config: include depth exceeded at %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("nginx config: %w", err)
	}
	c.Files = append(c.Files, path)

	toks := tokenize(string(data))
	dirs, rest, err := parseBlock(toks, false)
	if err != nil {
		return nil, fmt.Errorf("nginx config %s: %w", path, err)
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("nginx config %s: unexpected '}'", path)
	}
	return c.expandIncludes(dirs, prefix, depth), nil
}

// expandIncludes replaces every include directive with the directives
// of the included files, recursively. A missing or unreadable include
// is a warning, never fatal: nginx itself tolerates globs matching
// nothing, and a partially readable configuration still yields useful
// discovery.
func (c *Config) expandIncludes(dirs []directive, prefix string, depth int) []directive {
	out := make([]directive, 0, len(dirs))
	for _, d := range dirs {
		if d.name == "include" && len(d.args) == 1 {
			pattern := d.args[0]
			if !isAbs(pattern) {
				pattern = filepath.Join(prefix, pattern)
			}
			matches, err := filepath.Glob(pattern)
			if err != nil || len(matches) == 0 {
				c.warn("include %q matched no file", d.args[0])
				continue
			}
			sort.Strings(matches)
			for _, m := range matches {
				sub, err := c.parseFile(m, prefix, depth+1)
				if err != nil {
					c.warn("%v", err)
					continue
				}
				out = append(out, sub...)
			}
			continue
		}
		if len(d.block) > 0 {
			d.block = c.expandIncludes(d.block, prefix, depth)
		}
		out = append(out, d)
	}
	return out
}

func (c *Config) warn(format string, a ...any) {
	c.Warnings = append(c.Warnings, fmt.Sprintf(format, a...))
}

// collect walks the parsed tree and extracts the http-level defaults
// and every server block.
func (c *Config) collect(dirs []directive, prefix string) {
	for _, d := range dirs {
		if d.name != "http" {
			continue
		}
		var defCert, defKey string
		for _, h := range d.block {
			switch h.name {
			case "ssl_certificate":
				if len(h.args) == 1 {
					defCert = h.args[0]
				}
			case "ssl_certificate_key":
				if len(h.args) == 1 {
					defKey = h.args[0]
				}
			}
		}
		for _, h := range d.block {
			if h.name == "server" {
				if s, ok := c.site(h, defCert, defKey, prefix); ok {
					c.Sites = append(c.Sites, s)
				}
			}
		}
	}
}

func (c *Config) site(d directive, defCert, defKey, prefix string) (Site, bool) {
	s := Site{CertFile: defCert, KeyFile: defKey}
	sslOn := false
	for _, e := range d.block {
		switch e.name {
		case "listen":
			if l, ok := c.listen(e.args); ok {
				s.Listens = append(s.Listens, l)
			}
		case "server_name":
			for _, n := range e.args {
				n = strings.TrimSuffix(strings.ToLower(n), ".")
				if n == "" {
					continue
				}
				if strings.HasPrefix(n, "~") {
					c.warn("server_name %q: regular expressions are not supported", n)
					continue
				}
				s.Names = append(s.Names, n)
			}
		case "ssl_certificate":
			if len(e.args) == 1 {
				s.CertFile = e.args[0]
			}
		case "ssl_certificate_key":
			if len(e.args) == 1 {
				s.KeyFile = e.args[0]
			}
		case "ssl": // deprecated form: ssl on;
			sslOn = len(e.args) == 1 && e.args[0] == "on"
		}
	}
	for _, l := range s.Listens {
		if l.SSL {
			s.SSL = true
		}
		if l.Default {
			s.Default = true
		}
	}
	if sslOn {
		s.SSL = true
	}

	// A certificate path containing a runtime variable cannot be
	// resolved statically; drop it rather than guessing.
	if strings.Contains(s.CertFile, "$") || strings.Contains(s.KeyFile, "$") {
		c.warn("server %v: certificate path contains a variable, ignored", s.Names)
		s.CertFile, s.KeyFile = "", ""
	}
	s.CertFile = abs(s.CertFile, prefix)
	s.KeyFile = abs(s.KeyFile, prefix)
	if s.SSL && (s.CertFile == "" || s.KeyFile == "") {
		c.warn("server %v listens on ssl but has no usable certificate", s.Names)
	}
	if len(s.Names) == 0 && !s.Default {
		return s, false
	}
	return s, true
}

func abs(path, prefix string) string {
	if path == "" || isAbs(path) {
		return path
	}
	return filepath.Join(prefix, path)
}

// isAbs follows nginx's own notion of an absolute path (a leading
// slash), not the host's: the configuration being parsed always
// describes a Unix filesystem, even when the parser runs elsewhere
// during development.
func isAbs(path string) bool {
	return strings.HasPrefix(path, "/") || filepath.IsAbs(path)
}

// listen normalizes a listen directive. Unix sockets are skipped: rukh
// speaks TCP to nginx.
func (c *Config) listen(args []string) (Listen, bool) {
	if len(args) == 0 {
		return Listen{}, false
	}
	spec := args[0]
	if strings.HasPrefix(spec, "unix:") {
		return Listen{}, false
	}
	l := Listen{}
	for _, a := range args[1:] {
		switch a {
		case "ssl":
			l.SSL = true
		case "default_server", "default":
			l.Default = true
		}
	}

	host, port := "0.0.0.0", ""
	switch {
	case strings.HasPrefix(spec, "["): // [::]:443 or [::1]:8443
		end := strings.Index(spec, "]")
		if end < 0 {
			return Listen{}, false
		}
		host = spec[1:end]
		rest := spec[end+1:]
		if strings.HasPrefix(rest, ":") {
			port = rest[1:]
		}
	case strings.Contains(spec, ":"):
		i := strings.LastIndex(spec, ":")
		host, port = spec[:i], spec[i+1:]
	default:
		if isNumeric(spec) {
			port = spec
		} else {
			host = spec // address without port: nginx defaults to :80
		}
	}
	if port == "" {
		port = "80"
		if l.SSL {
			port = "443"
		}
	}
	if !isNumeric(port) {
		c.warn("listen %q: unsupported port", spec)
		return Listen{}, false
	}
	if host == "*" || host == "" {
		host = "0.0.0.0"
	}
	if strings.Contains(host, ":") { // bare IPv6
		host = "[" + host + "]"
	}
	l.Addr = host + ":" + port
	return l, true
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil
}

// Hosts returns every hostname declared by the discovered sites,
// lowercased and deduplicated.
func (c *Config) Hosts() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range c.Sites {
		for _, n := range s.Names {
			if n == "_" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Match returns the site serving the given hostname, following nginx's
// own precedence: exact name, then longest wildcard, then the default
// server. Returns nil when nothing matches.
func (c *Config) Match(host string) *Site {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if h, _, err := splitPort(host); err == nil {
		host = h
	}
	var wildcard *Site
	var wildcardLen int
	var deflt *Site
	for i := range c.Sites {
		s := &c.Sites[i]
		if s.Default && deflt == nil {
			deflt = s
		}
		for _, n := range s.Names {
			switch {
			case n == host:
				return s
			case n == "_":
				if deflt == nil {
					deflt = s
				}
			case strings.HasPrefix(n, "*."):
				suffix := n[1:] // ".example.com"
				if strings.HasSuffix(host, suffix) && len(suffix) > wildcardLen {
					wildcard, wildcardLen = s, len(suffix)
				}
			case strings.HasSuffix(n, ".*"):
				base := n[:len(n)-1] // "www."
				if strings.HasPrefix(host, base) && len(base) > wildcardLen {
					wildcard, wildcardLen = s, len(base)
				}
			}
		}
	}
	if wildcard != nil {
		return wildcard
	}
	return deflt
}

// splitPort is net.SplitHostPort without the import cycle of intent:
// it must not fail on a bare hostname.
func splitPort(s string) (string, string, error) {
	i := strings.LastIndex(s, ":")
	if i < 0 || strings.Contains(s[:i], ":") {
		return "", "", fmt.Errorf("no port")
	}
	return s[:i], s[i+1:], nil
}

// Backends returns the listeners nginx keeps that rukh may use as its
// upstream: everything that is not one of the addresses rukh binds
// itself. Loopback listeners come first, then the lowest port; this is
// the order the auto-detection tries.
func (c *Config) Backends(taken []string) []Backend {
	takenPorts := map[string]bool{}
	for _, t := range taken {
		if _, p, err := splitPort(t); err == nil {
			takenPorts[p] = true
		}
	}
	seen := map[string]bool{}
	var out []Backend
	for _, s := range c.Sites {
		for _, l := range s.Listens {
			host, port, err := splitPort(l.Addr)
			if err != nil || takenPorts[port] || seen[l.Addr] {
				continue
			}
			seen[l.Addr] = true
			addr := l.Addr
			// A wildcard bind is reachable on loopback, and loopback is
			// the hop we want (no traffic leaves the machine).
			if host == "0.0.0.0" || host == "[::]" {
				addr = "127.0.0.1:" + port
			}
			out = append(out, Backend{Addr: addr, SSL: l.SSL})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := isLoopbackAddr(out[i].Addr), isLoopbackAddr(out[j].Addr)
		if li != lj {
			return li
		}
		pi, _ := strconv.Atoi(portOf(out[i].Addr))
		pj, _ := strconv.Atoi(portOf(out[j].Addr))
		return pi < pj
	})
	return out
}

func portOf(addr string) string {
	_, p, err := splitPort(addr)
	if err != nil {
		return ""
	}
	return p
}

func isLoopbackAddr(addr string) bool {
	h, _, err := splitPort(addr)
	if err != nil {
		return false
	}
	return strings.HasPrefix(h, "127.") || h == "[::1]" || h == "localhost"
}
