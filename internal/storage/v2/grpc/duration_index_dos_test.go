// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package grpc

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/internal/metrics"
	"github.com/jaegertracing/jaeger/internal/proto-gen/storage/v2"
	"github.com/jaegertracing/jaeger/internal/storage/cassandra"
	"github.com/jaegertracing/jaeger/internal/storage/v1/cassandra/spanstore"
	cassstore "github.com/jaegertracing/jaeger/internal/storage/v2/cassandra/tracestore"
)

// maxDurationIndexStatementsPerSearch is the most duration_index statements one
// search may execute, however far apart the caller sets the two ends of the
// window. This server copies that window onto the Cassandra reader, which
// issues one statement per hourly bucket. 48 is two days of buckets.
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

func newRecordingHandler(t *testing.T) (*recordingSession, *Handler) {
	t.Helper()
	session := &recordingSession{}
	core, err := spanstore.NewSpanReader(
		session,
		metrics.NullFactory,
		zap.NewNop(),
		noop.NewTracerProvider().Tracer("duration-index-dos"),
	)
	require.NoError(t, err)
	reader := cassstore.NewTraceReader(core)
	return session, NewHandler(reader, nil, nil)
}

func wireQuery() *storage.TraceQueryParameters {
	start, end := wideSearchWindow()
	return &storage.TraceQueryParameters{
		ServiceName:  "frontend",
		StartTimeMin: start,
		StartTimeMax: end,
		DurationMin:  time.Second,
		SearchDepth:  20,
	}
}

func assertDurationIndexBounded(t *testing.T, session *recordingSession) {
	t.Helper()
	start, end := wideSearchWindow()
	got := session.durationIndexStatements()
	require.LessOrEqualf(t, got, maxDurationIndexStatementsPerSearch,
		"duration search from %s to %s executed %d duration_index statements; an empty hour must not add another statement, and one search must stay within %d",
		start.Format(time.RFC3339), end.Format(time.RFC3339), got, maxDurationIndexStatementsPerSearch)
}

func TestDurationSearch_FindTracesDoesNotQueryEveryHour(t *testing.T) {
	session, handler := newRecordingHandler(t)
	err := handler.FindTraces(&storage.FindTracesRequest{Query: wireQuery()}, &testStream{})
	_ = err
	assertDurationIndexBounded(t, session)
}

func TestDurationSearch_FindTraceIDsDoesNotQueryEveryHour(t *testing.T) {
	session, handler := newRecordingHandler(t)
	_, err := handler.FindTraceIDs(context.Background(), &storage.FindTraceIDsRequest{Query: wireQuery()})
	_ = err
	assertDurationIndexBounded(t, session)
}
