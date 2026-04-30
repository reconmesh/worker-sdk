# Changelog

All notable changes to `worker-sdk` are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) ·
this project follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [v0.5.4] - 2026-04-29

### Added
- `Manifest.Description` · optional one-line operator-facing summary
  surfaced on the controlplane Plugins page. YAML round-trip pinned by
  test (`TestLoadManifest_DescriptionRoundTrip`). Wire-additive · workers
  without a description still register cleanly.
- `worker/jobargs_test.go` · pin tests on the JobKind wire contract
  (sister to controlplane/internal/jobtype/jobtype_test.go). A silent
  rename on either side makes River stop matching jobs · the pin
  guard is now symmetric.

### Tests
- `internal/dispatcher` (notif-service) shape audit added a
  `TestLoadManifest_DescriptionRoundTrip` pin · catches yaml-tag drift
  on the new Description field that would silently blank the catalog.

### CI
- `release.yml` workflow added · v* tags now create a GitHub Release
  page with auto-generated notes + a body pointing at CHANGELOG.md.
  The library doesn't ship binaries, but consumer module-maintainers
  bumping `require worker-sdk vX.Y.Z` can now browse releases at
  git.vozec.lan/Parabellum/worker-sdk/releases instead of scrolling git log.

### Fixed
- `Manifest.Validate` accepted `priority_hint` 0..4; widened to 0..9.
  5 modules (tm-vulnx=8, tm-dork=7, tm-ctwatch=6, tm-jsfinder=5,
  tm-uncover=5) shipped with values > 4 and would `worker.Serve()`-fail
  at boot with `manifest validate: phases[0].priority_hint must be
  0..4`. The cascade engine in controlplane/internal/cascade clamps
  to River's 1..4 range at job-insertion time, so 5..9 are documentary
  intent that map to River priority 4. Pin test reflects the new shape.

### Docs
- `Finding` godoc corrected · the doc claimed dedup happens via a
  `findings.dedup_hash` column. No such table exists · findings live
  as JSONB elements inside `assets.attrs.findings`. Same dedup outcome
  via per-element `hash` field, but the documented model now matches
  the storage layout. Operators writing custom analytics SQL get the
  right shape.
- README Layout block aligned with the actual package tree · phantoms
  removed (`worker/filter/`, `proto/`, `internal/`); real entries
  surfaced (`worker/jobargs.go`, `worker/river_adapter.go`,
  `worker/once.go`, `sdk/metrics/`, `grafana/`).
- README Documentation section trimmed to the 2 docs that actually
  exist (IDEMPOTENCE.md + MANIFEST.md). Phantom `ASSETS.md` /
  `FINDINGS.md` / `CONCURRENCY.md` / `OBSERVABILITY.md` references
  dropped · godoc on the public types in `worker/` is the canonical
  reference until the long-form docs land.

## [v0.5.3] - 2026-04-29

Session marathon · all additive · no wire-contract break vs v0.5.2.

### Added
- `httpcache.SourceCache` · per-(url_hash, source_path) persistence
  for original sources recovered from `.map` sourcemap v3 archives
  (H7/C2 chain). Wraps `tm_extracted_sources` table from
  `platform` migration `0016`.
- `httpcache.Cache.Pool()` accessor · exposes the underlying
  pgxpool so a sister cache (`SourceCache`) can attach without
  doubling the connection set.
- `sdk/mtls/mtls.go` cleanhttp pattern · sized keep-alive pool 100/10
  (was 20), `ProxyFromEnvironment`, `ForceAttemptHTTP2`. 30 lines
  inline in lieu of the projectdiscovery/cleanhttp dep weight.

### Performance
- `worker.fingerprintAttrs` · pool sha256 hashers + bytes.Buffer
  via `sync.Pool`. -27% bytes/op on the UpsertAsset hot path.
  5 pin tests + 1 BenchmarkFingerprintAttrs ground truth.

### Tests
- 66 (was 53) · +13 from SourceCache validation, fingerprint
  pool stability, and mtls cleanhttp shape pins.

### Docs
- README sdk/ layout reflects the actual surface (mtls, httpcache,
  dns, secretbox, tracing).

## [v0.5.2] - 2026-04-25

### Added
- `sdk/secretbox` · I22 read-only AES-256-GCM decrypt for
  cluster_settings secrets (paired with the controlplane's
  encrypt-on-PUT). 14 pin tests.
- `Manifest.Secrets []string` · field-level declaration so
  controlplane knows which config fields to walk on encrypt /
  mask paths.
- `runtime.applyConfig` calls `decryptSecrets` between merge
  and `ReloadConfig` · workers see plaintext only inside the
  process.

## [v0.5.1] - 2026-04-15

### Added
- `Job.ForceFresh` + `CascadeArgs.ForceFresh` · phase 1 manual-rescan
  bypass. Wired by tm-resolve (bypass DNS resolvers) and
  techmapper-worker (skip body cache short-circuit).

## [v0.5.0] - 2026-04-01

### Added
- `tm.Fingerprint.SourceMaps` typed manifest (Stage 9 substrat).
- Full Phase 0 surface · auth/mTLS/audit/observability stable.

For the full pre-0.5.0 history, see git log.
