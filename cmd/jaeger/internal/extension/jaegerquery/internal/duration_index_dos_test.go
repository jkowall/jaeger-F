// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gogo/protobuf/jsonpb"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/featuregate"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"

	"github.com/jaegertracing/jaeger-idl/proto-gen/api_v2"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/internal/apiv3"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/querysvc"
	"github.com/jaegertracing/jaeger/internal/expression"
	"github.com/jaegertracing/jaeger/internal/jptrace"
	"github.com/jaegertracing/jaeger/internal/metrics"
	"github.com/jaegertracing/jaeger/internal/proto/api_v3"
	expressionproto "github.com/jaegertracing/jaeger/internal/proto/expression/v1"
	"github.com/jaegertracing/jaeger/internal/storage/cassandra"
	"github.com/jaegertracing/jaeger/internal/storage/v1/cassandra/spanstore"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore/tracestoremetrics"
	cassstore "github.com/jaegertracing/jaeger/internal/storage/v2/cassandra/tracestore"
)

// maxDurationIndexStatementsPerSearch is the most duration_index statements one
// search may execute, however far apart the caller sets the two ends of the
// window. The query service forwards that window, and the Cassandra reader
// issues one statement per hourly bucket. An empty bucket does not end the
// walk. 48 is two days of buckets, counting both rounded ends.
const maxDurationIndexStatementsPerSearch = 48

func wideSearchWindow() (start, end time.Time) {
	end = time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	start = end.Add(-365 * 24 * time.Hour)
	return start, end
}

type recordingSession struct {
	mu    sync.Mutex
	stmts []string
}

func (s *recordingSession) Query(stmt string, _ ...any) cassandra.Query {
	return &recordingQuery{session: s, stmt: stmt}
}

func (*recordingSession) Close() {}

func (s *recordingSession) executed(stmt string) {
	s.mu.Lock()
	s.stmts = append(s.stmts, stmt)
	s.mu.Unlock()
}

func (s *recordingSession) durationIndexStatements() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, stmt := range s.stmts {
		if strings.Contains(stmt, "duration_index") {
			n++
		}
	}
	return n
}

type recordingQuery struct {
	session *recordingSession
	stmt    string
}

func (q *recordingQuery) Exec() error {
	q.session.executed(q.stmt)
	return nil
}

func (*recordingQuery) ScanCAS(...any) (bool, error) { return false, nil }

func (q *recordingQuery) Iter() cassandra.Iterator {
	q.session.executed(q.stmt)
	return emptyIterator{}
}

func (q *recordingQuery) Bind(...any) cassandra.Query                       { return q }
func (q *recordingQuery) Consistency(cassandra.Consistency) cassandra.Query { return q }
func (q *recordingQuery) PageSize(int) cassandra.Query                      { return q }
func (q *recordingQuery) String() string                                    { return q.stmt }

type emptyIterator struct{}

func (emptyIterator) Scan(...any) bool { return false }
func (emptyIterator) Close() error     { return nil }

func newDurationQueryService(t *testing.T) (*recordingSession, *querysvc.QueryService, http.Handler) {
	t.Helper()
	session := &recordingSession{}
	core, err := spanstore.NewSpanReader(
		session,
		metrics.NullFactory,
		zap.NewNop(),
		noop.NewTracerProvider().Tracer("duration-index-dos"),
	)
	require.NoError(t, err)
	// CreateTraceReader wraps the Cassandra reader with the metrics decorator
	// before the query service sees it. The decorator forwards the query.
	reader := tracestoremetrics.NewReaderDecorator(cassstore.NewTraceReader(core), metrics.NullFactory)
	qs := querysvc.NewQueryService(reader, nil, querysvc.QueryServiceOptions{})
	handler, _, err := initRouter(
		context.Background(),
		qs,
		nil,
		&QueryOptions{},
		noBackendCaps,
		noopTenancyMgr(),
		initTelSet(zaptest.NewLogger(t), noop.NewTracerProvider()),
	)
	require.NoError(t, err)
	return session, qs, handler
}

func assertDurationIndexBounded(t *testing.T, session *recordingSession) {
	t.Helper()
	start, end := wideSearchWindow()
	got := session.durationIndexStatements()
	require.LessOrEqualf(t, got, maxDurationIndexStatementsPerSearch,
		"duration search from %s to %s executed %d duration_index statements; an empty hour must not add another statement, and one search must stay within %d",
		start.Format(time.RFC3339), end.Format(time.RFC3339), got, maxDurationIndexStatementsPerSearch)
}

