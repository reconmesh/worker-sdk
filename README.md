# worker-sdk

Shared Go library every reconmesh worker links against. Carries the wire
contract between the control plane and the workers, plus the runtime
plumbing every worker would otherwise re-implement: River subscription,
Postgres pool, OpenTelemetry tracing, Prometheus metrics, finding
deduplication, graceful shutdown.

The SDK's surface is intentionally tiny — three interfaces, a handful of
types, one `Serve` entry point. New tools implement `Tool.Run` and call
`worker.Serve(t)`. That's it. Everything else is hidden.

## Versioning

`worker-sdk` is the wire contract. Breaking changes cost ~one PR per
worker, so we treat them carefully:

- MINOR / PATCH bumps preserve full backward compatibility.
- MAJOR bumps go through a deprecation cycle of at least one MINOR before
  the old API is removed.
- The SDK ships its own `Version` constant; the control plane reads each
  worker's `manifest.sdk_version` to assert compatibility.

See `../platform/docs/VERSIONING.md` for the org-wide policy.

## Layout

```
worker-sdk/
├── worker/             # the public package; what `import` consumers use
│   ├── tool.go         # Tool interface, Job, Result, Finding, Asset
│   ├── manifest.go     # YAML manifest schema + load/validate
│   ├── serve.go        # Serve() entry point: River + signal handling
│   ├── runtime.go      # PG pool, metrics, OTel
│   ├── asset_writer.go # UpsertAsset + sync.Pool fingerprint (G1)
│   ├── dedup.go        # finding hash canonicalization
│   └── filter/         # `consumes.filter` parser + PG predicate compile
├── sdk/                # opt-in helpers · workers import only what they need
│   ├── mtls/           # cleanhttp-style http.Client with mTLS roots
│   ├── httpcache/      # cluster body cache + SourceCache (H7)
│   ├── dns/            # dns-service client wrapper
│   ├── secretbox/      # I22 AES-256-GCM decrypt (read-only by design)
│   └── tracing/        # OTel exporter helpers
├── proto/              # (future) protobuf job payloads when we go cross-language
├── internal/
└── docs/
```

`internal/` is hidden by Go's tooling — anything outside `worker/` and
`sdk/` is implementation detail and may change without a SemVer event.

## Quick example

```go
package main

import (
    "context"

    "github.com/reconmesh/worker-sdk/worker"
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

- `docs/ASSETS.md` — conventions for `Asset.Kind` and `Asset.Attrs`
- `docs/FINDINGS.md` — finding shape, severity, dedup hash recipe
- `docs/IDEMPOTENCE.md` — why your `Run` must be safe to retry
- `docs/CONCURRENCY.md` — per-host limits, AIMD, circuit breaker
- `docs/OBSERVABILITY.md` — required metrics + tracing conventions
- `docs/MANIFEST.md` — the YAML schema in detail

## Changelog

See [CHANGELOG.md](./CHANGELOG.md) for release notes by version.

## License

MIT.
