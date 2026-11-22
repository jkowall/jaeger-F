// Copyright (c) 2023 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package jtracer

import (
	"context"
	"os"
	"strings"
	"sync"

	"go.opentelemetry.io/contrib/samplers/jaegerremote"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/jaegertracing/jaeger/internal/telemetry/otelsemconv"
)

var once sync.Once

func NewProvider(ctx context.Context, serviceName string) (trace.TracerProvider, func(ctx context.Context) error, error) {
	return newProviderHelper(ctx, serviceName, initOTEL)
}

func newProviderHelper(
	ctx context.Context,
	serviceName string,
	tracerProvider func(ctx context.Context, svc string) (*sdktrace.TracerProvider, func(), error),
) (trace.TracerProvider, func(ctx context.Context) error, error) {
	provider, closeSampler, err := tracerProvider(ctx, serviceName)
	if err != nil {
		return nil, nil, err
	}
	closer := func(ctx context.Context) error {
		if closeSampler != nil {
			closeSampler()
		}
		return provider.Shutdown(ctx)
	}
	return provider, closer, nil
}

// initOTEL initializes OTEL Tracer
func initOTEL(ctx context.Context, svc string) (*sdktrace.TracerProvider, func(), error) {
	return initHelper(ctx, svc, otelExporter, otelResource)
}

func initHelper(
	ctx context.Context,
	svc string,
	otelExporter func(_ context.Context) (sdktrace.SpanExporter, error),
	otelResource func(_ context.Context, _ /* svc */ string) (*resource.Resource, error),
) (*sdktrace.TracerProvider, func(), error) {
	res, err := otelResource(ctx, svc)
	if err != nil {
		return nil, nil, err
	}

	traceExporter, err := otelExporter(ctx)
	if err != nil {
		return nil, nil, err
	}

	// Register the trace exporter with a TracerProvider, using a batch
	// span processor to aggregate spans before export.
	bsp := sdktrace.NewBatchSpanProcessor(traceExporter)

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithSpanProcessor(bsp),
		sdktrace.WithResource(res),
	}

	var closeSampler func()
	if strings.ToLower(os.Getenv("OTEL_TRACES_SAMPLER")) == "jaeger_remote" {
		s := jaegerremote.New(svc)
		opts = append(opts, sdktrace.WithSampler(s))
		closeSampler = s.Close
	}

	tracerProvider := sdktrace.NewTracerProvider(opts...)

	once.Do(func() {
		otel.SetTextMapPropagator(
			propagation.NewCompositeTextMapPropagator(
				propagation.TraceContext{},
				propagation.Baggage{},
			),
		)
	})

	otel.SetTracerProvider(tracerProvider)

	return tracerProvider, closeSampler, nil
}

func otelResource(ctx context.Context, svc string) (*resource.Resource, error) {
	return resource.New(
		ctx,
		resource.WithAttributes(otelsemconv.ServiceNameAttribute(svc)),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithOSType(),
		resource.WithFromEnv(),
	)
}

const (
	// loopbackGRPCEndpoint and loopbackHTTPEndpoint are the OTLP exporters'
	// built-in default endpoints.
	loopbackGRPCEndpoint = "localhost:4317"
	loopbackHTTPEndpoint = "localhost:4318"
)

// otlpTargetEnv lists the variables through which an operator chooses where
// OTLP traces are exported and how that connection is secured.
var otlpTargetEnv = []string{
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	"OTEL_EXPORTER_OTLP_INSECURE",
	"OTEL_EXPORTER_OTLP_TRACES_INSECURE",
	"OTEL_EXPORTER_OTLP_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_CLIENT_KEY",
	"OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY",
}

// usesLoopbackDefault reports whether none of otlpTargetEnv is set, matching
// the exporters' own rule that a blank value counts as unset.
func usesLoopbackDefault() bool {
	for _, key := range otlpTargetEnv {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return false
		}
	}
	return true
}

// GRPCOptions returns the otlptracegrpc options Jaeger binaries pass to their
// exporters. Any configured endpoint is left to the exporter's environment
// handling, which uses TLS unless OTEL_EXPORTER_OTLP_[TRACES_]INSECURE is true
// or the endpoint scheme is http. Plaintext is applied only together with the
// loopback endpoint it was chosen for, when nothing is configured.
func GRPCOptions() []otlptracegrpc.Option {
	if !usesLoopbackDefault() {
		return nil
	}
	return []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(loopbackGRPCEndpoint),
		otlptracegrpc.WithInsecure(),
	}
}

// HTTPOptions is the otlptracehttp counterpart of GRPCOptions.
func HTTPOptions() []otlptracehttp.Option {
	if !usesLoopbackDefault() {
		return nil
	}
	return []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(loopbackHTTPEndpoint),
		otlptracehttp.WithInsecure(),
	}
}

func otelExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	client := otlptracegrpc.NewClient(
		GRPCOptions()...,
	)
	return otlptrace.New(ctx, client)
}
