// Package telemetry sets up the OTLP trace export.
package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Setup starts the export when enabled is true. The endpoint comes from the OTEL_EXPORTER_OTLP_* environment.
// The returned function flushes and stops the export.
func Setup(ctx context.Context, enabled bool) (func(context.Context) error, error) {
	if !enabled {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(resource.NewSchemaless(semconv.ServiceName("ward"))))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}
