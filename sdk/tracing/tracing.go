// Package tracing - OTel setup helper for workers + controlplane.
//
// Init reads the standard OTel envs:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT  e.g. http://jaeger:4318
//	OTEL_SERVICE_NAME            e.g. tm-httpx
//	OTEL_RESOURCE_ATTRIBUTES     e.g. deployment.environment=dev
//
// When OTEL_EXPORTER_OTLP_ENDPOINT is empty, Init returns a no-op
// shutdown - no exporter, no goroutines, no overhead. Workers /
// services who run without observability stay simple.
//
// Caller stashes the returned shutdown func and defers it; spans
// flush at process exit. Failure to export is logged once at boot
// and silenced afterwards (we'd rather lose traces than drown the
// log with retry chatter).
package tracing

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Init configures the global tracer provider. service is the value
// used for service.name when OTEL_SERVICE_NAME is unset (tools
// usually pass their manifest.tool).
//
// Returns:
//   - shutdown func that flushes pending spans (defer it in main)
//   - err if the exporter URL is set but the connection fails to
//     build. Empty URL → (no-op shutdown, nil err).
func Init(ctx context.Context, service string, logger *slog.Logger) (func(context.Context) error, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		// No-op tracer + identity propagator so call sites can use
		// otel.Tracer + otel.GetTextMapPropagator without checks.
		otel.SetTracerProvider(sdktrace.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{}))
		return func(context.Context) error { return nil }, nil
	}

	if name := os.Getenv("OTEL_SERVICE_NAME"); name != "" {
		service = name
	}
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(service),
		),
	)
	if err != nil {
		return nil, err
	}

	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(endpoint),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		// Sample everything in dev. Operators tune via
		// OTEL_TRACES_SAMPLER (e.g. parentbased_traceidratio,
		// arg=0.1) which the SDK honors automatically.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	if logger != nil {
		logger.Info("otel tracing enabled",
			"service", service, "endpoint", endpoint)
	}

	return tp.Shutdown, nil
}

// Tracer returns the named tracer. Equivalent to otel.Tracer(name).
// Wrapped here so consumers don't need to import otel directly for
// the common case.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// once-guard so multiple Init calls in a single process don't double-
// register the global provider. Subsequent calls are no-ops.
var initOnce sync.Once

// InitOnce wraps Init so the same SDK runtime can call it without
// caring whether the binary already initialized tracing elsewhere
// (e.g. TechMapper which has its own internal/tracing).
func InitOnce(ctx context.Context, service string, logger *slog.Logger) (func(context.Context) error, error) {
	var (
		shutdown func(context.Context) error
		err      error
	)
	initOnce.Do(func() {
		shutdown, err = Init(ctx, service, logger)
	})
	if shutdown == nil {
		shutdown = func(context.Context) error { return nil }
	}
	if err == nil {
		err = errors.Join() // nil
	}
	return shutdown, err
}
