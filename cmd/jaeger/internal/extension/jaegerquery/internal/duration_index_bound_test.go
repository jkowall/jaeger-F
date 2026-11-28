// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger-idl/proto-gen/api_v2"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/internal/apiv3"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/querysvc"
	builder "github.com/jaegertracing/jaeger/internal/expression"
	"github.com/jaegertracing/jaeger/internal/jiter"
	"github.com/jaegertracing/jaeger/internal/jptrace"
	storagepb "github.com/jaegertracing/jaeger/internal/proto-gen/storage/v2"
	"github.com/jaegertracing/jaeger/internal/proto/api_v3"
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/spanstore"
	"github.com/jaegertracing/jaeger/internal/storage/v1/badger"
	v2badger "github.com/jaegertracing/jaeger/internal/storage/v2/badger"
	storagegrpc "github.com/jaegertracing/jaeger/internal/storage/v2/grpc"
	"github.com/jaegertracing/jaeger/internal/storage/v2/v1adapter"
	"github.com/jaegertracing/jaeger/internal/telemetry"
)

// apiOutsideSpans is the number of spans that start before the search window.
// Each one becomes a duration-index key the search must not buffer.
const apiOutsideSpans = 400

// TestQueryAPIDurationSearchDoesNotBufferOutsideWindow drives every query entry
// point that copies a duration bound through to the Badger reader. A one-minute
// window with a 1 microsecond minimum, or a maximum and no minimum, must not
// allocate one key per span stored across the retention period.
func TestQueryAPIDurationSearchDoesNotBufferOutsideWindow(t *testing.T) {
	cfg := badger.DefaultConfig()
	cfg.Ephemeral = true
	cfg.TTL.Spans = 72 * time.Hour
	factory, err := v2badger.NewFactory(*cfg, telemetry.NoopSettings())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, factory.Close())
	})

	writer, err := factory.CreateTraceWriter()
	require.NoError(t, err)
	spanWriter := v1adapter.GetV1Writer(writer)

	windowEnd := time.Now().UTC().Truncate(time.Microsecond)
	windowStart := windowEnd.Add(-time.Minute)
	insideAt := windowStart.Add(10 * time.Second)
	beforeAt := windowStart.Add(-2 * time.Hour)

	writeAPISpan(t, spanWriter, model.TraceID{High: 1, Low: 1}, "svc", insideAt, 5*time.Millisecond)
	for i := range apiOutsideSpans {
		writeAPISpan(t, spanWriter, model.TraceID{High: 2, Low: uint64(i + 1)}, "other",
			beforeAt.Add(time.Duration(i)*time.Microsecond), 5*time.Millisecond)
	}

	reader, err := factory.CreateTraceReader()
	require.NoError(t, err)
	qs := querysvc.NewQueryService(reader, nil, querysvc.QueryServiceOptions{})
	gateway := &apiv3.HTTPGateway{
		QueryService: qs,
		Logger:       zap.NewNop(),
		Tracer:       nooptrace.NewTracerProvider(),
	}
	mux := http.NewServeMux()
	gateway.RegisterRoutes(mux)
	v3Handler := &apiv3.Handler{QueryService: qs}
	v2Handler := NewGRPCHandler(qs, GRPCHandlerOptions{})
	storageHandler := storagegrpc.NewHandler(reader, nil, nil)

	// A duration predicate over this window matches the same one trace as a search
	// with no duration predicate. It may allocate a little more to read that
	// trace's duration key, and must not allocate once per span stored earlier.
	const extraAllocs = 200.0

	requireNear := func(t *testing.T, what string, baseline, got float64) {
		t.Helper()
		require.LessOrEqualf(t, got, baseline+extraAllocs,
			"%s allocated %.0f times, %.0f more than the same search with no duration bound (%.0f)",
			what, got, got-baseline, baseline)
	}

	t.Run("http duration_min", func(t *testing.T) {
		baseline := testing.AllocsPerRun(3, func() {
			status := queryHTTP(mux, "/api/v3/traces", windowStart, windowEnd, nil)
			if status != http.StatusOK {
				panic(status)
			}
		})
		allocs := testing.AllocsPerRun(3, func() {
			status := queryHTTP(mux, "/api/v3/traces", windowStart, windowEnd, url.Values{
				"query.duration_min": {"1us"},
			})
			if status != http.StatusOK {
				panic(status)
			}
		})
		requireNear(t, "GET /api/v3/traces duration_min=1us", baseline, allocs)
	})

	t.Run("http durationMin", func(t *testing.T) {
		baseline := testing.AllocsPerRun(3, func() {
			status := queryHTTP(mux, "/api/v3/traces", windowStart, windowEnd, nil)
			if status != http.StatusOK {
				panic(status)
			}
		})
		allocs := testing.AllocsPerRun(3, func() {
			status := queryHTTP(mux, "/api/v3/traces", windowStart, windowEnd, url.Values{
				"query.durationMin": {"1us"},
			})
			if status != http.StatusOK {
				panic(status)
			}
		})
		requireNear(t, "GET /api/v3/traces durationMin=1us", baseline, allocs)
	})

	t.Run("http duration_max", func(t *testing.T) {
		baseline := testing.AllocsPerRun(3, func() {
			status := queryHTTP(mux, "/api/v3/traces", windowStart, windowEnd, nil)
			if status != http.StatusOK {
				panic(status)
			}
		})
		allocs := testing.AllocsPerRun(3, func() {
			status := queryHTTP(mux, "/api/v3/traces", windowStart, windowEnd, url.Values{
				"query.duration_max": {"10ms"},
			})
			if status != http.StatusOK {
				panic(status)
			}
		})
		requireNear(t, "GET /api/v3/traces duration_max=10ms", baseline, allocs)
	})

	t.Run("http trace summaries", func(t *testing.T) {
		baseline := testing.AllocsPerRun(3, func() {
			status := queryHTTP(mux, "/api/v3/trace-summaries", windowStart, windowEnd, nil)
			if status != http.StatusOK {
				panic(status)
			}
		})
		allocs := testing.AllocsPerRun(3, func() {
			status := queryHTTP(mux, "/api/v3/trace-summaries", windowStart, windowEnd, url.Values{
				"query.duration_min": {"1us"},
			})
			if status != http.StatusOK {
				panic(status)
			}
		})
		requireNear(t, "GET /api/v3/trace-summaries duration_min=1us", baseline, allocs)
	})

	v3Query := func(durationMin time.Duration) *api_v3.FindTracesRequest {
		return &api_v3.FindTracesRequest{
			Query: &api_v3.TraceQueryParameters{
				ServiceName:  "svc",
				StartTimeMin: windowStart,
				StartTimeMax: windowEnd,
				DurationMin:  durationMin,
				SearchDepth:  20,
			},
		}
	}
	t.Run("grpc v3 FindTraces", func(t *testing.T) {
		baseline := testing.AllocsPerRun(3, func() {
			if err := v3Handler.FindTraces(v3Query(0), discardV3Stream{}); err != nil {
				panic(err)
			}
		})
		allocs := testing.AllocsPerRun(3, func() {
			if err := v3Handler.FindTraces(v3Query(time.Microsecond), discardV3Stream{}); err != nil {
				panic(err)
			}
		})
		requireNear(t, "api_v3 FindTraces", baseline, allocs)
	})

	v2Query := func(durationMin time.Duration) *api_v2.FindTracesRequest {
		return &api_v2.FindTracesRequest{
			Query: &api_v2.TraceQueryParameters{
				ServiceName:  "svc",
				StartTimeMin: windowStart,
				StartTimeMax: windowEnd,
				DurationMin:  durationMin,
				SearchDepth:  20,
			},
		}
	}
	t.Run("grpc v2 FindTraces", func(t *testing.T) {
		baseline := testing.AllocsPerRun(3, func() {
			if err := v2Handler.FindTraces(v2Query(0), discardV2Stream{}); err != nil {
				panic(err)
			}
		})
		allocs := testing.AllocsPerRun(3, func() {
			if err := v2Handler.FindTraces(v2Query(time.Microsecond), discardV2Stream{}); err != nil {
				panic(err)
			}
		})
		requireNear(t, "api_v2 FindTraces", baseline, allocs)
	})

	storageQuery := func(durationMin time.Duration) *storagepb.FindTracesRequest {
		return &storagepb.FindTracesRequest{
			Query: &storagepb.TraceQueryParameters{
				ServiceName:  "svc",
				StartTimeMin: windowStart,
				StartTimeMax: windowEnd,
				DurationMin:  durationMin,
				SearchDepth:  20,
			},
		}
	}
	t.Run("storage grpc FindTraces", func(t *testing.T) {
		baseline := testing.AllocsPerRun(3, func() {
			if err := storageHandler.FindTraces(storageQuery(0), discardStorageStream{}); err != nil {
				panic(err)
			}
		})
		allocs := testing.AllocsPerRun(3, func() {
			if err := storageHandler.FindTraces(storageQuery(time.Microsecond), discardStorageStream{}); err != nil {
				panic(err)
			}
		})
		requireNear(t, "storage FindTraces", baseline, allocs)
	})

	t.Run("duration filter rewritten to legacy bound", func(t *testing.T) {
		var p builder.Predicate
		plain := querysvc.TraceQueryParams{
			ServiceName:  "svc",
			Attributes:   pcommon.NewMap(),
			StartTimeMin: windowStart,
			StartTimeMax: windowEnd,
		}
		filtered := plain
		filtered.ServiceName = ""
		filtered.Filter = p.And(
			p.Resource().Service.Eq("svc"),
			p.Span().Duration.Gte("1us"),
		)
		baseline := testing.AllocsPerRun(3, func() {
			traces, err := jiter.FlattenWithErrors(qs.FindTraces(context.Background(), plain))
			if err != nil {
				panic(err)
			}
			if len(traces) == 0 {
				panic("expected the in-window trace")
			}
		})
		allocs := testing.AllocsPerRun(3, func() {
			traces, err := jiter.FlattenWithErrors(qs.FindTraces(context.Background(), filtered))
			if err != nil {
				panic(err)
			}
			if len(traces) == 0 {
				panic("expected the in-window trace")
			}
		})
		requireNear(t, "filter duration >= 1us", baseline, allocs)
	})
}

