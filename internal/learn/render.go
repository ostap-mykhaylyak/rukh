package learn

import "strings"

// LinkPreload renders the Link header values of an Early Hints
// response: one entry per resource, in the order the browser should
// start them.
//
// Fonts are always fetched in CORS mode by the browser, even
// same-origin: without crossorigin the preloaded font would be
// downloaded twice, so the attribute is mandatory there.
func LinkPreload(hints []Hint) []string {
	if len(hints) == 0 {
		return nil
	}
	out := make([]string, 0, len(hints))
	for _, h := range hints {
		var b strings.Builder
		b.WriteByte('<')
		b.WriteString(escapeURL(h.URL))
		b.WriteString(">; rel=preload; as=")
		b.WriteString(h.As)
		if h.CrossOrigin {
			b.WriteString("; crossorigin")
		}
		out = append(out, b.String())
	}
	return out
}

// LinkPrefetch renders Link rel=prefetch values for the pages a
// visitor is most likely to open next. Prefetch is low priority by
// definition: the browser fetches them when idle.
func LinkPrefetch(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, "<"+escapeURL(p)+">; rel=prefetch")
	}
	return out
}

// escapeURL removes the few characters that would break out of the
// header syntax. Paths come from observed traffic, so they are treated
// as untrusted input.
func escapeURL(u string) string {
	if !strings.ContainsAny(u, "<>\"\r\n ,;") {
		return u
	}
	var b strings.Builder
	b.Grow(len(u))
	for i := 0; i < len(u); i++ {
		switch c := u[i]; c {
		case '<':
			b.WriteString("%3C")
		case '>':
			b.WriteString("%3E")
		case '"':
			b.WriteString("%22")
		case ' ':
			b.WriteString("%20")
		case ',':
			b.WriteString("%2C")
		case ';':
			b.WriteString("%3B")
		case '\r', '\n':
			// dropped: never let a request path forge a header
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
