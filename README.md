# worker-sdk

Shared Go library every parabellum worker links against. Carries the wire
contract between the control plane and the workers, plus the runtime
plumbing every worker would otherwise re-implement: River subscription,
Postgres pool, OpenTelemetry tracing, Prometheus metrics, finding
deduplication, graceful shutdown.

The SDK's surface is intentionally tiny: three interfaces, a handful of
types, one `Serve` entry point. New tools implement `Tool.Run` and call
`worker.Serve(t)`. That's it. Everything else is hidden.

## Versioning

`worker-sdk` is the wire contract. Breaking changes cost ~one PR per
worker, so they are handled carefully:

- MINOR / PATCH bumps preserve full backward compatibility.
- MAJOR bumps go through a deprecation cycle of at least one MINOR before
  the old API is removed.
- The SDK ships its own `Version` constant; the control plane reads each
  worker's `manifest.sdk_version` to assert compatibility.

See `../platform/docs/VERSIONING.md` for the org-wide policy.

## Layout

```
worker-sdk/
├── worker/                # the public package; what `import` consumers use
│   ├── tool.go            # Tool interface, Job, Result, Finding, Asset
│   ├── manifest.go        # YAML manifest schema + load/validate (incl. Description)
│   ├── jobargs.go         # CascadeArgs wire contract (matches controlplane/jobtype)
│   ├── serve.go           # Serve() entry point: River + signal handling
│   ├── runtime.go         # PG pool, metrics, OTel, config reload
│   ├── river_adapter.go   # River JobArgs binding + worker registration
│   ├── asset_writer.go    # UpsertAsset + sync.Pool fingerprint
│   ├── dedup.go           # finding hash canonicalization
│   └── once.go            # --once / --asset synthetic-job mode (no DB, no River)
├── sdk/                   # opt-in helpers; workers import only what they need
│   ├── mtls/              # cleanhttp-style http.Client with mTLS roots
│   ├── httpcache/         # cluster body cache + SourceCache
│   ├── dns/               # dns-service HTTP client wrapper + local fallback
│   ├── secretbox/         # AES-256-GCM decrypt (read-only by design)
│   ├── metrics/           # shared prometheus collectors for the worker side
│   └── tracing/           # OTLP exporter helpers
├── docs/                  # ASSETS / FINDINGS / IDEMPOTENCE / CONCURRENCY / MANIFEST
├── grafana/               # ready-to-import dashboards for worker-side metrics
└── CHANGELOG.md
```

The `consumes.filter` DSL is parsed inside the controlplane (cascade
engine), not the SDK. Workers receive jobs that already match the
filter. The SDK only ships the *manifest* type that declares it.

## Quick example

```go
package main

import (
    "context"

    "git.vozec.fr/Parabellum/worker-sdk/worker"
)

type MyTool struct{}

func (t *MyTool) Name() string { return "tm-mytool" }

func (t *MyTool) Run(ctx context.Context, j worker.Job) (worker.Result, error) {
    return worker.Result{
        Findings: []worker.Finding{{
            Kind:     "demo",
            Severity: worker.SeverityInfo,
            Title:    "Hello from " + j.Asset.Value,
        }},
    }, nil
}

func main() { worker.Serve(&MyTool{}) }
```

A worker with that body will:

- read `manifest.yaml` next to the binary
- connect to Postgres (`PG_DSN` env)
- subscribe to its declared phase queue via River
- expose `/metrics` and `/healthz` on `:9090` (configurable)
- emit OTLP traces if `OTEL_EXPORTER_OTLP_ENDPOINT` is set

## Documentation

- [`docs/IDEMPOTENCE.md`](./docs/IDEMPOTENCE.md) - why your `Run`
  must be safe to retry, what the SDK guarantees vs what you own
- [`docs/MANIFEST.md`](./docs/MANIFEST.md) - the YAML schema in detail
- [`docs/ASSETS.md`](./docs/ASSETS.md) - kinds, attrs conventions per
  kind, JSONB round-trip gotchas
- [`docs/FINDINGS.md`](./docs/FINDINGS.md) - finding shape, dedup hash
  recipe, kinds + severity ladder
- [`docs/CONCURRENCY.md`](./docs/CONCURRENCY.md) - per-host limits,
  AIMD, circuit breaker, dns-side concurrency
- [`docs/OBSERVABILITY.md`](./docs/OBSERVABILITY.md) - free metrics
  the SDK exposes, custom metrics + structured logging conventions

## Changelog

See [CHANGELOG.md](./CHANGELOG.md) for release notes by version.

## License

MIT.
