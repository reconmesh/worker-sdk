package worker

import "github.com/google/uuid"

// JobKind is the River "kind" string the cascade emits and we
// consume. It MUST match controlplane/internal/jobtype.JobKind —
// renaming requires a coordinated rollout (push controlplane first
// with both old + new, then push workers, then drop old).
//
// We keep the args type local in worker-sdk rather than importing
// it from controlplane so workers don't take a hard cross-repo
// dependency. The fields are stable wire contract; field renames
// are MAJOR bumps of worker-sdk.
const JobKind = "reconmesh.cascade.v1"

// CascadeArgs is the River JobArgs the cascade emits per phase x
// asset pair. The worker's runtime fetches the asset row from PG at
// dispatch time so the args don't go stale (an asset attrs update
// between enqueue and dequeue uses the freshest values).
type CascadeArgs struct {
	ScopeID   uuid.UUID `json:"scope_id"`
	AssetID   uuid.UUID `json:"asset_id"`
	Phase     string    `json:"phase"`
	RunID     uuid.UUID `json:"run_id,omitempty"`
	EventType string    `json:"event_type,omitempty"`
}

// Kind satisfies river.JobArgs. River dispatches jobs to the worker
// whose Args type advertises the same Kind.
func (CascadeArgs) Kind() string { return JobKind }
