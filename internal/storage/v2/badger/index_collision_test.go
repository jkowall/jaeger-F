// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package badger

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/jaegertracing/jaeger/internal/jptrace/sanitizer"
	v1badger "github.com/jaegertracing/jaeger/internal/storage/v1/badger"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
	"github.com/jaegertracing/jaeger/internal/telemetry"
	"github.com/jaegertracing/jaeger/internal/telemetry/otelsemconv"
)

// TestSubmittedSpanDoesNotAppearUnderCollidingService follows the collector
// write path: the storage exporter's sanitizer, then the Badger trace writer.
// A span whose service name and operation name join to the same bytes as
// another service's name and operation must not be returned for that other
// service. The query service is covered in querysvc.
func TestSubmittedSpanDoesNotAppearUnderCollidingService(t *testing.T) {
	reader, writer := newBadger(t)
	base := time.Now().Add(-time.Minute)
	victimID := traceID(1)
	plantedID := traceID(2)

	traces := ptrace.NewTraces()
	appendSpan(traces, otelSpan{id: victimID, span: spanID(1), service: "a", operation: "bc", start: base})
	appendSpan(traces, otelSpan{id: plantedID, span: spanID(2), service: "ab", operation: "c", start: base.Add(time.Second)})
	// storageexporter.TraceWriter.WriteTraces sanitizes, then writes.
	require.NoError(t, writer.WriteTraces(context.Background(), sanitizer.Sanitize(traces)))

	for _, serviceOp := range []struct {
		service   string
		operation string
		want      pcommon.TraceID
	}{
		{service: "a", operation: "bc", want: victimID},
		{service: "ab", operation: "c", want: plantedID},
	} {
		params := traceQuery(base, serviceOp.service, serviceOp.operation, nil)
		assertOnlyOTELTrace(t, readTraces(t, reader, params), serviceOp.want, serviceOp.service, serviceOp.operation, nil)
		require.Equal(t, []pcommon.TraceID{serviceOp.want}, readTraceIDs(t, reader, params))
	}
}

func TestSubmittedTagsDoNotMatchCollidingKey(t *testing.T) {
	reader, writer := newBadger(t)
	base := time.Now().Add(-time.Minute)
	victimID := traceID(3)
	plantedID := traceID(4)

	traces := ptrace.NewTraces()
	appendSpan(traces, otelSpan{
		id: victimID, span: spanID(3), service: "a", operation: "op-a", start: base,
		spanAttrs: map[string]string{"bc": "d"}, resourceAttrs: map[string]string{"bc": "d"},
		eventKey: "bc", eventValue: "d",
	})
	appendSpan(traces, otelSpan{
		id: plantedID, span: spanID(4), service: "ab", operation: "op-ab", start: base.Add(time.Second),
		spanAttrs: map[string]string{"c": "d"}, resourceAttrs: map[string]string{"c": "d"},
		eventKey: "c", eventValue: "d",
	})
	require.NoError(t, writer.WriteTraces(context.Background(), sanitizer.Sanitize(traces)))

	// Span attribute, resource attribute, and span event are indexed as tags.
	// Each pair joins to the same bytes across the two services.
	for _, key := range []string{"bc", "c"} {
		service := "a"
		want := victimID
		if key == "c" {
			service = "ab"
			want = plantedID
		}
		params := traceQuery(base, service, "", map[string]string{key: "d"})
		got := readTraces(t, reader, params)
		require.Len(t, got, 1)
		require.Equal(t, want, got[0].ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).TraceID())
		require.Equal(t, service, serviceOf(t, got[0]))
	}
}

func newBadger(t *testing.T) (tracestore.Reader, tracestore.Writer) {
	t.Helper()
	cfg := v1badger.DefaultConfig()
	cfg.Ephemeral = true
	cfg.SyncWrites = false
	factory, err := NewFactory(*cfg, telemetry.NoopSettings())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, factory.Close())
	})
	reader, err := factory.CreateTraceReader()
	require.NoError(t, err)
	writer, err := factory.CreateTraceWriter()
	require.NoError(t, err)
	return reader, writer
}

