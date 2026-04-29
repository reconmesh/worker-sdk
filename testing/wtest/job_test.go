package wtest

import (
	"testing"

	"github.com/google/uuid"
)

func TestMockJob_Defaults(t *testing.T) {
	job := MockJob().Build()
	if job.RunID == "" {
		t.Errorf("RunID should be auto-set, got empty")
	}
	if job.Asset.Kind != "host" {
		t.Errorf("default kind = %q, want host", job.Asset.Kind)
	}
	if job.Phase != "test" {
		t.Errorf("default phase = %q, want test", job.Phase)
	}
	if job.Asset.Attrs == nil {
		t.Errorf("default Attrs should be non-nil empty map")
	}
	if job.Deadline.IsZero() {
		t.Errorf("default Deadline should not be zero")
	}
}

func TestMockJob_Chaining(t *testing.T) {
	scope := uuid.New()
	job := MockJob().
		WithKind("subdomain").
		WithValue("a.acme.com").
		WithAttrs(map[string]any{"discovered_via": "ct_log"}).
		WithScopeID(scope).
		WithPhase("discover").
		WithPriority(1).
		WithForceFresh(true).
		Build()

	if job.Asset.Kind != "subdomain" {
		t.Errorf("kind = %q", job.Asset.Kind)
	}
	if job.Asset.Value != "a.acme.com" {
		t.Errorf("value = %q", job.Asset.Value)
	}
	if job.Asset.Attrs["discovered_via"] != "ct_log" {
		t.Errorf("attrs not set: %+v", job.Asset.Attrs)
	}
	if job.ScopeID != scope.String() {
		t.Errorf("scope = %q", job.ScopeID)
	}
	if job.Asset.ScopeID != scope.String() {
		t.Errorf("asset.scope = %q", job.Asset.ScopeID)
	}
	if job.Phase != "discover" {
		t.Errorf("phase = %q", job.Phase)
	}
	if job.Priority != 1 {
		t.Errorf("priority = %d", job.Priority)
	}
	if !job.ForceFresh {
		t.Errorf("forcefresh should be true")
	}
}

func TestMockJob_BuildIsIsolated(t *testing.T) {
	// A second mutation on the builder must not affect a prior Build().
	b := MockJob().WithValue("v1")
	first := b.Build()
	b.WithValue("v2")
	second := b.Build()
	if first.Asset.Value != "v1" {
		t.Errorf("first.Value mutated to %q, want v1", first.Asset.Value)
	}
	if second.Asset.Value != "v2" {
		t.Errorf("second.Value = %q, want v2", second.Asset.Value)
	}
}

func TestMockJob_AttrsCopiedDefensive(t *testing.T) {
	src := map[string]any{"k": "v"}
	job := MockJob().WithAttrs(src).Build()
	src["k"] = "mutated"
	if job.Asset.Attrs["k"] != "v" {
		t.Errorf("WithAttrs should defensively copy; got %q", job.Asset.Attrs["k"])
	}
}

func TestMockJob_MergeAttrs(t *testing.T) {
	job := MockJob().
		WithAttrs(map[string]any{"a": 1}).
		MergeAttrs(map[string]any{"b": 2}).
		MergeAttrs(map[string]any{"a": 99}).
		Build()
	if job.Asset.Attrs["a"] != 99 {
		t.Errorf("MergeAttrs should overwrite: a=%v", job.Asset.Attrs["a"])
	}
	if job.Asset.Attrs["b"] != 2 {
		t.Errorf("MergeAttrs should add: b=%v", job.Asset.Attrs["b"])
	}
}

func TestMockJob_WithScopeIDString(t *testing.T) {
	job := MockJob().WithScopeIDString("scope-123").Build()
	if job.ScopeID != "scope-123" {
		t.Errorf("scope = %q", job.ScopeID)
	}
}
