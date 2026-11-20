// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jaegertracing/jaeger-idl/proto-gen/api_v2"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/internal/apiv3"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/querysvc"
	"github.com/jaegertracing/jaeger/internal/proto/api_v3"
	es "github.com/jaegertracing/jaeger/internal/storage/elasticsearch"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/config"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/esclient"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/indices"
	esmetrics "github.com/jaegertracing/jaeger/internal/storage/metricstore/elasticsearch"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
	esv2 "github.com/jaegertracing/jaeger/internal/storage/v2/elasticsearch"
	v2dep "github.com/jaegertracing/jaeger/internal/storage/v2/elasticsearch/depstore"
	v2trace "github.com/jaegertracing/jaeger/internal/storage/v2/elasticsearch/tracestore"
	"github.com/jaegertracing/jaeger/internal/storage/v2/elasticsearch/tracestore/core"
	"github.com/jaegertracing/jaeger/internal/telemetry"
)

// A periodic Elasticsearch deployment accepts a search as wide as the configured
// max span age and refuses anything longer with a client error. The refusal
// happens before index names are built and before the search URL is sent.
// Query authentication is left unset, which is the default: the request carries
// no credential and must still be answered, not rejected as unauthorized.
func TestQueryAPIRejectsUncappedPeriodicWindow(t *testing.T) {
	cfg := esv2.DefaultConfig()
	maxSpanAge := cfg.MaxSpanAge
	end := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	shortStart := end.Add(-maxSpanAge)
	longStart := end.Add(-20 * 365 * 24 * time.Hour)

	cap := &searchCapture{}
	searcher := cap.searcher(t)
	spanRaw := indices.BuildRotation(cfg.Indices.IndexPrefix, config.SpanIndexName, cfg.ResolvedSpanRotation(), nil, zap.NewNop())
	depRaw := indices.BuildRotation(cfg.Indices.IndexPrefix, config.DependencyIndexName, cfg.ResolvedDependencyRotation(), nil, zap.NewNop())
	spanRot := &recordingRotation{inner: spanRaw}
	depRot := &recordingRotation{inner: depRaw}
	acceptedSpans := len(spanRaw.ReadTargets(shortStart, end))
	require.Greater(t, acceptedSpans, 0)

	reader := v2trace.NewTraceReader(core.SpanReaderParams{
		Searcher:            searcher,
		MaxSpanAge:          maxSpanAge,
		ServicesMaxLookback: maxSpanAge,
		MaxTraceDuration:    cfg.MaxTraceDuration,
		MaxDocCount:         cfg.MaxDocCount,
		TagDotReplacement:   "@",
		Logger:              zap.NewNop(),
		Tracer:              noop.NewTracerProvider().Tracer("test"),
		SpanRotation:        spanRot,
		ServiceRotation:     spanRot,
	})
	deps := v2dep.NewDependencyStoreV2(v2dep.Params{
		Searcher:    searcher,
		Logger:      zap.NewNop(),
		MaxDocCount: cfg.MaxDocCount,
		Rotation:    depRot,
	})
	qs := querysvc.NewQueryService(reader, deps, querysvc.QueryServiceOptions{})
	metricsReader := esmetrics.NewMetricsReader(searcher, cfg, zap.NewNop(), noop.NewTracerProvider(), spanRot)

	opts := DefaultQueryOptions()
	require.False(t, opts.HTTP.Auth.HasValue(), "query HTTP auth is unset unless configured")
	telset := telemetry.NoopSettings()
	telset.Logger = zap.NewNop()
	telset.TracerProvider = noop.NewTracerProvider()
	server, err := createHTTPServer(context.Background(), qs, metricsReader, &opts, noBackendCaps, noopTenancyMgr(), telset)
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })

	grpcV2 := NewGRPCHandler(qs, GRPCHandlerOptions{})
	grpcV3 := &apiv3.Handler{QueryService: qs}

	reset := func() {
		cap.uris = nil
		spanRot.widest = 0
		depRot.widest = 0
	}
	requireClientError := func(t *testing.T, code int, widest time.Duration) {
		t.Helper()
		listed := 0
		for _, uri := range cap.uris {
			listed += indexCount(uri)
		}
		if code != http.StatusBadRequest || widest > maxSpanAge+time.Hour || len(cap.uris) != 0 {
			t.Fatalf("status=%d widest=%s searches=%d names=%d; an unauthenticated caller must get 400 before any index name is built",
				code, widest, len(cap.uris), listed)
		}
	}

	t.Run("http traces", func(t *testing.T) {
		reset()
		code := serve(t, server, "/api/v3/traces?"+traceQuery(shortStart, end))
		require.NotEqual(t, http.StatusBadRequest, code)
		require.NotEqual(t, http.StatusUnauthorized, code)
		require.NotEmpty(t, cap.uris)
		require.LessOrEqual(t, indexCount(cap.uris[0]), acceptedSpans)

		reset()
		code = serve(t, server, "/api/v3/traces?"+traceQuery(longStart, end))
		requireClientError(t, code, spanRot.widest)
	})

	t.Run("http spans", func(t *testing.T) {
		reset()
		code := serve(t, server, "/api/v3/spans?"+traceQuery(shortStart, end))
		require.NotEqual(t, http.StatusBadRequest, code)
		require.NotEmpty(t, cap.uris)

		reset()
		code = serve(t, server, "/api/v3/spans?"+traceQuery(longStart, end))
		requireClientError(t, code, spanRot.widest)
	})

	t.Run("http trace summaries", func(t *testing.T) {
		reset()
		code := serve(t, server, "/api/v3/trace-summaries?"+traceQuery(shortStart, end))
		require.NotEqual(t, http.StatusBadRequest, code)
		require.NotEmpty(t, cap.uris)

		reset()
		code = serve(t, server, "/api/v3/trace-summaries?"+traceQuery(longStart, end))
		requireClientError(t, code, spanRot.widest)
	})

	t.Run("http dependencies", func(t *testing.T) {
		reset()
		code := serve(t, server, "/api/dependencies?"+dependencyQuery(end, maxSpanAge))
		require.NotEqual(t, http.StatusBadRequest, code)
		require.NotEmpty(t, cap.uris)

		reset()
		code = serve(t, server, "/api/dependencies?"+dependencyQuery(end, 20*365*24*time.Hour))
		requireClientError(t, code, depRot.widest)
	})

	for _, path := range []string{"/api/metrics/calls", "/api/metrics/latencies", "/api/metrics/errors"} {
		t.Run("http "+path, func(t *testing.T) {
			reset()
			code := serve(t, server, path+"?"+metricsQuery(end, maxSpanAge))
			require.NotEqual(t, http.StatusBadRequest, code)
			require.NotEmpty(t, cap.uris)

			reset()
			code = serve(t, server, path+"?"+metricsQuery(end, 20*365*24*time.Hour))
			requireClientError(t, code, spanRot.widest)
		})
	}

	t.Run("grpc find traces", func(t *testing.T) {
		reset()
		err := grpcV2.FindTraces(&api_v2.FindTracesRequest{Query: &api_v2.TraceQueryParameters{
			ServiceName:  "frontend",
			StartTimeMin: longStart,
			StartTimeMax: end,
		}}, &v2TraceStream{})
		listed := 0
		for _, uri := range cap.uris {
			listed += indexCount(uri)
		}
		if status.Code(err) != codes.InvalidArgument || spanRot.widest > maxSpanAge+time.Hour || len(cap.uris) != 0 {
			t.Fatalf("code=%s widest=%s searches=%d names=%d err=%v; want InvalidArgument before any index name is built",
				status.Code(err), spanRot.widest, len(cap.uris), listed, err)
		}
	})

	t.Run("grpc dependencies", func(t *testing.T) {
		reset()
		_, err := grpcV2.GetDependencies(context.Background(), &api_v2.GetDependenciesRequest{
			StartTime: longStart,
			EndTime:   end,
		})
		listed := 0
		for _, uri := range cap.uris {
			listed += indexCount(uri)
		}
		if status.Code(err) != codes.InvalidArgument || depRot.widest > maxSpanAge+time.Hour || len(cap.uris) != 0 {
			t.Fatalf("code=%s widest=%s searches=%d names=%d err=%v; want InvalidArgument before any index name is built",
				status.Code(err), depRot.widest, len(cap.uris), listed, err)
		}
	})

	t.Run("grpc v3 find spans", func(t *testing.T) {
		reset()
		err := grpcV3.FindSpans(&api_v3.FindSpansRequest{Query: &api_v3.SpanQueryParameters{
			StartTimeMin: longStart,
			StartTimeMax: end,
		}}, &v3SpanStream{})
		listed := 0
		for _, uri := range cap.uris {
			listed += indexCount(uri)
		}
		if status.Code(err) != codes.InvalidArgument || spanRot.widest > maxSpanAge+time.Hour || len(cap.uris) != 0 {
			t.Fatalf("code=%s widest=%s searches=%d names=%d err=%v; want InvalidArgument before any index name is built",
				status.Code(err), spanRot.widest, len(cap.uris), listed, err)
		}
	})

	t.Run("error is a query refusal", func(t *testing.T) {
		reset()
		_, err := qs.GetDependencies(context.Background(), end, 20*365*24*time.Hour)
		listed := 0
		for _, uri := range cap.uris {
			listed += indexCount(uri)
		}
		if err == nil || !errors.Is(err, tracestore.ErrInvalidQuery) || depRot.widest > maxSpanAge+time.Hour || len(cap.uris) != 0 {
			t.Fatalf("err=%v widest=%s searches=%d names=%d; dependency lookback must be %v before any index name is built",
				err, depRot.widest, len(cap.uris), listed, tracestore.ErrInvalidQuery)
		}
	})
}

