# Concurrency: per-host limits, AIMD, circuit breaker

How a worker stays polite under load. Most of this is automatic
via `worker.Serve`; the manifest exposes the knobs operators tune.

## `concurrency_per_host`

Manifest field, declared in the phase block:

```yaml
phases:
  - name: http-fingerprint
    concurrency_per_host: 4   # max in-flight Run() against the same host
```

The runtime tracks in-flight `Run` invocations grouped by
`Asset.Attrs["host"]` (or `Asset.Value` for kind=host). When the
limit is reached, additional jobs queue up in River until a slot
frees - they don't get rejected, they just wait.

Default if unset: 1 (one in-flight per host). Conservative on
purpose; raise if your worker is read-only (DNS lookups, passive
sources) or your target tolerates concurrent connections (CDNs).

Don't confuse this with worker-level concurrency: River dispatches up
to `--workers <N>` jobs across all hosts. The per-host cap layers on
top.

## AIMD adaptive concurrency

`worker-sdk/sdk/dialer` (a future package; not yet shipped, the
dialer is currently part of `httpcache`) implements additive-increase /
multiplicative-decrease on the per-host concurrency:

- Increment by 1 every N successful responses.
- Halve on every 5xx / connect failure / TLS error.
- Floor at 1, ceiling at the manifest-declared `concurrency_per_host`.

Result: a worker that finds a fast host pushes more concurrency
without exceeding the manifest cap, while a flaky host gets backed
off automatically.

If you don't want AIMD (e.g. a passive-source worker that should
always run at max concurrency): set the AIMD floor == ceiling
in your manifest config. The runtime treats matching values as
"static concurrency, no adaptive tuning".

## Circuit breaker

Per-host failure counter in `worker/runtime.go`. Fires when a host
returns errors at >50% rate over a 60s window:

- The breaker opens; subsequent jobs for that host fail-fast
  with `ErrHostCircuitOpen` instead of attempting the call.
- Half-open after 30s; one probe job runs; success closes the
  breaker, failure re-opens it for another 60s.

Workers see the circuit-open error and should treat it as a soft
failure; don't retry the same host immediately, return a `Result`
with `Stats{Skipped: 1}` instead.

The breaker is per-process; scaling out (more replicas) gets
independent breakers per replica. This is intentional: a noisy host
on one replica's network path may be fine on another.

## DNS-side concurrency (separate)

dns-service has its own per-resolver `qps_limit` from
`tm_dns_resolvers` (one of the cluster_settings tables). That's
upstream-resolver-side, independent of the worker's per-host cap.

If a worker's per-host cap is high but dns-service's `qps_limit` is
low, the bottleneck is dns-service. Tune both knobs together if you're
running a high-throughput scope.

## Patterns to follow

- **Pure-data workers** (tm-vulnx, pure embedded DB lookup):
  `concurrency_per_host: <high>`, zero outbound network, can run
  at max worker concurrency.
- **Passive-source workers** (tm-gau, tm-urlfinder, external API
  calls): `concurrency_per_host: 2-4`, AIMD enabled. The external
  rate limit dominates anyway.
- **Active-scan workers** (tm-portscan, tm-httpx): default = 4,
  AIMD enabled, circuit breaker active. The target IS the host,
  don't hammer it.
- **Browser-driven workers** (techmapper-worker with Camoufox):
  `concurrency_per_host: 1`. Headful browsers don't parallelize
  per-host meaningfully.
