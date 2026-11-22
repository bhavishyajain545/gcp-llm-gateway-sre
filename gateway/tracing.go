package main

import (
	"context"
	"fmt"

	texporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

var tracer trace.Tracer = otel.Tracer("llm-gateway")

// setupTracing wires OpenTelemetry. With TRACE_EXPORTER=gcp spans go to Cloud
// Trace through the ambient credentials (Workload Identity); otherwise spans
// are created but dropped, so the code path is identical in tests and locally.
func setupTracing(ctx context.Context, cfg Config, version string) (func(context.Context) error, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("llm-gateway"),
			semconv.ServiceVersion(version),
		),
	)
	if err != nil {
		return nil, err
	}
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.TraceRatio))),
	}
	if cfg.TraceExporter == "gcp" {
		exp, err := texporter.New(texporter.WithProjectID(cfg.Project))
		if err != nil {
			return nil, fmt.Errorf("cloud trace exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	tracer = tp.Tracer("llm-gateway")
	return tp.Shutdown, nil
}
