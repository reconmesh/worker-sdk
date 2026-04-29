// Package worker is the public surface of the reconmesh worker SDK.
//
// A tool author imports this package, implements [Tool], and calls
// [Serve]. Every other concern - Postgres pool, River subscription,
// OpenTelemetry tracing, metrics, finding dedup, graceful shutdown,
// retries, concurrency limits - is provided by the runtime that
// [Serve] starts.
//
// The contract is wire-stable across the [Version] major: control
// planes and workers built against any v1.x of this SDK interoperate.
// See ../docs/VERSIONING.md in the platform repo for the rules.
package worker

import (
	"context"
	"time"
)

// Version is the SDK semantic version. Workers expose it via their
// `workers.sdk_version` row so the control plane can flag incompatibilities
// at the registration step instead of at job dispatch time.
const Version = "0.1.0"

// Tool is the single interface a worker implements. Keep it small on
// purpose: every method here is a method every consumer must care about.
//
// Cross-cutting concerns (validation of the manifest, idempotence
// enforcement, metric emission) are NOT in this interface - they're
// in the SDK runtime. A tool author who reads this file should leave
// with a complete mental model of what their job is.
type Tool interface {
	// Name returns the canonical tool identifier - must match the
	// `tool:` field in manifest.yaml. Used as a label on metrics, as
	// the `findings.tool` column, and as the worker registration key.
	Name() string

	// Run executes one job. It is invoked by the SDK runtime once
	// River has reserved a job for this worker.
	//
	// Contract:
	//   - ctx is canceled when the job's deadline is reached or the
	//     worker is shutting down. Respect it; long-blocking calls
	//     without ctx awareness will be killed and counted as a
	//     timeout.
	//   - The runtime guarantees Run is invoked at most once per
	//     (run_id, asset_id, phase) for the same River job ID. If
	//     the worker crashes mid-run, the same job is redelivered
	//     with the SAME job ID - Run MUST be idempotent (see
	//     docs/IDEMPOTENCE.md).
	//   - Returning an error nacks the job. The runtime decides
	//     retry vs dead-letter based on error class (see ErrFatal,
	//     ErrTransient).
	//   - Returning (Result, nil) acks; the runtime persists assets
	//     and findings transactionally.
	Run(ctx context.Context, job Job) (Result, error)
}

// Updatable is an optional interface. Tools that maintain a local DB
// (rule sets, signature dumps, vendor lists) implement it so the
// control plane can trigger refreshes via `POST /tools/{name}/update`.
type Updatable interface {
	Update(ctx context.Context) error
}

// Configurable is an optional interface that lets a Tool re-read its
// runtime config without restarting the process. The SDK runtime
// invokes ReloadConfig:
//
//   1. Once at boot, after the worker registers with the cluster,
//      with the deep-merged config (manifest defaults ⊕ operator
//      override from the tool_configs PG table).
//   2. Every time the operator edits the override via the UI / API,
//      driven by PG NOTIFY 'tool_config_changed'. The runtime's
//      LISTEN goroutine fires ReloadConfig on receipt.
//
// The map carries the FULL effective config (every key the worker
// would see at boot). Tools that hold mutable state (resolver pool,
// thread caps, API keys) swap it under their own mutex so jobs
// already in flight see consistent values for their duration but
// new jobs pick up the change.
//
// Returning an error logs but doesn't crash the worker - bad config
// can be rolled back by the operator from the same UI.
type Configurable interface {
	ReloadConfig(ctx context.Context, cfg map[string]any) error
}

