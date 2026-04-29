# Changelog

All notable changes to `worker-sdk` are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) ·
this project follows [Semantic Versioning](https://semver.org/).

## [v0.5.3] - 2026-04-29

Session marathon · all additive · no wire-contract break vs v0.5.2.

### Added
- `httpcache.SourceCache` · per-(url_hash, source_path) persistence
  for original sources recovered from `.map` sourcemap v3 archives
  (H7/C2 chain). Wraps `tm_extracted_sources` table from
  `recon-platform` migration `0016`.
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
