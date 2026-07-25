// Package hints implements the manually configured Early Hints: one
// YAML file per virtual host, in /etc/rukh/hints, whose name is the
// hostname it applies to.
//
// Why this exists. rukh learns which resources a page needs by
// watching them go through. Behind a CDN that caches static files at
// the edge — Cloudflare being the obvious case — those requests never
// reach the origin, so there is nothing to learn from: the HTML still
// arrives, the CSS does not. The operator knows the answer anyway, and
// this is where they write it down.
//
// Manual hints are not a fallback for the model: the two are merged,
// manual first (they are a decision, not a guess), and the learned
// ones fill whatever room is left.
package hints

import (
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ostap-mykhaylyak/rukh/internal/learn"
)

// Resource is one entry of a hints file. It accepts either a bare
// string (the URL, everything else inferred) or a mapping.
type Resource struct {
	URL string `yaml:"url"`
	// As is the preload type: style, script, font, image, fetch...
	// Inferred from the file extension when omitted.
	As string `yaml:"as"`
	// Rel defaults to preload; preconnect and dns-prefetch are useful
	// for a third-party origin the pages talk to.
	Rel string `yaml:"rel"`
	// CrossOrigin defaults to true for fonts (the browser fetches them
	// in CORS mode even same-origin, so without it the file would be
	// downloaded twice) and false for everything else.
	CrossOrigin *bool `yaml:"crossorigin"`
}

// UnmarshalYAML accepts the shorthand form, a plain URL string.
func (r *Resource) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		r.URL = s
		return nil
	}
	type plain Resource // avoid recursing into this method
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	*r = Resource(p)
	return nil
}

// File is one host's hints file.
type File struct {
	// Hosts overrides the hostname taken from the file name; useful to
	// cover the apex and the www alias with one file.
	Hosts []string `yaml:"hosts"`
	// Default applies to every page of the host.
	Default []Resource `yaml:"default"`
	// Paths applies to matching request paths only. A pattern ending
	// in * matches by prefix; everything else must match exactly.
	Paths map[string][]Resource `yaml:"paths"`
}

// Host is the compiled form served on the request path: immutable,
// looked up under a read of one atomic pointer.
type Host struct {
	Name string
	// Hosts are the hostnames this file applies to: what it declares,
	// or the file name.
	Hosts    []string
	Default  []learn.Hint
	exact    map[string][]learn.Hint
	prefixes []prefixRule
	Count    int
}

type prefixRule struct {
	prefix string
	hints  []learn.Hint
}

// Lookup returns the hints configured for a request path: the host
// default plus the most specific matching path rule.
func (h *Host) Lookup(path string) []learn.Hint {
	if h == nil {
		return nil
	}
	out := h.Default
	if extra, ok := h.exact[path]; ok {
		return append(append(make([]learn.Hint, 0, len(out)+len(extra)), out...), extra...)
	}
	// Longest prefix wins; rules are sorted longest first.
	for _, r := range h.prefixes {
		if strings.HasPrefix(path, r.prefix) {
			return append(append(make([]learn.Hint, 0, len(out)+len(r.hints)), out...), r.hints...)
		}
	}
	return out
}

// Parse compiles a hints file. Invalid entries are skipped and
// reported as warnings: one bad line must never cost the whole file.
func Parse(name string, data []byte) (*Host, []string, error) {
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", name, err)
	}

	var warnings []string
	compile := func(where string, in []Resource) []learn.Hint {
		out := make([]learn.Hint, 0, len(in))
		for _, r := range in {
			h, err := r.hint()
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%s: %s: %v", name, where, err))
				continue
			}
			out = append(out, h)
		}
		return out
	}

	host := &Host{
		Name:    name,
		Hosts:   []string{name},
		Default: compile("default", f.Default),
		exact:   map[string][]learn.Hint{},
	}
	if len(f.Hosts) > 0 {
		declared := make([]string, 0, len(f.Hosts))
		for _, h := range f.Hosts {
			if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
				declared = append(declared, h)
			}
		}
		if len(declared) > 0 {
			host.Hosts = declared
		}
	}
	host.Count = len(host.Default)
	for pattern, res := range f.Paths {
		hs := compile("paths: "+pattern, res)
		host.Count += len(hs)
		if strings.HasSuffix(pattern, "*") {
			host.prefixes = append(host.prefixes, prefixRule{prefix: strings.TrimSuffix(pattern, "*"), hints: hs})
			continue
		}
		host.exact[pattern] = hs
	}
	sort.Slice(host.prefixes, func(i, j int) bool {
		return len(host.prefixes[i].prefix) > len(host.prefixes[j].prefix)
	})
	return host, warnings, nil
}

// knownAs are the preload destinations rukh accepts. The browser
// rejects an unknown one and logs a console warning, so a typo is
// caught here instead.
var knownAs = map[string]bool{
	"style": true, "script": true, "font": true, "image": true,
	"fetch": true, "document": true, "audio": true, "video": true,
	"track": true, "embed": true, "object": true, "worker": true,
}

// hint validates one entry and turns it into the shape the renderer
// consumes.
func (r Resource) hint() (learn.Hint, error) {
	u := strings.TrimSpace(r.URL)
	if u == "" {
		return learn.Hint{}, fmt.Errorf("url is required")
	}
	crossOrigin := false
	switch {
	case strings.HasPrefix(u, "/"):
	case strings.HasPrefix(u, "http://"), strings.HasPrefix(u, "https://"):
		if _, err := url.Parse(u); err != nil {
			return learn.Hint{}, fmt.Errorf("%q: %w", u, err)
		}
		crossOrigin = true // a different origin always needs CORS
	default:
		return learn.Hint{}, fmt.Errorf("%q must start with / or with http(s)://", u)
	}

	rel := r.Rel
	switch rel {
	case "", "preload":
		rel = ""
	case "preconnect", "dns-prefetch":
		if r.CrossOrigin != nil {
			crossOrigin = *r.CrossOrigin
		}
		return learn.Hint{URL: u, Rel: r.Rel, CrossOrigin: crossOrigin, Manual: true}, nil
	default:
		return learn.Hint{}, fmt.Errorf("%q: rel must be preload, preconnect or dns-prefetch", r.Rel)
	}

	as := r.As
	if as == "" {
		as = asFromExtension(u)
	}
	if as == "" {
		return learn.Hint{}, fmt.Errorf("%q: cannot infer the type, set \"as\" explicitly", u)
	}
	if !knownAs[as] {
		return learn.Hint{}, fmt.Errorf("%q: unknown type %q", u, as)
	}
	if as == "font" {
		crossOrigin = true
	}
	if r.CrossOrigin != nil {
		crossOrigin = *r.CrossOrigin
	}
	return learn.Hint{URL: u, As: as, CrossOrigin: crossOrigin, Manual: true}, nil
}

// asFromExtension guesses the preload type from the file extension,
// which is right for the resources anybody actually lists by hand.
func asFromExtension(u string) string {
	clean := u
	if i := strings.IndexAny(clean, "?#"); i >= 0 {
		clean = clean[:i]
	}
	switch strings.ToLower(path.Ext(clean)) {
	case ".css":
		return "style"
	case ".js", ".mjs":
		return "script"
	case ".woff", ".woff2", ".ttf", ".otf", ".eot":
		return "font"
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".svg", ".ico":
		return "image"
	case ".json":
		return "fetch"
	}
	return ""
}
