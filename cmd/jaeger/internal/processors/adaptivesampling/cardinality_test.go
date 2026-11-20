// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package adaptivesampling

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap/zaptest"

	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/remotesampling"
	"github.com/jaegertracing/jaeger/internal/sampling/samplingstrategy/adaptive"
)

// Same bounds as internal/sampling/samplingstrategy/adaptive/cardinality_test.go.
// processTraces is the pipeline entry that turns receiver output into the
// aggregator's maps, so the cap has to hold here too.
const (
	maxAdaptiveSamplingOperations  = 1024
	maxAdaptiveSamplingNameLength  = 256
	adaptiveSamplingAttackMultiple = 3
)

func TestProcessTracesRetainsBoundedOperationNames(t *testing.T) {
	ctx := context.Background()
	telemetry := component.TelemetrySettings{
		Logger:         zaptest.NewLogger(t),
		TracerProvider: nooptrace.NewTracerProvider(),
		MeterProvider:  noopmetric.NewMeterProvider(),
	}
	opts := adaptive.DefaultOptions()
	// The aggregation loop must not flush or clear the map during this test.
	opts.CalculationInterval = time.Hour
	opts.Delay = 0
	opts.AggregationBuckets = 2

	processor, host := startAdaptiveProcessor(t, ctx, telemetry, opts)
	defer func() {
		require.NoError(t, processor.close(ctx))
	}()

	long := strings.Repeat("n", maxAdaptiveSamplingNameLength*16)
	traces := ptrace.NewTraces()
	appendRootSpan(&traces, long, long, true)
	n := maxAdaptiveSamplingOperations * adaptiveSamplingAttackMultiple
	for i := range n {
		appendRootSpan(&traces, fmt.Sprintf("svc-%d", i), fmt.Sprintf("op-%d", i), true)
	}
	// A child span and a root span without sampler tags must not be retained.
	appendChildSpan(&traces, "payments", "child-op")
	appendRootSpan(&traces, "payments", "missing-sampler", false)

	out, err := processor.processTraces(ctx, traces)
	require.NoError(t, err)
	require.Equal(t, n+3, out.SpanCount())

	names := operationNames(t, processor.aggregator)
	require.NotContains(t, names, "payments\x00child-op")
	require.NotContains(t, names, "payments\x00missing-sampler")
	require.NotEmpty(t, names)
	require.LessOrEqual(t, len(names), maxAdaptiveSamplingOperations)
	for _, name := range names {
		service, operation, ok := strings.Cut(name, "\x00")
		require.True(t, ok)
		require.LessOrEqual(t, len(service), maxAdaptiveSamplingNameLength)
		require.LessOrEqual(t, len(operation), maxAdaptiveSamplingNameLength)
	}

	// The extension started for this processor is shut down by t.Cleanup.
	require.NotNil(t, host)
}

func startAdaptiveProcessor(t *testing.T, ctx context.Context, telemetry component.TelemetrySettings, opts adaptive.Options) (*traceProcessor, component.Host) {
	t.Helper()
	cfg := createDefaultConfig().(*Config)
	processor := newTraceProcessor(*cfg, telemetry)

	rsCfg := &remotesampling.Config{
		Adaptive: configoptional.Some(remotesampling.AdaptiveConfig{
			SamplingStore: "foobar",
			Options:       opts,
		}),
	}
	host := makeRemoteSamplingExtension(t, rsCfg)
	require.NoError(t, processor.start(ctx, host))
	return processor, host
}

func appendRootSpan(traces *ptrace.Traces, service, operation string, withSampler bool) {
	span := appendSpan(traces, service, operation)
	if withSampler {
		span.Attributes().PutStr("sampler.type", "probabilistic")
		span.Attributes().PutStr("sampler.param", "0.001")
	}
}

func appendChildSpan(traces *ptrace.Traces, service, operation string) {
	span := appendSpan(traces, service, operation)
	span.SetParentSpanID(pcommon.SpanID{1, 2, 3, 4, 5, 6, 7, 8})
	span.Attributes().PutStr("sampler.type", "probabilistic")
	span.Attributes().PutStr("sampler.param", "0.001")
}

func appendSpan(traces *ptrace.Traces, service, operation string) ptrace.Span {
	rs := traces.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", service)
	span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	var traceID pcommon.TraceID
	var spanID pcommon.SpanID
	n := byte(traces.ResourceSpans().Len())
	traceID[15] = n
	spanID[7] = n
	span.SetTraceID(traceID)
	span.SetSpanID(spanID)
	span.SetName(operation)
	return span
}

// operationNames reads the aggregator's unexported throughput map. The
// processor only exposes the Aggregator interface, which has no length query.
func operationNames(t *testing.T, agg interface{}) []string {
	t.Helper()
	v := reflect.ValueOf(agg)
	require.Equal(t, reflect.Pointer, v.Kind())
	field := v.Elem().FieldByName("currentThroughput")
	require.Equal(t, reflect.Map, field.Kind())

	var names []string
	for services := field.MapRange(); services.Next(); {
		service := services.Key().String()
		operations := services.Value()
		require.Equal(t, reflect.Map, operations.Kind())
		for ops := operations.MapRange(); ops.Next(); {
			names = append(names, service+"\x00"+ops.Key().String())
		}
	}
	return names
}
