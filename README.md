# rukh

A learning reverse proxy that sits in front of nginx on the same
server, watches how visitors actually use the site, and uses what it
learns to make the site faster — automatically.

rukh takes over ports 80 and 443, terminates TLS **with the
certificates nginx already has** (it reads them out of the nginx
configuration, nothing is configured twice), forwards everything to
nginx on a loopback port, and while doing so:

- learns which pages exist and which static resources each page pulls
  in, then announces those resources with **HTTP 103 Early Hints**, so
  the browser starts downloading the CSS while nginx is still building
  the HTML;
- learns where visitors go next and advertises the likely next page as
  a **prefetch** hint;
- keeps the important pages hot with a **cache preloader** that
  re-requests them from nginx, prioritizing pages that are popular,
  recent or slow;
- forgets: every counter decays, so the model always describes traffic
  as it is now, not as it was last week.

Everything lives in memory. No database, no persistence, no dashboard,
no metrics export, no CMS integration: it works with any HTTP
application, WordPress or not.

Single static binary, no runtime dependencies, managed by systemd.

## How it fits with nginx

```
        :80/:443                     127.0.0.1:8080
client ──────────►  rukh  ──────────────────────────►  nginx  ──► PHP/app
                     │  TLS with nginx's certificates
                     │  X-Forwarded-For / X-Real-IP
                     └─ 103 Early Hints, prefetch hints, cache warm-up
```

nginx keeps doing everything it does today — virtual hosts, rewrites,
FastCGI cache, static files. rukh never routes: it forwards the
original `Host` and lets nginx decide, so adding a site to nginx adds
it to rukh too.

## Install

```bash
tar xzf rukh-v0.1.0-linux-amd64.tar.gz
cd rukh-v0.1.0-linux-amd64
sudo ./rukh --init
```

`--init` installs the binary in `/usr/sbin/rukh`, the default config in
`/etc/rukh/config.yaml`, the systemd unit and the logrotate policy. It
never overwrites an existing configuration, so it is also the upgrade
path.

Then move nginx off the public ports — this is the only change nginx
needs:

```nginx
server {
-   listen 80;
-   listen [::]:80;
+   listen 127.0.0.1:8080;
    server_name example.com;
    ...
}

server {
-   listen 443 ssl;
-   listen [::]:443 ssl;
+   listen 127.0.0.1:8443 ssl;      # or drop TLS here: rukh terminates it
    server_name example.com;
    ssl_certificate     /etc/letsencrypt/live/example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/example.com/privkey.pem;
    ...
}
```

