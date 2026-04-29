package wtest

import (
	"context"
	"errors"
	"testing"
)

// fakeTool is a minimal Configurable used to drive ReloadCases. It
// holds both an exported and unexported field so the helper exercises
// both code paths.
type fakeTool struct {
	TimeoutSec int
	apiKey     string // unexported
	failNext   bool
}

func (f *fakeTool) ReloadConfig(_ context.Context, cfg map[string]any) error {
	if f.failNext {
		return errors.New("forced failure")
	}
	if v, ok := cfg["timeout_seconds"].(float64); ok && v > 0 {
		f.TimeoutSec = int(v)
	}
	if v, ok := cfg["api_key"].(string); ok && v != "" {
		f.apiKey = v
	}
	return nil
}

func TestReloadCases_AppliesGoodValue(t *testing.T) {
	tool := &fakeTool{TimeoutSec: 15}
	ReloadCases(t, tool, []ReloadCase{
		{Name: "apply", Cfg: map[string]any{"timeout_seconds": 30.0}, Field: "TimeoutSec", Want: 30},
	})
	if tool.TimeoutSec != 30 {
		t.Errorf("post-reload TimeoutSec = %d", tool.TimeoutSec)
	}
}

func TestReloadCases_IgnoresWrongType(t *testing.T) {
	tool := &fakeTool{TimeoutSec: 15}
	ReloadCases(t, tool, []ReloadCase{
		{Name: "wrong_type", Cfg: map[string]any{"timeout_seconds": "bogus"}, Field: "TimeoutSec", Want: 15},
		{Name: "zero_value", Cfg: map[string]any{"timeout_seconds": 0.0}, Field: "TimeoutSec", Want: 15},
	})
}

func TestReloadCases_UnexportedField(t *testing.T) {
	tool := &fakeTool{}
	ReloadCases(t, tool, []ReloadCase{
		{Name: "set_key", Cfg: map[string]any{"api_key": "secret"}, Field: "apiKey", Want: "secret"},
	})
	if tool.apiKey != "secret" {
		t.Errorf("apiKey = %q", tool.apiKey)
	}
}

func TestReloadCases_WantErrCase(t *testing.T) {
	tool := &fakeTool{failNext: true, TimeoutSec: 9}
	// We use the ReloadCase machinery to assert that a forced error
	// flows through the WantErr path. Field is left empty so the
	// helper doesn't try to compare values when the error fired.
	ReloadCases(t, tool, []ReloadCase{
		{Name: "errors", Cfg: map[string]any{}, WantErr: true},
	})
}

func TestReadField_MissingFieldReturnsFalse(t *testing.T) {
	tool := &fakeTool{}
	if _, ok := readField(tool, "DoesNotExist"); ok {
		t.Errorf("missing field should return ok=false")
	}
}

func TestReadField_NonStructReturnsFalse(t *testing.T) {
	if _, ok := readField("hello", "Anything"); ok {
		t.Errorf("non-struct should return ok=false")
	}
}
