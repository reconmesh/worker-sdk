package tracing

import (
	"context"
	"testing"
)

// The no-op path is the default when operators don't deploy an OTLP
// collector. It must (a) not error, (b) return a no-op shutdown that
// completes immediately, and (c) leave Tracer() callable so worker
// code paths don't need to special-case the no-tracing build.

func TestInit_NoEndpoint_NoOp(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	shutdown, err := Init(context.Background(), "test", nil)
	if err != nil {
		t.Fatalf("Init no-op: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown is nil")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown returned error: %v", err)
	}
	// Tracer() must remain callable. We don't assert span content;
	// the no-op provider returns a tracer that records no data.
	tr := Tracer("test")
	if tr == nil {
		t.Fatal("Tracer returned nil")
	}
	_, span := tr.Start(context.Background(), "noop")
	span.End()
}
