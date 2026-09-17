// SPDX-License-Identifier: AGPL-3.0-or-later

// Package tracing wires OpenTelemetry into the Keystone manager.
// Optional — if --otel-endpoint is empty, all spans are no-ops via
// noop.NewTracerProvider.
//
// Endpoint convention: HTTP/protobuf to an OTLP collector
// (e.g. https://otel-collector.observability:4318). gRPC is supported
// in OTel's Go SDK but the HTTP transport is simpler for our hub model.
package tracing

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	noop "go.opentelemetry.io/otel/trace/noop"
)

// ProviderConfig is what the manager passes after parsing flags.
type ProviderConfig struct {
	// Endpoint is an OTLP HTTP URL (e.g. "https://collector:4318").
	// Empty = use a no-op tracer (all spans are silent).
	Endpoint string

	// Insecure skips TLS verification on the collector connection.
	// Use for in-cluster collectors with self-signed certs.
	Insecure bool

	// ServiceVersion is filled into the standard service.version
	// Resource attribute so traces correlate with the binary that
	// produced them.
	ServiceVersion string

	// SampleRatio is the parent-based sampler ratio (0.0–1.0).
	// 1.0 = sample everything; production default 0.1.
	SampleRatio float64
}

// Init returns a TracerProvider + a shutdown func. Always returns a
// usable TracerProvider — no-op when endpoint is empty.
func Init(ctx context.Context, cfg ProviderConfig) (trace.TracerProvider, func(context.Context) error, error) {
	if cfg.Endpoint == "" {
		// No-op path. Still install propagators so any incoming
		// trace context is preserved across boundaries.
		otel.SetTextMapPropagator(propagation.TraceContext{})
		tp := noop.NewTracerProvider()
		otel.SetTracerProvider(tp)
		return tp, func(context.Context) error { return nil }, nil
	}

	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(cfg.Endpoint),
		otlptracehttp.WithCompression(otlptracehttp.GzipCompression),
		otlptracehttp.WithTimeout(10 * time.Second),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exp, err := otlptrace.New(ctx, otlptracehttp.NewClient(opts...))
	if err != nil {
		return nil, nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("keystone-manager"),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build resource: %w", err)
	}

	sampler := tracesdk.ParentBased(tracesdk.TraceIDRatioBased(cfg.SampleRatio))
	tp := tracesdk.NewTracerProvider(
		tracesdk.WithSampler(sampler),
		tracesdk.WithResource(res),
		tracesdk.WithBatcher(exp,
			tracesdk.WithBatchTimeout(5*time.Second),
			tracesdk.WithMaxExportBatchSize(512),
		),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	shutdown := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return tp.Shutdown(ctx)
	}
	return tp, shutdown, nil
}

// Tracer returns the global Keystone tracer. All controllers should
// use this single tracer name so spans group correctly.
func Tracer() trace.Tracer {
	return otel.Tracer("keystone.hexxlock.io/manager")
}