func serve(t *testing.T, server *httpServer, rawURL string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, rawURL, http.NoBody)
	rec := httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, req)
	return rec.Code
}

func traceQuery(start, end time.Time) string {
	q := url.Values{}
	q.Set("query.serviceName", "frontend")
	q.Set("query.startTimeMin", start.Format(time.RFC3339Nano))
	q.Set("query.startTimeMax", end.Format(time.RFC3339Nano))
	return q.Encode()
}

func dependencyQuery(end time.Time, lookback time.Duration) string {
	q := url.Values{}
	q.Set("endTs", strconv.FormatInt(end.UnixMilli(), 10))
	q.Set("lookback", strconv.FormatInt(lookback.Milliseconds(), 10))
	return q.Encode()
}

func metricsQuery(end time.Time, lookback time.Duration) string {
	q := url.Values{}
	q.Set("service", "frontend")
	q.Set("endTs", strconv.FormatInt(end.UnixMilli(), 10))
	q.Set("lookback", strconv.FormatInt(lookback.Milliseconds(), 10))
	q.Set("quantile", "0.95")
	return q.Encode()
}

func indexCount(uri string) int {
	path, _, _ := strings.Cut(uri, "?")
	path = strings.Trim(strings.TrimSuffix(path, "/_search"), "/")
	if path == "" {
		return 0
	}
	return strings.Count(path, ",") + 1
}

