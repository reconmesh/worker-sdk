# Finding shape + dedup hash recipe

A `worker.Finding` is one observation a worker emits. They live as
JSONB elements inside `assets.attrs.findings` (no separate findings
table) · the `/api/findings` endpoint flattens via
`jsonb_array_elements` for the dashboard.

## Shape

```go
type Finding struct {
    Kind     string         `json:"kind"`        // snake_case category
    Severity string         `json:"severity"`    // info|warn|error|critical
    Title    string         `json:"title"`       // one-line, operator-facing
    Data     map[string]any `json:"data"`        // free-form details
    Hash     string         `json:"hash"`        // computed by SDK on insert
    LastSeen time.Time      `json:"last_seen"`   // bumped on re-emission
}
```

### Kind

Short snake_case category. The `/findings` dashboard groups by kind
when the operator picks "group by kind" · keep it stable across
versions of the same finding.

Conventional kinds across the fleet:

- `tech` · technology / framework detected · `techmapper-worker`
- `secret` · regex-matched secret · `secrets` module
- `secret_entropy` · entropy-flagged secret · `secretsentropy` module
- `secret_weak` · framework-default / known-weak secret · `weaksecrets` module
- `cve` · CVE match · `tm-vulnx`
- `exposed_sourcemap` · `.map` reachable · sourcemap module
- `exposed_git` · `.git/` directory · (future tm-gitdump · not shipped)
- `weak_tls` · expired / weak cert · `tm-tlsx`

### Severity

- `info` · observation, no action needed
- `warn` · operator should look
- `error` · likely-actionable issue
- `critical` · likely security finding · pages on-call when wired to webhooks

The Findings dashboard sorts by severity desc by default, so
`critical` rows surface first.

### Data

Free-form. Operator UI renders selected keys via the worker's
manifest `ui.tab.views` declarative config. Conventional keys:

- `cve_id` (string) · `CVE-YYYY-NNNNN`
- `severity_source` · `nvd` / `manual` / `heuristic`
- `version_range` · the range of affected versions for tech-version
  matches · format follows `tm-vulnx`'s embedded shape
- `evidence` · the matched bytes / regex group / pattern that
  triggered the finding · keep short, operator-facing

## Dedup hash

The SDK computes `Finding.Hash` from
`(Kind, Severity, canonicalized Data)` before insert. Same content
= same hash = UPSERT-merge · `last_seen` bumps, no new array element.

This means:

- A cron sweep that re-touches an asset doesn't grow the array.
- Two workers emitting the same finding (e.g. a regex match + an
  entropy match for the same secret) DO produce two elements
  because `kind` differs · operators see both signals.
- Changing the `Title` doesn't affect the hash · so polishing the
  message doesn't deduplicate against prior emissions.

The canonicalization step in `worker/dedup.go`:

1. `Kind` lowercased, trimmed.
2. `Severity` normalized to one of the 4 levels.
3. `Data` walked depth-first, keys sorted, JSON-marshaled with
   stable float repr.
4. SHA-256 of the concatenated bytes.

If your worker computes its own hash (e.g. for cross-asset dedup),
use a different field key inside `Data` · don't override `Hash`.

## Reading findings

From the controlplane API:

```bash
# All findings for a scope, sorted by severity then last_seen.
curl https://controlplane:8000/api/scopes/$SCOPE_ID/findings

# Flat findings across all scopes, filtered.
curl 'https://controlplane:8000/api/findings?severity=critical&since=24h'

# Severity counts (dashboard cards).
curl https://controlplane:8000/api/findings/summary
```

The Go side reads via `jsonb_array_elements(assets.attrs->'findings')`
in `controlplane/internal/api/findings.go`.

## Removing findings

There's no `DELETE /api/findings/{hash}` today. Operators clear a
finding by:

1. Removing the underlying asset · the JSONB array goes with the row.
2. Editing the JSONB column directly via psql (operational escape
   hatch · audit_log captures who / when).
3. Re-tagging the asset with a "false-positive" tag · the dashboard
   filter chips can hide tagged assets.
