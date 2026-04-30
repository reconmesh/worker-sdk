package wtest

import (
	"context"
	"reflect"
	"testing"

	"git.vozec.fr/Parabellum/worker-sdk/worker"
)

// ReloadCase is one case in a [ReloadCases] table. Field is the Go
// field name on the Tool struct (case-sensitive); reflection reads it
// after ReloadConfig returns. Want is compared with reflect.DeepEqual.
//
// To assert "field unchanged on bad input", set Want to the value the
// Tool started with - the helper has no notion of "before/after" diff.
type ReloadCase struct {
	// Name is the subtest name passed to t.Run.
	Name string
	// Cfg is the map handed to ReloadConfig. Use float64 for numerics
	// since that matches the JSON-decoded shape the runtime feeds in.
	Cfg map[string]any
	// Field is the Go field name on the Tool struct to read after
	// ReloadConfig. Empty skips the field check (useful when the case
	// just asserts ReloadConfig returns no error).
	Field string
	// Want is the expected value of Field after the call.
	Want any
	// WantErr asserts ReloadConfig returns an error. When true, Field
	// + Want are still checked after the call (unchanged-state asserts).
	WantErr bool
}

// ReloadCases drives a Configurable Tool through a table of reload
// scenarios. Each case runs as a t.Run subtest; reflection reads the
// post-call field value.
//
// Example:
//
//	wtest.ReloadCases(t, tool, []wtest.ReloadCase{
//	    {Name: "apply",      Cfg: map[string]any{"timeout_seconds": 30.0}, Field: "TimeoutSec", Want: 30},
//	    {Name: "wrong_type", Cfg: map[string]any{"timeout_seconds": "x"},  Field: "TimeoutSec", Want: 15},
//	    {Name: "zero",       Cfg: map[string]any{"timeout_seconds": 0.0},  Field: "TimeoutSec", Want: 15},
//	})
//
// The helper handles unexported fields too (uses
// [reflect.Value.FieldByName] then unsafe-style access via the addr
// of the parent struct).
func ReloadCases(t *testing.T, tool worker.Configurable, cases []ReloadCase) {
	t.Helper()
	for _, tc := range cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			err := tool.ReloadConfig(context.Background(), tc.Cfg)
			switch {
			case tc.WantErr && err == nil:
				t.Errorf("ReloadCase %q: expected error, got nil", tc.Name)
			case !tc.WantErr && err != nil:
				t.Errorf("ReloadCase %q: unexpected error: %v", tc.Name, err)
			}
			if tc.Field == "" {
				return
			}
			got, ok := readField(tool, tc.Field)
			if !ok {
				t.Errorf("ReloadCase %q: field %q not found on %T", tc.Name, tc.Field, tool)
				return
			}
			if !reflect.DeepEqual(got, tc.Want) {
				t.Errorf("ReloadCase %q: %s = %v (%T), want %v (%T)",
					tc.Name, tc.Field, got, got, tc.Want, tc.Want)
			}
		})
	}
}

// readField returns the value of the named field on tool, walking
// through one level of pointer if needed. Supports unexported fields
// by reading the underlying memory; this matches the way most modules
// store private state.
func readField(tool any, name string) (any, bool) {
	v := reflect.ValueOf(tool)
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil, false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil, false
	}
	f := v.FieldByName(name)
	if !f.IsValid() {
		return nil, false
	}
	if f.CanInterface() {
		return f.Interface(), true
	}
	// Unexported field. Bypass the visibility check by reflecting via
	// the field's address. The caller's tool is typically a *Tool, so
	// we can take the address of v and offset to f.
	if !v.CanAddr() {
		return nil, false
	}
	// reflect.NewAt + UnsafePointer produces a settable Value pointing
	// at the same memory; .Elem().Interface() reads it.
	ptr := reflect.NewAt(f.Type(), unsafePtr(f))
	return ptr.Elem().Interface(), true
}
