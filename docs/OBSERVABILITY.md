# Observability conventions

What every reconmesh worker exposes for free, and what your `Run`
function should add when it does something interesting.

## What the SDK gives you

`worker.Serve(t)` mounts `/metrics` + `/healthz` on the admin port
(default `:9090`, override via `ADMIN_ADDR=:9091`).

### Free metrics

The SDK registers these on the Prometheus default registry. Scrape
them via `recon-platform/deploy/prometheus.yml` (already wired):

- `recon_worker_jobs_total{tool, phase, outcome}`: counter, every
  Run() invocation. `outcome` is `success` / `error` / `timeout` /
  `circuit_open`.
- `recon_worker_run_duration_seconds{tool, phase}`: histogram, Run()
  wall-clock duration.
- `recon_worker_assets_emitted_total{tool, phase, kind}`: counter,
  one per `Result.NewAssets[i]`.
- `recon_worker_findings_emitted_total{tool, phase, kind, severity}`:
  counter, one per `Result.Findings[i]`.
- `recon_worker_dns_lookups_total{tool, outcome}`: counter, the
  shared `sdk/dns` package increments this when used.
- `recon_worker_http_requests_total{tool, status_class}`: counter,
  the shared `sdk/httpcache` increments this on every fetch (cache
  hit and miss both counted separately via `outcome`).

`/healthz` returns `200 OK` when the worker is past the boot phase
and inside `River.Subscribe`. Returns 503 during PG connect failure
or River init fail.

### Tracing

Every `Run()` is wrapped in an OpenTelemetry span when
`OTEL_EXPORTER_OTLP_ENDPOINT` is set. Span name = `<tool>.run`,
attributes:

- `worker.tool`
- `worker.phase`
- `asset.kind`
- `asset.scope_id`
- `asset.id`
- `result.new_assets_count`
- `result.findings_count`

Errors are recorded via `span.RecordError(err)`. Timeouts get
`status=Timeout`.

For child spans inside `Run()` (e.g. one span per outbound HTTP),
use `otel.Tracer("reconmesh.worker").Start(ctx, "<descriptive name>")`.
The cascade engine in controlplane is the parent of the run span;
the controlplane API is the parent of the cascade dispatch.

## What you should add

When your `Run()` does something non-trivial, surface it via:

### Custom metrics

Register at init() in `cmd/main.go`:

```go
var myCounter = prometheus.NewCounterVec(
    prometheus.CounterOpts{
        Name: "tm_mytool_<thing>_total",
        Help: "<one-line description>",
    },
    []string{"<low-cardinality label>"},
)

func init() {
    prometheus.MustRegister(myCounter)
}
```

Cardinality budget: keep label values < 100 distinct. Don't label
by host, asset_id, URL; those explode the cardinality.

### Structured log lines

The runtime injects `slog.Default()` with attributes:

- `tool`, `phase`, `scope_id`, `asset_id`, `run_id`

Workers should log via `slog.InfoContext(ctx, ...)` (not `slog.Info`)
so the run-scoped attributes propagate. Examples:

```go
slog.InfoContext(ctx, "starting NVD live lookup",
    "cve", cveID, "api", "nvd")
```

`info` level by default; raise to `debug` for verbose internals.
`warn` for soft failures (rate limit, parse error on bad input).
`error` for failures the operator should see.

### What NOT to log

- Secrets: API keys, tokens, passwords. The runtime's slog handler
  doesn't redact for you.
- Full HTTP bodies: they go in `tm_http_bodies` cache, log a hash
  reference instead.
- Per-byte loop progress: drowns signal. Log start/end + summary stats.

## Dashboards

`recon-platform/deploy/grafana/dashboards/reconmesh-overview.json`
auto-imports on `make up-stats`. Shows:

- Cluster job-rate (jobs/sec across all workers)
- Per-tool error rate (red badges when >5%)
- Asset growth (per-scope, last 7d)
- Finding counts by severity
- River queue depth per phase

Adding a new panel: edit the JSON in-tree, restart `grafana` container,
provisioning re-imports.