// Healthchecker is an optional interface a Tool implements to
// surface its functional state to the Modules admin page. The SDK
// runtime calls Healthcheck periodically (default 60s) and writes
// the result to the controlplane's module_health table; the UI
// renders the latest status with the appropriate severity color
// and the operator-facing error message.
//
// Use cases:
//   - tm-uncover / tm-c99: probe the API key against a cheap endpoint
//     so a revoked key surfaces as "API key invalid" instead of
//     silent zero-result jobs
//   - tm-vulnx: validate the embedded CVE DB loaded successfully
//   - tm-shodan-passive / tm-favicon-hash: ping the upstream service
//     so a multi-hour outage surfaces as "service_unreachable"
//
// Healthcheck MUST be cheap and non-mutating · the runtime calls it
// on a schedule whether or not the worker is processing jobs.
// Tools without this interface get a default "unknown" status that
// flips to "healthy" when the worker successfully completes any
// Run() and to "unhealthy" when Run() returns a HealthError.
type Healthchecker interface {
	Healthcheck(ctx context.Context) HealthReport
}

// HealthReport is what Healthcheck returns. The runtime forwards
// this to the controlplane's module_health endpoint without
// transformation · keep it small.
type HealthReport struct {
	// Status is the overall verdict: "healthy" / "degraded" /
	// "unhealthy". "degraded" means the tool can still serve some
	// requests (e.g. one of N upstreams is down).
	Status string
	// Class names the failure mode when Status != "healthy" so the
	// UI can render targeted help. See module_health.error_class
	// taxonomy.
	Class string
	// Message is operator-facing, single-line · drop into a tooltip.
	Message string
	// Extra is free-form structured detail (which upstream failed,
	// quota remaining, last good response timestamp, ...). Stored
	// as JSONB; rendered as a key/value list in the UI.
	Extra map[string]any
}

// HealthError is a sentinel error class workers return from Run()
// when a transient or persistent infrastructure issue prevents the
// job from running. The runtime catches it, reports the class to
// the controlplane, and acks the job (returning ErrTransient via
// the standard path retries; HealthError is "this whole module is
// broken, don't pile on retries").
type HealthError struct {
	Class   string
	Message string
}

func (e *HealthError) Error() string {
	if e.Class == "" {
		return e.Message
	}
	return e.Class + ": " + e.Message
}

// NewHealthError builds a HealthError. Helper because Class strings
// must come from the closed-set taxonomy:
//
//	worker.NewHealthError("api_key_invalid", "Shodan returned 401")
//	worker.NewHealthError("service_unreachable", "DNS lookup failed for ...")
func NewHealthError(class, message string) *HealthError {
	return &HealthError{Class: class, Message: message}
}

// Job is the input to Tool.Run.
//
// Field stability: every field here is part of the wire contract.
// Adding a new field is a MINOR; renaming or removing one is a MAJOR.
type Job struct {
	// ID is the River job ID. Idempotency key - the runtime guarantees
	// at-least-once delivery; identical IDs in two invocations mean
	// the job is being retried.
	ID int64
	// RunID is the pipeline run that produced this job. Multiple
	// jobs share the same RunID; cascades inherit their parent's
	// RunID.
	RunID string
	// ScopeID is the owning scope. Findings produced in Run must be
	// attributed to this scope; the runtime enforces it.
	ScopeID string
	// Phase is the manifest phase name that scheduled this job. A
	// worker subscribed to multiple phases distinguishes here.
	Phase string
	// Asset is the unit of work. Its Kind is one of the kinds the
	// worker's manifest declared in `consumes.kinds`; Attrs match
	// the manifest's `consumes.filter`.
	Asset Asset
	// Priority maps to River's job priority. 1 = highest, 4 = lowest.
	// Tools rarely need this but it's exposed for telemetry.
	Priority int
	// Deadline mirrors the ctx deadline; provided as a separate field
	// for tools that want to compute their own internal budgets
	// without inspecting ctx.
	Deadline time.Time
	// TraceID is already set on ctx via OTel. Exposed here for
	// log-only use cases where the trace context isn't propagated.
	TraceID string
	// ForceFresh signals the operator triggered this run manually and
	// wants live data - caches (httpcache.Lookup, dns-service LRU,
	// in-memory tool-side memo, …) MUST be bypassed for this Job.
	// Tools that don't cache anything can ignore this field; tools
	// that cache should branch on it BEFORE the cache lookup, then
	// still write to the cache after the fresh fetch so future
	// non-fresh callers benefit.
	ForceFresh bool
}

