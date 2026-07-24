package proxy

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// realIP resolves the address of the actual client.
//
// rukh is normally the edge, so the peer address IS the client. When
// something else sits in front (a CDN, another load balancer), list it
// in realip.trusted_proxies and the client is taken from the
// forwarding header instead — walking the chain from the right and
// skipping the addresses that are trusted, so a client cannot forge
// its own address by sending the header itself.
type realIP struct {
	header  string
	trusted []netip.Prefix
}

func newRealIP(header string, trusted []string) *realIP {
	r := &realIP{header: header}
	for _, t := range trusted {
		if p, err := netip.ParsePrefix(t); err == nil {
			r.trusted = append(r.trusted, p)
			continue
		}
		if a, err := netip.ParseAddr(t); err == nil {
			r.trusted = append(r.trusted, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return r
}

// peer returns the address rukh is actually talking to.
func peer(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

func (r *realIP) isTrusted(ip string) bool {
	if len(r.trusted) == 0 {
		return false
	}
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientIP returns the client address for logging, learning and the
// X-Real-IP header handed to nginx.
func (r *realIP) clientIP(req *http.Request) string {
	p := peer(req)
	if !r.isTrusted(p) {
		return p
	}
	values := req.Header.Values(r.header)
	if len(values) == 0 {
		return p
	}
	// Walk the chain right to left: the first address that is not one
	// of our trusted proxies is the client.
	var chain []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				chain = append(chain, part)
			}
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if !r.isTrusted(chain[i]) {
			return chain[i]
		}
	}
	return p
}

// forwardedFor returns the value of the X-Forwarded-For header to hand
// to nginx: the inbound chain plus the peer when the peer is trusted,
// the peer alone otherwise (an untrusted client must never be able to
// inject addresses into the chain).
func (r *realIP) forwardedFor(req *http.Request) string {
	p := peer(req)
	if !r.isTrusted(p) {
		return p
	}
	inbound := strings.Join(req.Header.Values("X-Forwarded-For"), ", ")
	if inbound == "" {
		return p
	}
	return inbound + ", " + p
}
