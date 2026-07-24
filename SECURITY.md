# Security Policy

rukh sits on the public edge of a server, in front of nginx: security
reports are taken seriously.

## Reporting a vulnerability

Please do NOT open a public issue. Report privately to
**ostap.mykhaylyak@gmail.com** with a description, reproduction steps
and the affected version (`rukh --version`). You will get an
acknowledgment as soon as possible.

## Design notes relevant to security

- rukh exposes no management API and no network endpoint of its own:
  everything it receives is proxied to nginx. The only local interface
  is the read-only status Unix socket under `/run/rukh`.
- Client-supplied forwarding headers are never trusted unless the peer
  is listed in `realip.trusted_proxies`; `X-Forwarded-For` is rewritten
  on every request, and the internal `X-Rukh-Preload` marker is
  stripped from inbound requests.
- Learned paths are treated as untrusted input: they are length-capped
  and escaped before ending up in a `Link` header.
- The traffic model is bounded by explicit caps and a periodic sweep;
  it cannot be grown without limit by crafted requests.
- Requests carrying `Authorization`, responses setting cookies and
  responses marked `no-store`/`private` are excluded from the model, so
  a personalized page is never warmed up or announced.
- The daemon is expected to run under the shipped systemd hardening.
