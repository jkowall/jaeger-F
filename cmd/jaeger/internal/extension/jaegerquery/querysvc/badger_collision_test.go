// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package querysvc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/jaegertracing/jaeger/internal/jptrace/sanitizer"
	v1badger "github.com/jaegertracing/jaeger/internal/storage/v1/badger"
	v2badger "github.com/jaegertracing/jaeger/internal/storage/v2/badger"
	"github.com/jaegertracing/jaeger/internal/telemetry"
	"github.com/jaegertracing/jaeger/internal/telemetry/otelsemconv"
)

// TestFindTracesDoesNotReturnCollidingBadgerSpan is the query an operator runs.
// The collector sanitizer does not rewrite these names, and FindTraces must not
// add a trace stored under a different service and operation that share the
// joined index bytes.
func TestFindTracesDoesNotReturnCollidingBadgerSpan(t *testing.T) {
	cfg := v1badger.DefaultConfig()
	cfg.Ephemeral = true
	cfg.SyncWrites = false
	factory, err := v2badger.NewFactory(*cfg, telemetry.NoopSettings())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, factory.Close())
	})
	reader, err := factory.CreateTraceReader()
	require.NoError(t, err)
	writer, err := factory.CreateTraceWriter()
	require.NoError(t, err)

	base := time.Now().Add(-time.Minute)
	victimID := collisionTraceID(1)
	plantedID := collisionTraceID(2)
	traces := ptrace.NewTraces()
	appendCollisionSpan(traces, victimID, collisionSpanID(1), "a", "bc", base)
	appendCollisionSpan(traces, plantedID, collisionSpanID(2), "ab", "c", base.Add(time.Second))
	require.NoError(t, writer.WriteTraces(context.Background(), sanitizer.Sanitize(traces)))

	qs := NewQueryService(reader, nil, QueryServiceOptions{})
	for _, serviceOp := range []struct {
		service   string
		operation string
		want      pcommon.TraceID
	}{
		{service: "a", operation: "bc", want: victimID},
		{service: "ab", operation: "c", want: plantedID},
	} {
		attrs := pcommon.NewMap()
		var got []ptrace.Traces
		for batch, err := range qs.FindTraces(context.Background(), TraceQueryParams{
			ServiceName:   serviceOp.service,
			OperationName: serviceOp.operation,
			Attributes:    attrs,
			StartTimeMin:  base.Add(-time.Minute),
			StartTimeMax:  base.Add(time.Hour),
			SearchDepth:   20,
		}) {
			require.NoError(t, err)
			got = append(got, batch...)
		}
		require.Len(t, got, 1, "service=%q operation=%q", serviceOp.service, serviceOp.operation)
		require.Equal(t, 1, got[0].SpanCount())
		span := got[0].ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
		require.Equal(t, serviceOp.want, span.TraceID())
		require.Equal(t, serviceOp.operation, span.Name())
		serviceName, ok := got[0].ResourceSpans().At(0).Resource().Attributes().Get(otelsemconv.ServiceNameKey)
		require.True(t, ok)
		require.Equal(t, serviceOp.service, serviceName.Str())
	}
}

func appendCollisionSpan(td ptrace.Traces, traceID pcommon.TraceID, spanID pcommon.SpanID, service, operation string, start time.Time) {
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr(otelsemconv.ServiceNameKey, service)
	span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.SetTraceID(traceID)
	span.SetSpanID(spanID)
	span.SetName(operation)
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(start))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(start.Add(time.Millisecond)))
}

func collisionTraceID(last byte) pcommon.TraceID {
	id := pcommon.NewTraceIDEmpty()
	id[15] = last
	return id
}

func collisionSpanID(last byte) pcommon.SpanID {
	id := pcommon.NewSpanIDEmpty()
	id[7] = last
	return id
}
