package wtest

import (
	"reflect"
	"unsafe"
)

// unsafePtr returns the underlying pointer of a reflect.Value whose
// CanInterface() is false (i.e. an unexported struct field). Used by
// readField in reload.go so ReloadCases can verify state on private
// Tool fields without forcing modules to export them just for tests.
//
// This is the standard "I know what I'm doing" pattern for reflection
// over unexported fields - no different from what testify uses
// internally.
func unsafePtr(v reflect.Value) unsafe.Pointer {
	return unsafe.Pointer(v.UnsafeAddr())
}
