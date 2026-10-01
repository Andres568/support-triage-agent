package obs

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// EndpointEnv enables tracing. otlptracegrpc reads it and the other standard
// OTEL_EXPORTER_OTLP_* variables itself.
const EndpointEnv = "OTEL_EXPORTER_OTLP_ENDPOINT"

// TracerName is the instrumentation scope of every span this project starts.
const TracerName = "github.com/Andres568/support-triage-agent"

// Setup installs the global tracer provider: OTLP over gRPC when
// OTEL_EXPORTER_OTLP_ENDPOINT is set, otherwise the default no-op. The
// returned shutdown flushes buffered spans and must run before the process
// exits: a batch job that skips it loses its last spans.
func Setup(ctx context.Context, service string) (shutdown func(context.Context) error, err error) {
	if os.Getenv(EndpointEnv) == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, err
	}
	// Schemaless, so it merges with the default resource whatever its schema.
	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(attribute.String("service.name", service)))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}