// Result is the output of Tool.Run.
//
// All slices may be nil. The runtime never inspects nil slices.
// Empty Result + nil error is a valid "we ran cleanly, found nothing"
// outcome - counted as success, not skipped.
type Result struct {
	// NewAssets are children to add to the asset graph. The runtime
	// upserts them by (scope_id, kind, value); duplicates are merged.
	NewAssets []Asset
	// Findings are the actionable observations. The runtime dedupes
	// by (asset_id, dedup_hash); re-emitted findings update last_seen.
	Findings []Finding
	// AssetUpdate, when non-nil, is merged into the consumed asset's
	// attrs. Use it to enrich an asset without producing a new one
	// (e.g. resolve adds an `ip` attr to a `subdomain`).
	AssetUpdate map[string]any
	// Stats is free-form metric contribution to the run summary.
	// Surfaced on /api/runs/{id} under the worker's name.
	Stats map[string]any
}

// Asset is one node in the discovery graph.
//
// Kind is an open enum: the platform doesn't enforce a closed set so
// new tools can introduce new kinds. The convention is short snake_case
// nouns: "subdomain", "host", "port", "service", "url", "js_file".
//
// Value is the canonical, comparable identity within Kind. Two assets
// with the same (scope, kind, value) are the same asset - choose Value
// so that re-discovery collapses (lowercase host, normalize URL, etc.).
//
// Attrs carries kind-specific metadata. Consult docs/ASSETS.md for the
// agreed-upon shape of each kind. Adding new attrs without coordination
// is fine - readers should only depend on documented attrs.
type Asset struct {
	ID       string         `json:"id,omitempty"`        // set by runtime, ignored on write
	ScopeID  string         `json:"scope_id"`
	Kind     string         `json:"kind"`
	Value    string         `json:"value"`
	ParentID string         `json:"parent_id,omitempty"`
	Attrs    map[string]any `json:"attrs,omitempty"`
	// State is set by the runtime; tools should not write it.
	State string `json:"state,omitempty"`
}

// Severity is the canonical scale for findings. Five levels keep
// triage UIs tractable. The control plane's UI maps each to a color
// and an alert threshold.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// Finding is one actionable observation.
//
// Dedup behavior: the runtime computes a hash from
// (Kind, Severity, canonicalized Data) and stamps it as the finding
// object's `hash` field. Findings live as JSONB elements inside
// assets.attrs.findings (no separate findings table) · re-emitting
// a finding with the same hash UPSERT-merges by hash and bumps
// last_seen rather than appending a new element, so periodic
// re-scans don't bloat the array. See dedup.go.
type Finding struct {
	// Kind is the finding category, short snake_case.
	// Examples: "tech", "secret", "exposed_git", "weak_tls".
	Kind     string
	Severity Severity
	// Title is the human-readable headline shown in the UI.
	Title string
	// Data is the worker-specific evidence. Sort keys, normalize URLs,
	// lower-case strings - anything that doesn't change meaning but
	// affects equality should be canonical here so the dedup hash
	// stays stable across runs.
	Data map[string]any
}

// ErrTransient signals a retryable failure (network blip, broker
// hiccup, third-party 503). The runtime backs off and re-queues. Use
// errors.Is(err, ErrTransient) at call sites that want to distinguish.
var ErrTransient = transientErr{}

// ErrFatal signals a non-retryable failure (input malformed, target
// schema unsupported). The runtime moves the job to dead-letter
// without retry.
var ErrFatal = fatalErr{}

type transientErr struct{}

func (transientErr) Error() string { return "transient" }
func (transientErr) Transient()    {}

type fatalErr struct{}

func (fatalErr) Error() string { return "fatal" }
func (fatalErr) Fatal()        {}

// IsTransient and IsFatal let runtime classifiers decide retry policy
// without exporting the concrete types.
func IsTransient(err error) bool {
	_, ok := err.(interface{ Transient() })
	return ok
}

func IsFatal(err error) bool {
	_, ok := err.(interface{ Fatal() })
	return ok
}
