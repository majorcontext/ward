// Package telemetry sets up the OTLP export of traces and metrics.
package telemetry

import (
	"context"
	"os"
	"strings"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	promebridge "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const flushTimeout = 5 * time.Second

// Setup starts the export of traces when enabled is true. The endpoint, the headers, the sampler and the service name
// come from the OTEL_* environment, and the service name is ward when the environment sets none. The returned function
// flushes and stops the export.
func Setup(ctx context.Context, enabled bool) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if !enabled {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	res, err := serviceResource()
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// serviceResource names the service ward, unless OTEL_SERVICE_NAME or OTEL_RESOURCE_ATTRIBUTES names it. Traces and metrics share it.
func serviceResource() (*resource.Resource, error) {
	if os.Getenv("OTEL_SERVICE_NAME") != "" || strings.Contains(os.Getenv("OTEL_RESOURCE_ATTRIBUTES"), "service.name=") {
		return resource.Default(), nil
	}
	return resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName("ward")))
}

// SetupMetrics exports the metrics of reg over OTLP, on the interval of OTEL_METRIC_EXPORT_INTERVAL, when enabled is
// true. The returned function flushes and stops the export.
func SetupMetrics(ctx context.Context, enabled bool, reg promclient.Gatherer) (func(context.Context) error, error) {
	if !enabled {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	reader := sdkmetric.NewPeriodicReader(exp, sdkmetric.WithProducer(promebridge.NewMetricProducer(promebridge.WithGatherer(reg))))
	res, err := serviceResource()
	if err != nil {
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(res))
	otel.SetMeterProvider(mp)
	return mp.Shutdown, nil
}