type otelSpan struct {
	id            pcommon.TraceID
	span          pcommon.SpanID
	service       string
	operation     string
	start         time.Time
	spanAttrs     map[string]string
	resourceAttrs map[string]string
	eventKey      string
	eventValue    string
}

func appendSpan(td ptrace.Traces, in otelSpan) {
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr(otelsemconv.ServiceNameKey, in.service)
	for key, value := range in.resourceAttrs {
		rs.Resource().Attributes().PutStr(key, value)
	}
	span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.SetTraceID(in.id)
	span.SetSpanID(in.span)
	span.SetName(in.operation)
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(in.start))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(in.start.Add(time.Millisecond)))
	for key, value := range in.spanAttrs {
		span.Attributes().PutStr(key, value)
	}
	if in.eventKey != "" {
		event := span.Events().AppendEmpty()
		event.SetTimestamp(pcommon.NewTimestampFromTime(in.start))
		event.Attributes().PutStr(in.eventKey, in.eventValue)
	}
}

func traceQuery(base time.Time, service, operation string, tags map[string]string) tracestore.TraceQueryParams {
	attrs := pcommon.NewMap()
	for key, value := range tags {
		attrs.PutStr(key, value)
	}
	return tracestore.TraceQueryParams{
		ServiceName:   service,
		OperationName: operation,
		Attributes:    attrs,
		StartTimeMin:  base.Add(-time.Minute),
		StartTimeMax:  base.Add(time.Hour),
		SearchDepth:   20,
	}
}

func readTraces(t *testing.T, reader tracestore.Reader, q tracestore.TraceQueryParams) []ptrace.Traces {
	t.Helper()
	var out []ptrace.Traces
	for batch, err := range reader.FindTraces(context.Background(), q) {
		require.NoError(t, err)
		out = append(out, batch...)
	}
	return out
}

func readTraceIDs(t *testing.T, reader tracestore.Reader, q tracestore.TraceQueryParams) []pcommon.TraceID {
	t.Helper()
	var ids []pcommon.TraceID
	for chunk, err := range reader.FindTraceIDs(context.Background(), q) {
		require.NoError(t, err)
		for _, id := range chunk.Results {
			ids = append(ids, id.TraceID)
		}
	}
	return ids
}

func assertOnlyOTELTrace(t *testing.T, traces []ptrace.Traces, want pcommon.TraceID, service, operation string, tags map[string]string) {
	t.Helper()
	require.Len(t, traces, 1)
	spans := 0
	for i := 0; i < traces[0].ResourceSpans().Len(); i++ {
		rs := traces[0].ResourceSpans().At(i)
		require.Equal(t, service, serviceOfResource(t, rs))
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			ss := rs.ScopeSpans().At(j).Spans()
			for k := 0; k < ss.Len(); k++ {
				span := ss.At(k)
				spans++
				require.Equal(t, want, span.TraceID())
				if operation != "" {
					require.Equal(t, operation, span.Name())
				}
				for key, value := range tags {
					attr, ok := span.Attributes().Get(key)
					require.True(t, ok)
					require.Equal(t, value, attr.Str())
				}
			}
		}
	}
	require.Equal(t, 1, spans)
}

func serviceOf(t *testing.T, td ptrace.Traces) string {
	t.Helper()
	require.Positive(t, td.ResourceSpans().Len())
	return serviceOfResource(t, td.ResourceSpans().At(0))
}

func serviceOfResource(t *testing.T, rs ptrace.ResourceSpans) string {
	t.Helper()
	value, ok := rs.Resource().Attributes().Get(otelsemconv.ServiceNameKey)
	require.True(t, ok)
	return value.Str()
}

func traceID(last byte) pcommon.TraceID {
	id := pcommon.NewTraceIDEmpty()
	id[15] = last
	return id
}

func spanID(last byte) pcommon.SpanID {
	id := pcommon.NewSpanIDEmpty()
	id[7] = last
	return id
}
