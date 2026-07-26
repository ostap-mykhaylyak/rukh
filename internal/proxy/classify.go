package proxy

import (
	"net/http"
	"path"
	"strings"
)

// asType returns the value of the preload "as" attribute for a
// subresource, or "" when the request is not a subresource rukh knows
// how to announce.
//
// The browser tells us directly through Sec-Fetch-Dest (every current
// browser sends it); the Content-Type of the response and finally the
// file extension are the fallbacks for the rest.
func asType(req *http.Request, contentType string) string {
	switch req.Header.Get("Sec-Fetch-Dest") {
	case "style":
		return "style"
	case "script", "worker", "serviceworker", "sharedworker":
		return "script"
	case "font":
		return "font"
	case "image":
		return "image"
	case "document", "iframe", "frame", "embed", "object":
		return "" // navigations are pages, not subresources
	}

	ct := contentType
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.ToLower(strings.TrimSpace(ct))
	switch {
	case ct == "text/css":
		return "style"
	case strings.Contains(ct, "javascript") || ct == "text/ecmascript" || ct == "application/ecmascript":
		return "script"
	case strings.HasPrefix(ct, "font/") || ct == "application/font-woff" || ct == "application/font-woff2" ||
		ct == "application/vnd.ms-fontobject" || ct == "application/x-font-ttf":
		return "font"
	case strings.HasPrefix(ct, "image/"):
		return "image"
	}

	switch strings.ToLower(path.Ext(strings.SplitN(req.URL.Path, "?", 2)[0])) {
	case ".css":
		return "style"
	case ".js", ".mjs":
		return "script"
	case ".woff", ".woff2", ".ttf", ".otf", ".eot":
		return "font"
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".svg", ".ico":
		return "image"
	}
	return ""
}

// isHTML reports whether a response is an HTML document.
func isHTML(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	return strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "application/xhtml+xml")
}

// wantsHTML reports whether the request looks like a navigation to a
// page: this is what makes Early Hints worth sending.
func wantsHTML(req *http.Request) bool {
	switch req.Header.Get("Sec-Fetch-Dest") {
	case "document", "iframe", "frame":
		return true
	case "":
		// Old client or a non-browser: fall back to Accept.
	default:
		return false
	}
	return strings.Contains(req.Header.Get("Accept"), "text/html")
}

// scheme reports how the visitor reached rukh. Together with the
// protocol version in the same log line it answers "is this client
// getting HTTP/2, and if not why": h2 requires TLS.
func scheme(req *http.Request) string {
	if req.TLS != nil {
		return "https"
	}
	return "http"
}

// isSpeculative reports whether a request was made by the browser on
// its own initiative — a prefetch or a prerender — rather than by a
// visitor.
//
// This matters more than it looks: a prefetch rukh itself suggested
// arrives back as a perfectly ordinary GET, with the suggesting page
// as its Referer. Counting it would make every prediction confirm
// itself, the probability would climb to certainty on its own, and the
// model would end up describing rukh's guesses instead of the
// visitors' behaviour.
func isSpeculative(req *http.Request) bool {
	// Sec-Purpose is the current header (Chrome, Edge); the others are
	// the historical spellings still emitted by some clients.
	if p := req.Header.Get("Sec-Purpose"); p != "" {
		return strings.Contains(p, "prefetch") || strings.Contains(p, "prerender")
	}
	for _, h := range []string{"Purpose", "X-Purpose", "X-Moz"} {
		switch strings.ToLower(req.Header.Get(h)) {
		case "prefetch", "preview", "prerender":
			return true
		}
	}
	return false
}

// refererPath returns the path of the referring page when it belongs
// to the same host, normalized like every other model key. Anything
// cross-origin is invisible to the model on purpose.
// It is parsed by hand rather than with net/url: this runs on every
// subresource request, and url.Parse allocates a dozen times to build
// a structure of which two fields are used.
func refererPath(req *http.Request, host string) string {
	ref := req.Header.Get("Referer")
	rest, ok := strings.CutPrefix(ref, "https://")
	if !ok {
		if rest, ok = strings.CutPrefix(ref, "http://"); !ok {
			return "" // relative or opaque referrer: nothing to attribute
		}
	}
	refHost, target := rest, "/"
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		refHost, target = rest[:i], rest[i:]
	}
	if i := strings.IndexByte(target, '#'); i >= 0 {
		target = target[:i]
	}
	if refHost == "" || !strings.EqualFold(hostOnly(refHost), hostOnly(host)) {
		return ""
	}
	if target == "" {
		return "/"
	}
	return target
}

// hostOnly strips the port (and the IPv6 brackets) from a Host header
// value.
func hostOnly(h string) string {
	if strings.HasPrefix(h, "[") { // [::1]:443 or [::1]
		if i := strings.IndexByte(h, ']'); i >= 0 {
			return h[1:i]
		}
		return h
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[:i], ":") {
		return h[:i]
	}
	return h
}

// cacheableResponse reports whether a page response looks safe to warm
// up later: a personalized answer (a session cookie being set, an
// explicit no-store/private) must never be preloaded, or the warm-up
// would poison a shared cache with somebody's page.
func cacheableResponse(resp *http.Response) bool {
	if resp.Header.Get("Set-Cookie") != "" {
		return false
	}
	cc := strings.ToLower(resp.Header.Get("Cache-Control"))
	if strings.Contains(cc, "no-store") || strings.Contains(cc, "private") {
		return false
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Vary")), "cookie") {
		return false
	}
	return true
}
