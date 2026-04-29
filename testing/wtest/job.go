package wtest

import (
	"time"

	"github.com/google/uuid"
	"github.com/reconmesh/worker-sdk/worker"
)

// JobBuilder fluently constructs a [worker.Job] for tests. Defaults:
//
//	ID         random int64-shaped uuid (low bits)
//	RunID      random uuid string
//	ScopeID    "" (uuid.Nil string form)
//	Phase      "test"
//	Asset.Kind "host"
//	Asset.ID   random uuid string
//	Priority   2
//	Deadline   now + 30s
//
// Construct via [MockJob]; call .Build() to materialize.
type JobBuilder struct {
	job worker.Job
}

// MockJob returns a JobBuilder pre-populated with sane defaults. Call
// the With* methods to override individual fields, then Build().
func MockJob() *JobBuilder {
	id := uuid.New()
	return &JobBuilder{
		job: worker.Job{
			ID:       int64(id.ID()),
			RunID:    uuid.NewString(),
			ScopeID:  "",
			Phase:    "test",
			Priority: 2,
			Deadline: time.Now().Add(30 * time.Second),
			Asset: worker.Asset{
				ID:    uuid.NewString(),
				Kind:  "host",
				Value: "",
				Attrs: map[string]any{},
			},
		},
	}
}

// WithKind sets Asset.Kind.
func (b *JobBuilder) WithKind(kind string) *JobBuilder {
	b.job.Asset.Kind = kind
	return b
}

// WithValue sets Asset.Value.
func (b *JobBuilder) WithValue(v string) *JobBuilder {
	b.job.Asset.Value = v
	return b
}

// WithAttrs replaces Asset.Attrs wholesale. Use [MergeAttrs] to add
// individual keys without clobbering.
func (b *JobBuilder) WithAttrs(attrs map[string]any) *JobBuilder {
	if attrs == nil {
		b.job.Asset.Attrs = map[string]any{}
	} else {
		// Copy so the test owning the map can't mutate the job under
		// our feet.
		copyMap := make(map[string]any, len(attrs))
		for k, v := range attrs {
			copyMap[k] = v
		}
		b.job.Asset.Attrs = copyMap
	}
	return b
}

// MergeAttrs adds (or overwrites) individual keys without clobbering
// the existing Attrs map.
func (b *JobBuilder) MergeAttrs(attrs map[string]any) *JobBuilder {
	if b.job.Asset.Attrs == nil {
		b.job.Asset.Attrs = map[string]any{}
	}
	for k, v := range attrs {
		b.job.Asset.Attrs[k] = v
	}
	return b
}

// WithScopeID sets the job's scope ID. Accepts a uuid or any string;
// the worker contract just stores it as a string.
func (b *JobBuilder) WithScopeID(id uuid.UUID) *JobBuilder {
	b.job.ScopeID = id.String()
	b.job.Asset.ScopeID = id.String()
	return b
}

// WithScopeIDString is the string form for tests that don't carry a
// uuid.UUID around.
func (b *JobBuilder) WithScopeIDString(id string) *JobBuilder {
	b.job.ScopeID = id
	b.job.Asset.ScopeID = id
	return b
}

// WithRunID sets the pipeline run ID.
func (b *JobBuilder) WithRunID(id string) *JobBuilder {
	b.job.RunID = id
	return b
}

// WithPhase sets the manifest phase name.
func (b *JobBuilder) WithPhase(phase string) *JobBuilder {
	b.job.Phase = phase
	return b
}

// WithAssetID sets the consumed asset's id.
func (b *JobBuilder) WithAssetID(id string) *JobBuilder {
	b.job.Asset.ID = id
	return b
}

// WithParentID sets the consumed asset's parent.
func (b *JobBuilder) WithParentID(id string) *JobBuilder {
	b.job.Asset.ParentID = id
	return b
}

// WithDeadline sets the absolute deadline.
func (b *JobBuilder) WithDeadline(t time.Time) *JobBuilder {
	b.job.Deadline = t
	return b
}

// WithForceFresh signals the operator triggered the run manually and
// any caches should be bypassed.
func (b *JobBuilder) WithForceFresh(v bool) *JobBuilder {
	b.job.ForceFresh = v
	return b
}

// WithPriority sets the River priority (1 highest, 4 lowest).
func (b *JobBuilder) WithPriority(p int) *JobBuilder {
	b.job.Priority = p
	return b
}

// Build returns the assembled worker.Job. The returned value is a
// copy; subsequent builder mutations don't affect prior builds.
func (b *JobBuilder) Build() worker.Job {
	// Defensive copy of the attrs map.
	out := b.job
	if out.Asset.Attrs != nil {
		copied := make(map[string]any, len(out.Asset.Attrs))
		for k, v := range out.Asset.Attrs {
			copied[k] = v
		}
		out.Asset.Attrs = copied
	}
	return out
}