> **The port has to change, not just the address.** Moving nginx to
> `127.0.0.1:80` is not enough: a wildcard bind (rukh's default `:80`)
> collides with a specific bind on the same port, and rukh would fail
> to start with "address already in use". Either give nginx a different
> port, as above, or use the alternative layout below.

and let nginx trust the local proxy, so logs and PHP keep seeing the
real client address (in the `http` block):

```nginx
set_real_ip_from 127.0.0.1;
real_ip_header   X-Forwarded-For;
```

Finally:

```bash
nginx -t && systemctl reload nginx
systemctl daemon-reload
systemctl enable --now rukh
rukh status
```

`rukh --check-config` prints what rukh discovered — server blocks,
certificates, the upstream it picked — without touching anything. Run
it before starting the service if you want to see the plan first.

### Simplest possible setup

Keep **one** plain loopback listener per site in nginx
(`listen 127.0.0.1:8080;`), delete the `443` server blocks, and leave
the `ssl_certificate` directives where they are: rukh reads them for
TLS termination and nginx no longer has to. HTTP/2 for visitors comes
from rukh.

### Alternative: keep nginx's ports, bind rukh to the public address

If you would rather not touch the `listen` ports at all, do the
opposite: leave nginx on `127.0.0.1:80` and `127.0.0.1:443`, and tell
rukh to bind only the public address, which does not collide with
loopback:

```yaml
server:
  http: "203.0.113.5:80"      # your public IPv4
  https: "203.0.113.5:443"
```

rukh then auto-detects nginx on `127.0.0.1:80`. The trade-off is that
the address is now hard-coded: on a machine whose IP changes, or with
IPv6 to serve as well, the port move is the simpler setup — the
default `:80`/`:443` binds every address of both families.

### Certificate renewals

Nothing changes. certbot (or whatever issues them) keeps writing the
same files; rukh re-checks their mtime every 30 seconds and swaps the
certificate with no downtime and no reload. If certbot uses the
`webroot` or `--nginx` plugin, the ACME challenge is proxied through to
nginx like any other request.

## Commands

Service verbs are bare words; everything else takes a leading `--`.

| Command | What it does |
| --- | --- |
| `rukh start` | run the daemon in the foreground (what systemd does) |
| `rukh stop` | ask the running daemon to shut down |
| `rukh reload` | re-read the config, re-scan nginx, reopen the logs |
| `rukh restart` | stop the running daemon, then start it again |
| `rukh status` | ask the running daemon what it is doing |
| `rukh status --watch 2s` | live view, like `top` |
| `rukh status --json` | stable JSON, for monitoring |
| `rukh --init` | install layout, binary, unit, logrotate |
| `rukh --purge` | remove config, logs and the binary (asks first) |
| `rukh --check-config` | parse config, nginx setup and hints files, and report |
| `rukh --version` | print the version |

`rukh status` exits with the Nagios convention: `0` OK, `1` WARNING,
`2` CRITICAL, `3` UNKNOWN — drop-in for any monitor.

```
$ rukh status
rukh OK - active, pid 2417, uptime 6h12m, config valid, 3 site(s) -> 127.0.0.1:8080,
412 page(s) learned, 918233 requests, 21044 hinted
```

## What it learns, and how

**Pages.** Every successful HTML `GET` is a page view, keyed by
host + path. Tracking parameters (`utm_*`, `fbclid`, `gclid`, …) are
stripped, so the same page seen through ten campaigns stays one entry.

**Resources.** A subresource request carries a `Referer` (and a
`Sec-Fetch-Dest`); that is what ties `/style.css` to the page that
pulled it in. When the browser strips the referrer, rukh falls back to
the last page that client navigated to, within a short window.

**Which resources are worth announcing.** Confidence is measured
against the most requested resource of the same page, not against page
views: browsers cache subresources, so a returning visitor loads the
HTML without re-fetching the CSS, and comparing resources with each
other cancels that bias out. A resource is announced only if it is
requested reliably (`hints.min_confidence`), often enough to be trusted
(`hints.min_samples`) and recently (`hints.max_age`). Stylesheets go
first, then fonts (always with `crossorigin`), then scripts.

**When traffic cannot teach: manual hints.** Behind a CDN that serves
static files from its edge — Cloudflare being the obvious case — those
requests never reach this server, so there is nothing to learn from:
the HTML still arrives, the CSS does not. Write the answer down
instead, one file per host in `/etc/rukh/hints`, named after the host:

```yaml
# /etc/rukh/hints/example.com.yaml
hosts: [example.com, www.example.com]   # optional, defaults to the file name

default:                                 # every page of this host
  - /wp-content/themes/mytheme/style.css # type inferred from the extension
  - url: /wp-content/themes/mytheme/inter.woff2
    as: font                             # fonts always get crossorigin
  - url: https://cdn.example.net
    rel: preconnect                      # open a third-party connection early

paths:                                   # added on top of the defaults
  "/":
    - /wp-content/uploads/hero.webp
  "/product/*":                          # * matches by prefix
    - /wp-content/plugins/woocommerce/assets/css/woocommerce.css
```

The files are watched: saving one applies it immediately. Manual
entries are always sent and come first — they are a decision, not a
guess — and the learned ones fill the remaining slots up to
`hints.max_links`. A file that stops parsing keeps serving its last
good version, and a single invalid entry is skipped with a warning
rather than costing the whole file. `rukh --check-config` lists what
was loaded.

**Navigation paths.** A navigation whose referrer is another page of
the same host is a transition. When one destination dominates
(`prefetch.min_probability`), it is advertised on the page response as
`Link: </next>; rel=prefetch`.

**rukh never learns from its own suggestions.** A prefetch the browser
performs on rukh's advice comes back as an ordinary GET carrying the
suggesting page as its Referer; counting it would make every
prediction confirm itself and climb to certainty on its own. Requests
marked `Sec-Purpose: prefetch` (and the older spellings) are therefore
excluded from the model entirely, they receive no hints of their own,
and no page ever suggests going back to the page the visitor just came
from. Speculative requests are flagged in the access log
(`"speculative": true`) so their share is visible.

**Decay.** Every counter is exponentially decayed with a configurable
half-life (default 6h): recent traffic always dominates, and anything
nobody touches fades to zero and is dropped by the sweep. That is also
the memory bound, together with the explicit caps
(`learn.max_pages_per_host` and friends).

**Nothing is written to disk.** After a restart the model is rebuilt
from live traffic within minutes.

## The cache preloader

Every 30 seconds rukh warms a few pages straight from nginx, so the
answer is already cached when the next visitor asks. It is designed to
never waste a request:

- pages are ranked by frequency, recency and origin latency — a slow,
  popular page is warmed first;
- each page gets its own refresh cadence from its rank, between
  `preload.min_refresh` and `preload.max_refresh`;
- a page a real visitor just loaded is already warm and is skipped;
- personalized pages (`Set-Cookie`, `no-store`, `private`,
  `Vary: Cookie`) and anything that did not answer `200` with HTML are
  never warmed;
- a hard budget (`preload.max_per_minute`, default 30) means warm-up
  can never compete with real traffic;
- if the backend starts failing, the preloader backs off exponentially.

Warm-up requests carry `X-Rukh-Preload: 1` (stripped from inbound
requests, so a client cannot fake it) and a distinctive `User-Agent`,
which makes them easy to exclude from analytics.

## Configuration

`/etc/rukh/config.yaml` is optional: every setting has a production
default and an empty file works. The shipped file documents all of
them; the ones worth knowing:

```yaml
backend:
  address: ""              # empty = auto-detect nginx's loopback listener

nginx:
  config: "/etc/nginx/nginx.conf"

learn:
  half_life: "6h"          # how fast the model forgets

hints:
  min_confidence: 0.6      # how reliably a resource must follow the page
  max_links: 10

prefetch:
  min_probability: 0.35

preload:
  max_per_minute: 30       # the warm-up budget
```

The file is watched: saving it applies the change. `rukh reload` (or
`systemctl reload rukh`) does the same and re-scans nginx.

Put a CDN in front? List it in `realip.trusted_proxies` and rukh will
take the client address from `X-Forwarded-For` instead of the peer —
otherwise the header is ignored, so nobody can forge an address.

## Logs

`/var/log/rukh/`, JSON, one stream per concern:

| File | Contents |
| --- | --- |
| `rukh.log` | startup, reloads, nginx discovery, upstream errors |
| `access.log` | one line per proxied request (`log.access: false` to disable) |
| `learn.log` | early-hint and preload decisions |

Each access line carries `duration_ms` (the whole request, including
sending the body to the client), `origin_ms` (nginx's time to first
byte — what the origin is actually responsible for) and `kind`
(`page`, `style`, `script`, `font`, `image`, `other`), so "what is
slow, and is it us or the visitor's connection" is one query:

```bash
# the 20 slowest responses, by the origin's own time
jq -r 'select(.msg=="request") | [.origin_ms, .duration_ms, .kind, .status, .path] | @tsv'   /var/log/rukh/access.log | sort -rn | head -20
```

Rotation is logrotate's job (policy installed by `--init`); SIGHUP
reopens the files.

## rukh has to be the outermost hop

Early Hints are the one thing that cannot survive a proxy in front:
**no version of nginx forwards a 103 from its upstream** — it has
never implemented Early Hints, in either direction — and most other
front proxies drop informational responses too. Put anything between
rukh and the visitor and the 103 dies there.

What does survive is the `Link` header on the final response, which
rukh sends as well: the browser still starts those downloads when the
HTML headers arrive, instead of when the parser reaches the tag. It is
a smaller win — the "early" part, the head start while the origin is
still generating the page, is what is lost.

One front proxy is the exception worth knowing: **Cloudflare rebuilds
the 103 at its edge** from exactly those `Link` headers, when Early
Hints is enabled in its dashboard. Behind Cloudflare the visitor gets
a real 103 again, generated one hop closer to them than rukh could.

Two settings matter when something is in front:

- `realip.trusted_proxies` must list it, or every request appears to
  come from the same address and the traffic model treats all visitors
  as one;
- `server.http3: false`, because the Alt-Svc advertisement would name a
  port the front proxy does not serve over QUIC. rukh warns about this
  combination.

## Protocols and overhead

**Visitors get the best protocol their client supports.** The TLS
listener advertises `h3` and `h2` over ALPN and QUIC runs on the same
port over UDP, so a current browser gets HTTP/3, an older one HTTP/2,
and a client that only speaks HTTP/1.1 falls back cleanly. Early Hints
travel over all three. Plain `:80` is HTTP/1.1 by definition — browsers
never negotiate h2 without TLS — so a request logged with
`"scheme":"http"` and `"proto":"HTTP/1.1"` is simply a visitor who
arrived on port 80.

HTTP/3 needs two things from the host: **UDP 443 open** on the firewall,
and larger UDP buffers, which `--init` prints as a step:

```bash
echo 'net.core.rmem_max=7500000' > /etc/sysctl.d/99-rukh.conf
echo 'net.core.wmem_max=7500000' >> /etc/sysctl.d/99-rukh.conf
sysctl --system
```

Failing to bind UDP never stops the daemon: HTTP/2 keeps serving, the
`Alt-Svc` advertisement is simply not sent, and `rukh status` reports
`http3` as not listening. Turn it off with `server.http3: false`.

**The hop to nginx is HTTP/1.1 with keep-alive, on purpose.** That is
the fastest option for a loopback hop, and the only one nginx accepts
upstream anyway (`proxy_pass` itself speaks HTTP/1.1). HTTP/2 would
multiplex everything onto a single TCP connection and reintroduce
head-of-line blocking where there is none today; the connection pool
(256 idle connections per host) already removes the handshake. A test
asserts that twenty requests reuse one connection.

**What rukh costs per request.** Measured with `go test -bench`, on one
core: the whole rukh layer — resolving the client address, classifying
the response, the hint lookup, the observation and the access line —
is **~7 µs**, of which ~4.4 µs is writing the JSON access line. Most of
that happens *after* the response has been written to the client, so it
is not even inside the `duration_ms` the log reports; what is inside is
the hint lookup, around a microsecond.

That is why `duration_ms - origin_ms` is not rukh's overhead. `origin_ms`
stops when nginx's response *headers* arrive; the remainder is reading
the body from nginx and writing it out to the visitor, which every
proxy has to do — nginx included, when it proxies to PHP. On a request
answered in 1 ms by the origin and taking 1.2 ms in total, the 0.2 ms
is bytes moving, not bookkeeping.

The access log is buffered (32 KiB, flushed every 250 ms) precisely so
it does not cost a write syscall per request; the trade-off is that a
`kill -9` can lose up to 250 ms of access lines. The service and learn
logs are unbuffered. `log.access: false` removes the cost entirely.

## Notes and limits

- Browsers act on `103` over HTTP/2 and HTTP/3. rukh does not send it
  over HTTP/1.1 unless `hints.http1: true`; some old intermediaries
  mishandle informational responses.
- Only same-origin resources are learnable: rukh sees what passes
  through it, and does not parse HTML.
- A site that sends `Referrer-Policy: no-referrer` limits the
  attribution to the client fallback described above.
- rukh does not cache responses itself. Caching stays in nginx (or the
  application); rukh only makes sure the right things are in that cache
  and that the browser starts fetching earlier.

## Uninstall

```bash
systemctl disable --now rukh
sudo rukh --purge          # removes config, logs and the binary
```

Then put nginx back on `:80`/`:443` and reload it. rukh never touches
the nginx configuration, so that is a two-line revert.

## License

MIT — see [LICENSE](LICENSE).
