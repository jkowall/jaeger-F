// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package spanstore

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/internal/metrics"
	"github.com/jaegertracing/jaeger/internal/storage/cassandra"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
)

// maxDurationIndexStatementsPerSearch is the most duration_index statements one
// search may execute, however far apart the caller sets the two ends of the
// window. queryByDuration issues one statement per hourly bucket from the end
// back to the start, and an empty bucket does not end the walk. 48 is two days
// of buckets, counting both rounded ends: a short window still fits, and a
// window of years does not get a statement per hour.
const maxDurationIndexStatementsPerSearch = 48

// wideSearchWindow is long enough that one statement per hour is far past the
// cap above. The same loop runs for any longer window, including one measured
// in years: nothing between the two timestamps stops it.
func wideSearchWindow() (start, end time.Time) {
	end = time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	start = end.Add(-365 * 24 * time.Hour)
	return start, end
}

// recordingSession is the Cassandra session the reader is given. It records a
// statement when the reader executes it (Iter or Exec), which is the point at
// which gocql sends the statement on a pooled connection.
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

func newRecordingReader(t *testing.T) (*recordingSession, *SpanReader) {
	t.Helper()
	session := &recordingSession{}
	reader, err := NewSpanReader(
		session,
		metrics.NullFactory,
		zap.NewNop(),
		noop.NewTracerProvider().Tracer("duration-index-dos"),
	)
	require.NoError(t, err)
	return session, reader
}

func durationQuery(minDuration, maxDuration time.Duration) *tracestore.TraceQueryParams {
	start, end := wideSearchWindow()
	return &tracestore.TraceQueryParams{
		ServiceName:  "frontend",
		StartTimeMin: start,
		StartTimeMax: end,
		DurationMin:  minDuration,
		DurationMax:  maxDuration,
		SearchDepth:  20,
		Attributes:   pcommon.NewMap(),
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

func TestDurationSearch_FindTraceIDsDoesNotQueryEveryHour(t *testing.T) {
	session, reader := newRecordingReader(t)
	_, err := reader.FindTraceIDs(context.Background(), durationQuery(time.Second, 0))
	// A refusal is a fix. Statements already sent are not.
	_ = err
	assertDurationIndexBounded(t, session)
}

func TestDurationSearch_FindTracesDoesNotQueryEveryHour(t *testing.T) {
	session, reader := newRecordingReader(t)
	for _, err := range reader.FindTraces(context.Background(), durationQuery(time.Second, 0)) {
		if err != nil {
			break
		}
	}
	assertDurationIndexBounded(t, session)
}

func TestDurationSearch_DurationMaxOnlyDoesNotQueryEveryHour(t *testing.T) {
	session, reader := newRecordingReader(t)
	_, err := reader.FindTraceIDs(context.Background(), durationQuery(0, 2*time.Second))
	_ = err
	assertDurationIndexBounded(t, session)
}