type recordingRotation struct {
	inner  indices.Rotation
	widest time.Duration
}

func (r *recordingRotation) ReadTargets(start, end time.Time) []string {
	if d := end.Sub(start); d > r.widest {
		r.widest = d
	}
	return r.inner.ReadTargets(start, end)
}

func (r *recordingRotation) WriteTarget(ts time.Time) string { return r.inner.WriteTarget(ts) }
func (r *recordingRotation) WriteOpType() es.WriteOpType     { return r.inner.WriteOpType() }
func (r *recordingRotation) RequiresDocumentTimestamp() bool {
	return r.inner.RequiresDocumentTimestamp()
}

type searchCapture struct {
	uris []string
}

func (c *searchCapture) searcher(t *testing.T) esclient.Searcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.uris = append(c.uris, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"hits":{"hits":[]}}`)
	}))
	t.Cleanup(srv.Close)
	client, err := esclient.NewClient(context.Background(), &config.Configuration{
		Servers: []string{srv.URL},
		Version: uint(es.ElasticV7),
	}, zap.NewNop(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return esclient.SearchClient{Client: client}
}

type v2TraceStream struct{ grpc.ServerStream }

func (*v2TraceStream) Context() context.Context              { return context.Background() }
func (*v2TraceStream) Send(*api_v2.SpansResponseChunk) error { return nil }

type v3SpanStream struct{ grpc.ServerStream }

func (*v3SpanStream) Context() context.Context             { return context.Background() }
func (*v3SpanStream) Send(*api_v3.FindSpansResponse) error { return nil }
