# Asset shape reference

The `worker.Asset` type the SDK passes to `Tool.Run`. Each asset is
one row in the `assets` table, owned by a scope, identified by
`(scope_id, kind, value)`.

## Kinds

The cascade engine routes by `Asset.Kind` to the phases that
declared it in `consumes.kinds`. Today's kinds:

| Kind        | Value example                  | Emitted by                                    |
|-------------|--------------------------------|-----------------------------------------------|
| `wildcard`  | `*.acme.com`                   | scope ingest (`scope.includes` parser)        |
| `subdomain` | `api.acme.com`                 | tm-subfind, tm-tldfind, tm-ip2vhost, tm-ctwatch |
| `host`      | `api.acme.com` (resolved)      | tm-resolve                                    |
| `port`      | `443/tcp`                      | tm-portscan, tm-masscan                       |
| `url`       | `https://api.acme.com/v1`      | tm-httpx, tm-urlfinder, tm-gau, tm-jsfinder   |

A new kind is rare. If your worker emits one:

1. Pick a stable lowercase identifier matching `nameRe`
   (`^[a-z][a-z0-9]*([-_][a-z0-9]+)*$`).
2. Document it here in the table above before merging.
3. Other workers' `consumes.kinds` must explicitly list it · the
   cascade routes by exact-match.

## Attrs

`Asset.Attrs` is `map[string]any` · kind-specific metadata. Free-form
on purpose · each worker contributes the keys it cares about, the
cascade filter language reads them server-side.

### Common attrs by kind

#### `subdomain` / `host`

- `ip` (string) · primary A record · `tm-resolve` writes
- `ipv6` (string) · primary AAAA · `tm-resolve` writes
- `cname` (string) · canonical alias if any · `tm-resolve` writes
- `asn` (int) · `tm-resolve` writes via asnmap inline
- `asn_org` (string) · org name · `tm-resolve` writes
- `cdn` (string) · cloudfront / cloudflare / fastly / akamai / … ·
  populated by techmapper-worker via cdncheck of cached records

#### `port`

- `tcp_port` (int) · the port number · `tm-portscan` / `tm-masscan` write
- `host` (string) · parent host's value (denormalized for fast filter)
- `ip` (string) · parent host's IP (denormalized)
- `tls` (bool) · TLS detected on connect · `tm-portscan` writes
- `tls.cert_fingerprint` / `tls.ja3` / `tls.sans` / `tls.expiry` /
  `tls.issuer` · `tm-tlsx` writes when running

#### `url`

- `host` (string) · denormalized for filter
- `tcp_port` (int) · denormalized
- `status_code` (int) · last fetch · `tm-httpx` writes
- `title` (string) · `<title>` from HTML body · `tm-httpx` writes
- `content_type` (string) · `tm-httpx` writes
- `content_length` (int) · `tm-httpx` writes
- `body_sha256` (string) · canonical hash for body cache lookup
- `technologies` ([]map) · wapp matches · `techmapper-worker` writes
- `findings` ([]map) · per-element finding objects · see FINDINGS.md
- `redirect_chain` ([]map) · `tm-httpx` writes

### Filter language gotchas

The cascade filter (`consumes.filter`) reads attrs server-side via
PG JSONB operators · grammar lives in `controlplane/internal/cascade`.
Keep keys flat or one level deep · `attrs.tls.ja3` works, `attrs.foo.bar.baz`
gets brittle on the parser side.

JSONB round-trips in PG can stringify numbers (`attrs.tcp_port` may
arrive as either `443` or `"443"` depending on path). Workers reading
attrs in Go should tolerate both shapes · see `tm-httpx`'s `numAttr`
helper for the canonical pattern (int / int64 / float64 / string-of-int
type-switch fallback).

## Identity & dedup

- An asset is uniquely keyed by `(scope_id, kind, value)`.
- `value` is canonicalized at write time · lowercase host, normalized
  URL, no trailing dot. The runtime in `worker/asset_writer.go` does
  this before INSERT so re-discovery collapses on the same row.
- `value` for wildcard SANs (`*.example.com`) is preserved as-is ·
  the subdomain space avoids them via `normalizeHost` returning empty
  on `*.` prefix (see `tm-ip2vhost`).
