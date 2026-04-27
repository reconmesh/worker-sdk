// Package worker is the public surface of the reconmesh worker SDK.
//
// A tool author imports this package, implements [Tool], and calls
// [Serve]. Every other concern — Postgres pool, River subscription,
// OpenTelemetry tracing, metrics, finding dedup, graceful shutdown,
// retries, concurrency limits — is provided by the runtime that
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
// enforcement, metric emission) are NOT in this interface — they're
// in the SDK runtime. A tool author who reads this file should leave
// with a complete mental model of what their job is.
type Tool interface {
	// Name returns the canonical tool identifier — must match the
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
	//     with the SAME job ID — Run MUST be idempotent (see
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

// Job is the input to Tool.Run.
//
// Field stability: every field here is part of the wire contract.
// Adding a new field is a MINOR; renaming or removing one is a MAJOR.
type Job struct {
	// ID is the River job ID. Idempotency key — the runtime guarantees
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
}

// Result is the output of Tool.Run.
//
// All slices may be nil. The runtime never inspects nil slices.
// Empty Result + nil error is a valid "we ran cleanly, found nothing"
// outcome — counted as success, not skipped.
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
// with the same (scope, kind, value) are the same asset — choose Value
// so that re-discovery collapses (lowercase host, normalize URL, etc.).
//
// Attrs carries kind-specific metadata. Consult docs/ASSETS.md for the
// agreed-upon shape of each kind. Adding new attrs without coordination
// is fine — readers should only depend on documented attrs.
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
// (Kind, Severity, canonicalized Data) and stores it in the
// findings.dedup_hash column. Re-emitting a finding with the same
// dedup_hash bumps last_seen rather than inserting a row, so periodic
// re-scans don't bloat the table. See dedup.go.
type Finding struct {
	// Kind is the finding category, short snake_case.
	// Examples: "tech", "secret", "exposed_git", "weak_tls".
	Kind     string
	Severity Severity
	// Title is the human-readable headline shown in the UI.
	Title string
	// Data is the worker-specific evidence. Sort keys, normalize URLs,
	// lower-case strings — anything that doesn't change meaning but
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
