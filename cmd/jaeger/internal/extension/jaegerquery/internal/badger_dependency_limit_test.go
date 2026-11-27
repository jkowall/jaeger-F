// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger-idl/proto-gen/api_v2"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/internal/apiv3"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/querysvc"
	"github.com/jaegertracing/jaeger/internal/proto-gen/storage/v2"
	"github.com/jaegertracing/jaeger/internal/proto/api_v3"
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/spanstore"
	v1badger "github.com/jaegertracing/jaeger/internal/storage/v1/badger"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/depstore"
	v2badger "github.com/jaegertracing/jaeger/internal/storage/v2/badger"
	storagegrpc "github.com/jaegertracing/jaeger/internal/storage/v2/grpc"
	"github.com/jaegertracing/jaeger/internal/storage/v2/v1adapter"
	"github.com/jaegertracing/jaeger/internal/telemetry"
)

// maxDependencyTraces matches the Badger trace reader's default search limit.
// Every dependencies entry point below currently accepts any positive window
// and the Badger reader then lifts that limit to math.MaxUint32.
const maxDependencyTraces = 100

func openQueryBadger(t *testing.T) (spanstore.Writer, depstore.Reader) {
	t.Helper()
	cfg := *v1badger.DefaultConfig()
	cfg.Ephemeral = true
	cfg.SyncWrites = false
	factory, err := v2badger.NewFactory(cfg, telemetry.NoopSettings())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, factory.Close())
	})
	traceWriter, err := factory.CreateTraceWriter()
	require.NoError(t, err)
	dependencyReader, err := factory.CreateDependencyReader()
	require.NoError(t, err)
	return v1adapter.GetV1Writer(traceWriter), dependencyReader
}

func seedDependencyTraces(t *testing.T, writer spanstore.Writer, n int, start time.Time) {
	t.Helper()
	for i := range n {
		traceID := model.TraceID{Low: uint64(i + 1), High: 11}
		parent := &model.Span{
			TraceID:       traceID,
			SpanID:        1,
			OperationName: "request",
			Process:       &model.Process{ServiceName: "frontend"},
			StartTime:     start.Add(time.Duration(i) * time.Millisecond),
			Duration:      time.Millisecond,
		}
		child := &model.Span{
			TraceID:       traceID,
			SpanID:        2,
			OperationName: "request",
			Process:       &model.Process{ServiceName: "payments-" + strconv.Itoa(i)},
			StartTime:     parent.StartTime,
			Duration:      time.Millisecond,
			References:    []model.SpanRef{model.NewChildOfRef(traceID, parent.SpanID)},
		}
		require.NoError(t, writer.WriteSpan(context.Background(), parent))
		require.NoError(t, writer.WriteSpan(context.Background(), child))
	}
}

func dependencyQueryService(t *testing.T, reader depstore.Reader) *querysvc.QueryService {
	t.Helper()
	return querysvc.NewQueryService(nil, reader, querysvc.QueryServiceOptions{})
}

func requireBoundedEdges(t *testing.T, edges int, err error) {
	t.Helper()
	if err != nil {
		return
	}
	require.LessOrEqualf(t, edges, maxDependencyTraces,
		"the dependencies call returned %d edges from a window with more traces than the cap", edges)
}

// TestDependenciesEndpointsDoNotMaterializeEveryBadgerTrace drives the query
// HTTP route, both query gRPC versions, and the storage gRPC dependency
// method through the Badger reader the query service wires up. Each one
// accepts a day-long or multi-year window and, while the vulnerability is
// present, gets back one edge per stored trace.
func TestDependenciesEndpointsDoNotMaterializeEveryBadgerTrace(t *testing.T) {
	writer, reader := openQueryBadger(t)
	end := time.Now().Add(time.Minute).UTC()
	const stored = maxDependencyTraces + 50
	seedDependencyTraces(t, writer, stored, end.Add(-time.Hour))
	longWindow := 10 * 365 * 24 * time.Hour

	t.Run("http default lookback", func(t *testing.T) {
		edges, err := httpDependencyEdges(t, reader, end, 0)
		requireBoundedEdges(t, edges, err)
	})
	t.Run("http multi-year lookback", func(t *testing.T) {
		edges, err := httpDependencyEdges(t, reader, end, longWindow)
		requireBoundedEdges(t, edges, err)
	})
	t.Run("grpc v2", func(t *testing.T) {
		handler := NewGRPCHandler(dependencyQueryService(t, reader), GRPCHandlerOptions{Logger: zap.NewNop()})
		resp, err := handler.GetDependencies(context.Background(), &api_v2.GetDependenciesRequest{
			StartTime: end.Add(-longWindow),
			EndTime:   end,
		})
		edges := 0
		if err == nil {
			edges = len(resp.GetDependencies())
		}
		requireBoundedEdges(t, edges, err)
	})
	t.Run("grpc v3", func(t *testing.T) {
		handler := &apiv3.Handler{QueryService: dependencyQueryService(t, reader)}
		resp, err := handler.GetDependencies(context.Background(), &api_v3.GetDependenciesRequest{
			StartTime: end.Add(-longWindow),
			EndTime:   end,
		})
		edges := 0
		if err == nil {
			edges = len(resp.GetDependencies())
		}
		requireBoundedEdges(t, edges, err)
	})
	t.Run("storage grpc", func(t *testing.T) {
		handler := storagegrpc.NewHandler(nil, nil, reader)
		resp, err := handler.GetDependencies(context.Background(), &storage.GetDependenciesRequest{
			StartTime: end.Add(-longWindow),
			EndTime:   end,
		})
		edges := 0
		if err == nil {
			edges = len(resp.GetDependencies())
		}
		requireBoundedEdges(t, edges, err)
	})
}

// httpDependencyEdges sends GET /api/dependencies. lookback 0 leaves the
// parameter off so the handler applies its default of 24h.
func httpDependencyEdges(t *testing.T, reader depstore.Reader, end time.Time, lookback time.Duration) (int, error) {
	t.Helper()
	handler := NewAPIHandler(dependencyQueryService(t, reader))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	query := url.Values{}
	query.Set(endTsParam, strconv.FormatInt(end.UnixMilli(), 10))
	if lookback > 0 {
		query.Set(lookbackParam, strconv.FormatInt(lookback.Milliseconds(), 10))
	}
	req := httptest.NewRequest(http.MethodGet, "/api/dependencies?"+query.Encode(), http.NoBody)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return 0, httpStatusError{code: rec.Code, body: rec.Body.String()}
	}
	var body struct {
		Data []struct {
			Parent    string `json:"parent"`
			Child     string `json:"child"`
			CallCount uint64 `json:"callCount"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return len(body.Data), nil
}

type httpStatusError struct {
	code int
	body string
}

func (e httpStatusError) Error() string {
	return "http status " + strconv.Itoa(e.code) + ": " + e.body
}
