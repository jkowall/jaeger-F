// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package spanstore_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger/internal/metrics"
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/spanstore"
	"github.com/jaegertracing/jaeger/internal/storage/v1/badger"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
)

// These tests pin the Badger index lookup to the service, operation, and tags
// that were stored on the span. They fail while index keys are the raw
// concatenation of those fields: a different split of the same bytes is
// returned as a hit, and a restarted reader reports the other split as an
// operation of this service.

func TestOperationIndexCollisionDoesNotMixTraces(t *testing.T) {
	t.Parallel()
	// "ab"+"c" and "a"+"bc" are the same byte string.
	runFactoryTest(t, func(tb testing.TB, sw spanstore.Writer, sr spanstore.Reader) {
		base := time.Now().Add(-time.Minute)
		victim := storedSpan(model.TraceID{High: 1, Low: 1}, 1, "a", "bc", base)
		planted := storedSpan(model.TraceID{High: 1, Low: 2}, 2, "ab", "c", base.Add(time.Second))
		require.NoError(tb, sw.WriteSpan(context.Background(), victim))
		require.NoError(tb, sw.WriteSpan(context.Background(), planted))

		// Both spans are stored under their own trace IDs.
		assertTraceIs(tb, sr, victim)
		assertTraceIs(tb, sr, planted)

		// A service-only lookup uses the service index, which is one field.
		assertOnlyTrace(tb, sr, query(base, "a", "", nil), victim.TraceID)
		assertOnlyTrace(tb, sr, query(base, "ab", "", nil), planted.TraceID)

		assertOnlyTrace(tb, sr, query(base, "a", "bc", nil), victim.TraceID)
		assertOnlyTrace(tb, sr, query(base, "ab", "c", nil), planted.TraceID)
	})
}

func TestTagIndexCollisionDoesNotMixTraces(t *testing.T) {
	t.Parallel()
	base := time.Now().Add(-time.Minute)

	t.Run("span tag service boundary", func(t *testing.T) {
		t.Parallel()
		// service "a" + key "bc" + value "d" == service "ab" + key "c" + value "d".
		runFactoryTest(t, func(tb testing.TB, sw spanstore.Writer, sr spanstore.Reader) {
			victim := storedSpan(model.TraceID{High: 2, Low: 1}, 1, "a", "op-a", base)
			victim.Tags = tags("bc", "d")
			planted := storedSpan(model.TraceID{High: 2, Low: 2}, 2, "ab", "op-ab", base.Add(time.Second))
			planted.Tags = tags("c", "d")
			require.NoError(tb, sw.WriteSpan(context.Background(), victim))
			require.NoError(tb, sw.WriteSpan(context.Background(), planted))

			assertOnlyTrace(tb, sr, query(base, "a", "", map[string]string{"bc": "d"}), victim.TraceID)
			assertOnlyTrace(tb, sr, query(base, "ab", "", map[string]string{"c": "d"}), planted.TraceID)
		})
	})

	t.Run("span tag key and value boundary", func(t *testing.T) {
		t.Parallel()
		// Same service: key "ab" + value "c" == key "a" + value "bc".
		runFactoryTest(t, func(tb testing.TB, sw spanstore.Writer, sr spanstore.Reader) {
			first := storedSpan(model.TraceID{High: 3, Low: 1}, 1, "svc", "op", base)
			first.Tags = tags("ab", "c")
			second := storedSpan(model.TraceID{High: 3, Low: 2}, 2, "svc", "op", base.Add(time.Second))
			second.Tags = tags("a", "bc")
			require.NoError(tb, sw.WriteSpan(context.Background(), first))
			require.NoError(tb, sw.WriteSpan(context.Background(), second))

			assertOnlyTrace(tb, sr, query(base, "svc", "", map[string]string{"ab": "c"}), first.TraceID)
			assertOnlyTrace(tb, sr, query(base, "svc", "", map[string]string{"a": "bc"}), second.TraceID)
		})
	})

	t.Run("process tag", func(t *testing.T) {
		t.Parallel()
		runFactoryTest(t, func(tb testing.TB, sw spanstore.Writer, sr spanstore.Reader) {
			victim := storedSpan(model.TraceID{High: 4, Low: 1}, 1, "a", "op-a", base)
			victim.Process.Tags = tags("bc", "d")
			planted := storedSpan(model.TraceID{High: 4, Low: 2}, 2, "ab", "op-ab", base.Add(time.Second))
			planted.Process.Tags = tags("c", "d")
			require.NoError(tb, sw.WriteSpan(context.Background(), victim))
			require.NoError(tb, sw.WriteSpan(context.Background(), planted))

			assertOnlyTrace(tb, sr, query(base, "a", "", map[string]string{"bc": "d"}), victim.TraceID)
			assertOnlyTrace(tb, sr, query(base, "ab", "", map[string]string{"c": "d"}), planted.TraceID)
		})
	})

	t.Run("log field", func(t *testing.T) {
		t.Parallel()
		runFactoryTest(t, func(tb testing.TB, sw spanstore.Writer, sr spanstore.Reader) {
			victim := storedSpan(model.TraceID{High: 5, Low: 1}, 1, "a", "op-a", base)
			victim.Logs = []model.Log{{Timestamp: base, Fields: tags("bc", "d")}}
			planted := storedSpan(model.TraceID{High: 5, Low: 2}, 2, "ab", "op-ab", base.Add(time.Second))
			planted.Logs = []model.Log{{Timestamp: base, Fields: tags("c", "d")}}
			require.NoError(tb, sw.WriteSpan(context.Background(), victim))
			require.NoError(tb, sw.WriteSpan(context.Background(), planted))

			assertOnlyTrace(tb, sr, query(base, "a", "", map[string]string{"bc": "d"}), victim.TraceID)
			assertOnlyTrace(tb, sr, query(base, "ab", "", map[string]string{"c": "d"}), planted.TraceID)
		})
	})
}

