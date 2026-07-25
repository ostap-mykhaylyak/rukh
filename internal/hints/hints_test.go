package hints

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ostap-mykhaylyak/rukh/internal/learn"
)

const sample = `
hosts:
  - example.com
  - www.example.com

default:
  - /theme/style.css
  - /theme/app.js
  - url: /theme/inter.woff2
    as: font
  - url: https://cdn.example.net
    rel: preconnect

paths:
  "/":
    - /uploads/hero.webp
  "/product/*":
    - url: /plugins/single-product.js
      as: script
  "/product/special":
    - /plugins/special.css
`

func load(t *testing.T, body string) *Host {
	t.Helper()
	h, warnings, err := Parse("example.com", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	return h
}

func urls(hs []learn.Hint) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.URL)
	}
	return out
}

func TestTypesAreInferredFromTheExtension(t *testing.T) {
	h := load(t, sample)
	byURL := map[string]learn.Hint{}
	for _, e := range h.Default {
		byURL[e.URL] = e
	}
	if got := byURL["/theme/style.css"]; got.As != "style" {
		t.Errorf("style.css: as = %q", got.As)
	}
	if got := byURL["/theme/app.js"]; got.As != "script" {
		t.Errorf("app.js: as = %q", got.As)
	}
	// A font is fetched in CORS mode even same-origin: without the
	// attribute the browser downloads it twice.
	if got := byURL["/theme/inter.woff2"]; got.As != "font" || !got.CrossOrigin {
		t.Errorf("font: %+v, want as=font with crossorigin", got)
	}
	if got := byURL["https://cdn.example.net"]; got.Rel != "preconnect" || !got.CrossOrigin {
		t.Errorf("preconnect: %+v", got)
	}
	for _, e := range h.Default {
		if !e.Manual {
			t.Errorf("%s: manual hints must be marked as such", e.URL)
		}
	}
}

func TestHostsComeFromTheFileOrItsName(t *testing.T) {
	if got := load(t, sample).Hosts; len(got) != 2 || got[0] != "example.com" {
		t.Fatalf("hosts = %v", got)
	}
	if got := load(t, "default: [/a.css]").Hosts; len(got) != 1 || got[0] != "example.com" {
		t.Fatalf("hosts = %v, want the file name", got)
	}
}

func TestLookupMergesDefaultsWithTheMostSpecificRule(t *testing.T) {
	h := load(t, sample)

	// A page with no rule of its own gets the defaults only.
	if got := urls(h.Lookup("/about")); len(got) != 4 {
		t.Fatalf("/about = %v, want the four defaults", got)
	}
	// Exact match.
	if got := urls(h.Lookup("/")); len(got) != 5 || got[4] != "/uploads/hero.webp" {
		t.Fatalf("/ = %v", got)
	}
	// Prefix match.
	got := urls(h.Lookup("/product/vespa-px-125"))
	if len(got) != 5 || got[4] != "/plugins/single-product.js" {
		t.Fatalf("/product/... = %v", got)
	}
	// An exact rule wins over the prefix one that also matches.
	got = urls(h.Lookup("/product/special"))
	if len(got) != 5 || got[4] != "/plugins/special.css" {
		t.Fatalf("/product/special = %v, want the exact rule", got)
	}
}

func TestInvalidEntriesAreSkippedNotFatal(t *testing.T) {
	h, warnings, err := Parse("example.com", []byte(`
default:
  - /good.css
  - /mystery
  - url: /thing.bin
    as: nonsense
  - url: /api/data
    as: fetch
  - url: "ftp://example.net/x"
`))
	if err != nil {
		t.Fatalf("a bad entry must not fail the file: %v", err)
	}
	if got := urls(h.Default); len(got) != 2 || got[0] != "/good.css" || got[1] != "/api/data" {
		t.Fatalf("kept = %v, want only the valid entries", got)
	}
	if len(warnings) != 3 {
		t.Fatalf("warnings = %v, want one per skipped entry", warnings)
	}
	for _, w := range warnings {
		if !strings.Contains(w, "example.com") {
			t.Errorf("warning should name the file: %q", w)
		}
	}
}

func TestBrokenYAMLIsAnError(t *testing.T) {
	if _, _, err := Parse("x", []byte("default: [oops")); err == nil {
		t.Fatal("expected a parse error")
	}
}

func newStore(t *testing.T, files map[string]string) *Store {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := NewStore(dir, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	s.LoadAll()
	return s
}

func TestStoreServesPerHostFiles(t *testing.T) {
	s := newStore(t, map[string]string{
		"example.com.yaml": sample,
		"other.test.yaml":  "default: [/other.css]",
		"README.example":   "not a hints file",
		"wild.yaml":        "hosts: [\"*.shop.test\"]\ndefault: [/wild.css]",
	})

	if got := urls(s.Lookup("example.com", "/")); len(got) != 5 {
		t.Errorf("example.com/ = %v", got)
	}
	// The file declares both the apex and the www alias.
	if got := urls(s.Lookup("www.example.com", "/about")); len(got) != 4 {
		t.Errorf("www.example.com = %v", got)
	}
	if got := urls(s.Lookup("other.test", "/")); len(got) != 1 || got[0] != "/other.css" {
		t.Errorf("other.test = %v", got)
	}
	// A wildcard host covers its subdomains.
	if got := urls(s.Lookup("it.shop.test", "/")); len(got) != 1 || got[0] != "/wild.css" {
		t.Errorf("it.shop.test = %v", got)
	}
	// An unknown host has no manual hints, and the .example file is
	// ignored: it is documentation, not configuration.
	if got := s.Lookup("nothing.test", "/"); got != nil {
		t.Errorf("nothing.test = %v", got)
	}
	if s.Count() != 4 {
		t.Errorf("hosts covered = %d, want 4", s.Count())
	}
}

func TestBrokenFileKeepsServingTheLastGoodVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "example.com.yaml")
	os.WriteFile(path, []byte("default: [/good.css]"), 0o644)

	s := NewStore(dir, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	s.LoadAll()
	if got := urls(s.Lookup("example.com", "/")); len(got) != 1 {
		t.Fatalf("initial load = %v", got)
	}

	os.WriteFile(path, []byte("default: [oops"), 0o644)
	s.LoadAll()
	if got := urls(s.Lookup("example.com", "/")); len(got) != 1 || got[0] != "/good.css" {
		t.Fatalf("after a broken edit = %v, want the last good version", got)
	}
	info := s.Snapshot()
	if len(info) != 1 || info[0].Error == "" {
		t.Fatalf("the failure must be visible in the status: %+v", info)
	}
}

func TestRenderedLinkHeaders(t *testing.T) {
	h := load(t, sample)
	links := learn.LinkPreload(h.Lookup("/"))
	want := []string{
		"</theme/style.css>; rel=preload; as=style",
		"</theme/app.js>; rel=preload; as=script",
		"</theme/inter.woff2>; rel=preload; as=font; crossorigin",
		"<https://cdn.example.net>; rel=preconnect; crossorigin",
		"</uploads/hero.webp>; rel=preload; as=image",
	}
	if len(links) != len(want) {
		t.Fatalf("links = %v", links)
	}
	for i := range want {
		if links[i] != want[i] {
			t.Errorf("link %d = %q, want %q", i, links[i], want[i])
		}
	}
}
