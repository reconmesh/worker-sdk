# Manifest reference

Every worker ships a `manifest.yaml` next to its binary. The control
plane reads it on registration; the SDK validates it at boot.

## Schema (annotated)

```yaml
# Required. Canonical worker name. Must equal Tool.Name() returned by
# the Go binary. Used as a metric label, the findings.tool column, and
# the worker registration key. Format: lowercase, snake_case or
# kebab-case, starts with a letter.
tool: tm-mytool

# Required. Worker's own SemVer. Reported in tm_workers.version. Bump
# this when YOU release; it's independent of worker-sdk's version.
version: 0.1.0

# Optional. Free-form team or individual identifier. Surfaced in the
# UI's "who owns this worker" page.
maintainer: team-tools

# Required. At least one phase. A binary can serve multiple phases
# (rare); the worker's Run method branches on Job.Phase.
phases:

  - # Required. Phase identifier referenced in Scope.phases[]. Format
    # same as `tool:`. Convention: prefix with the tool name when
    # ambiguous across the org (`techmapper-fingerprint` rather than
    # bare `fingerprint`).
    name: mytool-phase

    # Required. Asset selector for jobs.
    consumes:
      # Required, non-empty. Asset kinds this phase accepts. The
      # control plane queues a job for every newly-discovered asset
      # whose kind matches.
      kinds: [url]

      # Optional. Server-side predicate over the asset's attrs JSONB.
      # Grammar lives in worker/filter; supports = != < > <= >=, IN,
      # NOT IN, ~ (regex), ~* (case-insensitive regex), AND, OR, NOT,
      # parentheses. No functions, no joins, no field references
      # outside `attrs.*`.
      filter: 'attrs.status_code = 200 AND attrs.content_type ~ "text/html"'

    # Optional. Documentary metadata about phase outputs. The control
    # plane surfaces these in its UI; they don't constrain Run().
    produces:
      assets: []
      finding_kinds: [my_kind]

    # Optional. Cap on simultaneous Run() invocations targeting the
    # same host. Honor target rate limits without coupling tools.
    # 0 (default) = unlimited. The runtime uses the asset's parent
    # chain to resolve "host"; see docs/CONCURRENCY.md.
    concurrency_per_host: 4

    # Optional. Hint for the capacity planner; not enforced.
    avg_duration_ms: 500

    # Optional. Per-job hard timeout. 0 (default) = SDK default (300s).
    # ctx is canceled at this deadline.
    timeout_seconds: 60

    # Optional. River priority bias. 1 (highest) to 4 (lowest, default).
    # Used to push lightweight phases ahead of heavier ones inside the
    # same scope run.
    priority_hint: 3
```

## Validation

`LoadManifest` performs structural checks:

- `tool`, `version` non-empty
- `tool` matches `[a-z][a-z0-9]*([-_][a-z0-9]+)*`
- ≥1 phase, no duplicate phase names
- each phase has ≥1 `consumes.kinds`
- `priority_hint` in 0..4
- `timeout_seconds` ≥ 0

The filter expression isn't parsed at boot (it lives in the control
plane). Workers that include a malformed filter will register fine but
the control plane will refuse to schedule jobs against the malformed
phase and surface an error in `/api/workers`.

## Updating the schema

The manifest schema is part of the wire contract. New OPTIONAL fields
go in via MINOR bumps. New REQUIRED fields are MAJOR bumps because
older workers in the field will fail validation against the newer
schema.

When adding a field:

1. Add it to `Manifest` / `Phase` / etc. with a `yaml:",omitempty"` tag.
2. Document it here AND in `docs/MANIFEST.md` of the consuming
   control plane.
3. Default behavior when the field is absent must match the
   pre-existing behavior. No surprises for old workers.