func queryHTTP(mux *http.ServeMux, path string, start, end time.Time, extra url.Values) int {
	q := url.Values{}
	q.Set("query.service_name", "svc")
	q.Set("query.start_time_min", start.Format(time.RFC3339Nano))
	q.Set("query.start_time_max", end.Format(time.RFC3339Nano))
	for key, values := range extra {
		for _, value := range values {
			q.Add(key, value)
		}
	}
	req := httptest.NewRequest(http.MethodGet, path+"?"+q.Encode(), http.NoBody)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

func writeAPISpan(t *testing.T, writer spanstore.Writer, id model.TraceID, service string, start time.Time, duration time.Duration) {
	t.Helper()
	err := writer.WriteSpan(context.Background(), &model.Span{
		TraceID:       id,
		SpanID:        model.SpanID(id.Low),
		OperationName: "op",
		Process:       &model.Process{ServiceName: service},
		StartTime:     start,
		Duration:      duration,
	})
	require.NoError(t, err)
}

type discardV3Stream struct{ grpc.ServerStream }

func (discardV3Stream) Context() context.Context { return context.Background() }

func (discardV3Stream) Send(*jptrace.TracesData) error { return nil }

type discardV2Stream struct{ grpc.ServerStream }

func (discardV2Stream) Context() context.Context { return context.Background() }

func (discardV2Stream) Send(*api_v2.SpansResponseChunk) error { return nil }

type discardStorageStream struct{ grpc.ServerStream }

func (discardStorageStream) Context() context.Context { return context.Background() }

func (discardStorageStream) Send(*jptrace.TracesData) error { return nil }