func durationSearchURL(path string) string {
	start, end := wideSearchWindow()
	q := url.Values{}
	q.Set("query.serviceName", "frontend")
	q.Set("query.startTimeMin", start.Format(time.RFC3339Nano))
	q.Set("query.startTimeMax", end.Format(time.RFC3339Nano))
	q.Set("query.durationMin", "1s")
	return path + "?" + q.Encode()
}

func TestDurationSearch_HTTPFindTracesDoesNotQueryEveryHour(t *testing.T) {
	session, _, handler := newDurationQueryService(t)
	req := httptest.NewRequest(http.MethodGet, durationSearchURL("/api/v3/traces"), http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	// An empty window is answered "No traces found" only after the reader has
	// already walked every hour. The status is not the bound.
	assertDurationIndexBounded(t, session)
}

func TestDurationSearch_HTTPFindTraceSummariesDoesNotQueryEveryHour(t *testing.T) {
	session, _, handler := newDurationQueryService(t)
	req := httptest.NewRequest(http.MethodGet, durationSearchURL("/api/v3/trace-summaries"), http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assertDurationIndexBounded(t, session)
}

func TestDurationSearch_HTTPFilterDoesNotQueryEveryHour(t *testing.T) {
	original := querysvc.StructuredFiltersGate.IsEnabled()
	require.NoError(t, featuregate.GlobalRegistry().Set(querysvc.StructuredFiltersGate.ID(), true))
	t.Cleanup(func() {
		require.NoError(t, featuregate.GlobalRegistry().Set(querysvc.StructuredFiltersGate.ID(), original))
	})

	var pred expression.Predicate
	filter := pred.And(
		pred.Resource().Service.Eq("frontend"),
		pred.Span().Duration.Gte("1s"),
	)
	encoded, err := expressionproto.CallToProto(filter)
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, new(jsonpb.Marshaler).Marshal(&buf, encoded))

	start, end := wideSearchWindow()
	q := url.Values{}
	q.Set("query.startTimeMin", start.Format(time.RFC3339Nano))
	q.Set("query.startTimeMax", end.Format(time.RFC3339Nano))
	q.Set("query.filter", buf.String())

	session, _, handler := newDurationQueryService(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v3/traces?"+q.Encode(), http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assertDurationIndexBounded(t, session)
}

func TestDurationSearch_GRPCV2FindTracesDoesNotQueryEveryHour(t *testing.T) {
	session, qs, _ := newDurationQueryService(t)
	start, end := wideSearchWindow()
	handler := NewGRPCHandler(qs, GRPCHandlerOptions{Logger: zap.NewNop()})
	err := handler.FindTraces(&api_v2.FindTracesRequest{
		Query: &api_v2.TraceQueryParameters{
			ServiceName:  "frontend",
			StartTimeMin: start,
			StartTimeMax: end,
			DurationMin:  time.Second,
		},
	}, &v2FindTracesStream{})
	_ = err
	assertDurationIndexBounded(t, session)
}

func TestDurationSearch_GRPCV3FindTracesDoesNotQueryEveryHour(t *testing.T) {
	session, qs, _ := newDurationQueryService(t)
	start, end := wideSearchWindow()
	handler := &apiv3.Handler{QueryService: qs}
	err := handler.FindTraces(&api_v3.FindTracesRequest{
		Query: &api_v3.TraceQueryParameters{
			ServiceName:  "frontend",
			StartTimeMin: start,
			StartTimeMax: end,
			DurationMin:  time.Second,
		},
	}, &v3FindTracesStream{})
	_ = err
	assertDurationIndexBounded(t, session)
}

// nopServerStream supplies the grpc.ServerStream methods a handler does not
// call. Context is what both FindTraces handlers read before searching.
type nopServerStream struct {
	grpc.ServerStream
}

func (*nopServerStream) Context() context.Context { return context.Background() }

// v2FindTracesStream is the stream the api_v2 handler writes to. An empty
// result never calls Send.
type v2FindTracesStream struct {
	nopServerStream
}

func (*v2FindTracesStream) Send(*api_v2.SpansResponseChunk) error { return nil }

// v3FindTracesStream is the stream the api_v3 handler writes to.
type v3FindTracesStream struct {
	nopServerStream
}

func (*v3FindTracesStream) Send(*jptrace.TracesData) error { return nil }