func TestOperationIndexCollisionDoesNotPolluteOperationsAfterRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := time.Now().Add(-time.Minute)

	writer, _, closeWriter := openBadger(t, dir)
	// "a"+"z" is a prefix of the other span's "ab"+"c" once the fields are joined.
	own := storedSpan(model.TraceID{High: 6, Low: 1}, 1, "a", "z", base)
	other := storedSpan(model.TraceID{High: 6, Low: 2}, 2, "ab", "c", base.Add(time.Second))
	require.NoError(t, writer.WriteSpan(context.Background(), own))
	require.NoError(t, writer.WriteSpan(context.Background(), other))
	closeWriter()

	_, reader, _ := openBadger(t, dir)
	assertOperationNames(t, reader, "a", "z")
	assertOperationNames(t, reader, "ab", "c")
}

func storedSpan(traceID model.TraceID, spanID model.SpanID, service, operation string, start time.Time) *model.Span {
	return &model.Span{
		TraceID:       traceID,
		SpanID:        spanID,
		OperationName: operation,
		Process: &model.Process{
			ServiceName: service,
		},
		StartTime: start,
		Duration:  time.Millisecond,
	}
}

func tags(key, value string) []model.KeyValue {
	return []model.KeyValue{{
		Key:   key,
		VStr:  value,
		VType: model.StringType,
	}}
}

func query(base time.Time, service, operation string, tagFilter map[string]string) *spanstore.TraceQueryParameters {
	return &spanstore.TraceQueryParameters{
		ServiceName:   service,
		OperationName: operation,
		Tags:          tagFilter,
		StartTimeMin:  base.Add(-time.Minute),
		StartTimeMax:  base.Add(time.Hour),
		NumTraces:     20,
	}
}

func assertTraceIs(tb testing.TB, sr spanstore.Reader, want *model.Span) {
	tb.Helper()
	got, err := sr.GetTrace(context.Background(), spanstore.GetTraceParameters{TraceID: want.TraceID})
	require.NoError(tb, err)
	require.Len(tb, got.Spans, 1)
	require.Equal(tb, want.Process.ServiceName, got.Spans[0].Process.ServiceName)
	require.Equal(tb, want.OperationName, got.Spans[0].OperationName)
}

func assertOnlyTrace(tb testing.TB, sr spanstore.Reader, q *spanstore.TraceQueryParameters, want model.TraceID) {
	tb.Helper()
	traces, err := sr.FindTraces(context.Background(), q)
	require.NoError(tb, err)
	require.Len(tb, traces, 1, "FindTraces service=%q operation=%q tags=%v", q.ServiceName, q.OperationName, q.Tags)
	require.NotEmpty(tb, traces[0].Spans)
	for _, span := range traces[0].Spans {
		require.Equal(tb, want, span.TraceID)
		require.NotNil(tb, span.Process)
		require.Equal(tb, q.ServiceName, span.Process.ServiceName)
		if q.OperationName != "" {
			require.Equal(tb, q.OperationName, span.OperationName)
		}
		for key, value := range q.Tags {
			require.Truef(tb, spanHasTag(span, key, value), "span service=%q operation=%q missing tag %s=%s", span.Process.ServiceName, span.OperationName, key, value)
		}
	}

	ids, err := sr.FindTraceIDs(context.Background(), q)
	require.NoError(tb, err)
	require.Equal(tb, []model.TraceID{want}, ids, "FindTraceIDs service=%q operation=%q tags=%v", q.ServiceName, q.OperationName, q.Tags)
}

func spanHasTag(span *model.Span, key, value string) bool {
	if kvHas(span.Tags, key, value) {
		return true
	}
	if span.Process != nil && kvHas(span.Process.Tags, key, value) {
		return true
	}
	for _, log := range span.Logs {
		if kvHas(log.Fields, key, value) {
			return true
		}
	}
	return false
}

func kvHas(kvs []model.KeyValue, key, value string) bool {
	for _, kv := range kvs {
		if kv.Key == key && kv.AsString() == value {
			return true
		}
	}
	return false
}

func assertOperationNames(t *testing.T, sr spanstore.Reader, service string, want ...string) {
	t.Helper()
	ops, err := sr.GetOperations(context.Background(), tracestore.OperationQueryParams{ServiceName: service})
	require.NoError(t, err)
	names := make([]string, 0, len(ops))
	for _, op := range ops {
		names = append(names, op.Name)
	}
	require.Equal(t, want, names, "operations for service %q", service)
}

func openBadger(t *testing.T, dir string) (spanstore.Writer, spanstore.Reader, func()) {
	t.Helper()
	f := badger.NewFactory()
	if dir == "" {
		f.Config.Ephemeral = true
	} else {
		f.Config.Ephemeral = false
		f.Config.SyncWrites = true
		f.Config.Directories.Keys = filepath.Join(dir, "keys")
		f.Config.Directories.Values = filepath.Join(dir, "values")
	}
	require.NoError(t, f.Initialize(metrics.NullFactory, zap.NewNop()))
	var closed bool
	closeFactory := func() {
		t.Helper()
		if closed {
			return
		}
		closed = true
		require.NoError(t, f.Close())
	}
	t.Cleanup(closeFactory)
	sw, err := f.CreateSpanWriter()
	require.NoError(t, err)
	sr, err := f.CreateSpanReader()
	require.NoError(t, err)
	return sw, sr, closeFactory
}
