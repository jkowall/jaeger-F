// Copyright (c) 2019 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package spanstore

import (
	"context"
	"encoding/binary"
	"math/rand"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/spanstore"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
)

func TestEncodingTypes(t *testing.T) {
	// JSON encoding
	runWithBadger(t, func(store *badger.DB, t *testing.T) {
		testSpan := createDummySpan()

		cache := NewCacheStore(store, time.Duration(1*time.Hour))
		sw := NewSpanWriter(store, cache, time.Duration(1*time.Hour))
		rw := NewTraceReader(store, cache, true)

		sw.encodingType = jsonEncoding
		err := sw.WriteSpan(context.Background(), &testSpan)
		require.NoError(t, err)

		tr, err := rw.GetTrace(context.Background(), spanstore.GetTraceParameters{TraceID: model.TraceID{Low: 0, High: 1}})
		require.NoError(t, err)
		assert.Len(t, tr.Spans, 1)
	})

	// Unknown encoding write
	runWithBadger(t, func(store *badger.DB, t *testing.T) {
		testSpan := createDummySpan()

		cache := NewCacheStore(store, time.Duration(1*time.Hour))
		sw := NewSpanWriter(store, cache, time.Duration(1*time.Hour))
		// rw := NewTraceReader(store, cache)

		sw.encodingType = 0x04
		err := sw.WriteSpan(context.Background(), &testSpan)
		require.EqualError(t, err, "unknown encoding type: 0x04")
	})

	// Unknown encoding reader
	runWithBadger(t, func(store *badger.DB, t *testing.T) {
		testSpan := createDummySpan()

		cache := NewCacheStore(store, time.Duration(1*time.Hour))
		sw := NewSpanWriter(store, cache, time.Duration(1*time.Hour))
		rw := NewTraceReader(store, cache, true)

		err := sw.WriteSpan(context.Background(), &testSpan)
		require.NoError(t, err)

		startTime := model.TimeAsEpochMicroseconds(testSpan.StartTime)

		key, _, _ := createTraceKV(&testSpan, protoEncoding, startTime)
		e := &badger.Entry{
			Key:       key,
			ExpiresAt: uint64(time.Now().Add(1 * time.Hour).Unix()),
		}
		e.UserMeta = byte(0x04)

		store.Update(func(txn *badger.Txn) error {
			txn.SetEntry(e)
			return nil
		})

		_, err = rw.GetTrace(context.Background(), spanstore.GetTraceParameters{TraceID: model.TraceID{Low: 0, High: 1}})
		require.EqualError(t, err, "unknown encoding type: 0x04")
	})
}

func TestDecodeErrorReturns(t *testing.T) {
	garbage := []byte{0x08}

	_, err := decodeValue(garbage, protoEncoding)
	require.Error(t, err)

	_, err = decodeValue(garbage, jsonEncoding)
	require.Error(t, err)
}

func TestDuplicateTraceIDDetection(t *testing.T) {
	runWithBadger(t, func(store *badger.DB, t *testing.T) {
		testSpan := createDummySpan()
		cache := NewCacheStore(store, time.Duration(1*time.Hour))
		sw := NewSpanWriter(store, cache, time.Duration(1*time.Hour))
		rw := NewTraceReader(store, cache, true)
		origStartTime := testSpan.StartTime

		traceCount := 128
		for range traceCount {
			testSpan.TraceID.Low = rand.Uint64()
			for range 32 {
				testSpan.SpanID = model.SpanID(rand.Uint64())
				testSpan.StartTime = origStartTime.Add(time.Duration(rand.Int31n(8000)) * time.Millisecond)
				err := sw.WriteSpan(context.Background(), &testSpan)
				require.NoError(t, err)
			}
		}

		traces, err := rw.FindTraceIDs(context.Background(), &spanstore.TraceQueryParameters{
			ServiceName:  "service",
			NumTraces:    256, // Default is 100, we want to fetch more than there should be
			StartTimeMax: time.Now().Add(time.Hour),
			StartTimeMin: testSpan.StartTime.Add(-1 * time.Hour),
		})

		require.NoError(t, err)
		assert.Len(t, traces, 128)
	})
}

func createDummySpan() model.Span {
	tid := time.Now()

	dummyKv := []model.KeyValue{
		{
			Key:   "key",
			VType: model.StringType,
			VStr:  "value",
		},
	}

	testSpan := model.Span{
		TraceID: model.TraceID{
			Low:  uint64(0),
			High: 1,
		},
		SpanID:        model.SpanID(0),
		OperationName: "operation",
		Process: &model.Process{
			ServiceName: "service",
			Tags:        dummyKv,
		},
		StartTime: tid.Add(time.Duration(1 * time.Millisecond)),
		Duration:  time.Duration(1 * time.Millisecond),
		Tags:      dummyKv,
		Logs: []model.Log{
			{
				Timestamp: tid,
				Fields:    dummyKv,
			},
		},
	}

	return testSpan
}

func TestMergeJoin(t *testing.T) {
	chk := assert.New(t)

	// Test equals

	left := make([][]byte, 16)
	right := make([][]byte, 16)

	for i := range 16 {
		left[i] = make([]byte, 4)
		binary.BigEndian.PutUint32(left[i], uint32(i))

		right[i] = make([]byte, 4)
		binary.BigEndian.PutUint32(right[i], uint32(i))
	}

	merged := mergeJoinIds(left, right)
	chk.Len(merged, 16)

	// Check order
	chk.Equal(uint32(15), binary.BigEndian.Uint32(merged[15]))

	// Test simple non-equality different size

	merged = mergeJoinIds(left[1:2], right[13:])
	chk.Empty(merged)

	// Different size, some equalities

	merged = mergeJoinIds(left[0:3], right[1:7])
	chk.Len(merged, 2)
	chk.Equal(uint32(2), binary.BigEndian.Uint32(merged[1]))
}

func TestOldReads(t *testing.T) {
	runWithBadger(t, func(store *badger.DB, t *testing.T) {
		timeNow := model.TimeAsEpochMicroseconds(time.Now())
		s1Key := createIndexKey(serviceNameIndexKey, []byte("service1"), timeNow, model.TraceID{High: 0, Low: 0})
		s1o1Key := createIndexKey(operationNameIndexKey, operationIndexValue("service1", "operation1"), timeNow, model.TraceID{High: 0, Low: 0})

		tid := time.Now().Add(1 * time.Minute)

		writer := func() {
			store.Update(func(txn *badger.Txn) error {
				txn.SetEntry(&badger.Entry{
					Key:       s1Key,
					ExpiresAt: uint64(tid.Unix()),
				})
				txn.SetEntry(&badger.Entry{
					Key:       s1o1Key,
					ExpiresAt: uint64(tid.Unix()),
				})
				return nil
			})
		}

		cache := NewCacheStore(store, time.Duration(-1*time.Hour))
		writer()

		nuTid := tid.Add(1 * time.Hour)

		cache.Update("service1", "operation1", uint64(tid.Unix()))
		cache.services["service1"] = uint64(nuTid.Unix())
		cache.operations["service1"]["operation1"] = uint64(nuTid.Unix())

		// This is equivalent to populate caches of cache
		_ = NewTraceReader(store, cache, true)

		// Now make sure we didn't use the older timestamps from the DB
		assert.Equal(t, uint64(nuTid.Unix()), cache.services["service1"])
		assert.Equal(t, uint64(nuTid.Unix()), cache.operations["service1"]["operation1"])
	})
}

func TestCompositeIndexValuesDoNotCollide(t *testing.T) {
	opA := operationIndexValue("a", "bc")
	opAB := operationIndexValue("ab", "c")
	assert.NotEqual(t, opA, opAB)
	assert.Equal(t, uint32(1), binary.BigEndian.Uint32(opA[:4]))
	assert.Equal(t, "abc", string(opA[4:]))
	assert.Equal(t, uint32(2), binary.BigEndian.Uint32(opAB[:4]))
	assert.Equal(t, "abc", string(opAB[4:]))

	tagA := tagIndexValue("a", "bc", "v")
	tagAB := tagIndexValue("ab", "c", "v")
	assert.NotEqual(t, tagA, tagAB)

	// Same service, key "a"/value "bc" versus key "ab"/value "c".
	tagKeyA := tagIndexValue("svc", "a", "bc")
	tagKeyAB := tagIndexValue("svc", "ab", "c")
	assert.NotEqual(t, tagKeyA, tagKeyAB)
	keyLenAt := 4 + len("svc")
	assert.Equal(t, uint32(1), binary.BigEndian.Uint32(tagKeyA[keyLenAt:keyLenAt+4]))
	assert.Equal(t, uint32(2), binary.BigEndian.Uint32(tagKeyAB[keyLenAt:keyLenAt+4]))

	start := uint64(1)
	left := model.TraceID{High: 1, Low: 1}
	right := model.TraceID{High: 1, Low: 2}
	framedLeft := createIndexKey(operationNameIndexKey, opA, start, left)
	framedRight := createIndexKey(operationNameIndexKey, opAB, start, right)
	assert.Equal(t, operationNameIndexKey, framedLeft[0])
	assert.Equal(t, tagIndexKey, createIndexKey(tagIndexKey, tagA, start, left)[0])
	assert.NotEqual(t, framedLeft[:len(framedLeft)-sizeOfTraceID], framedRight[:len(framedRight)-sizeOfTraceID])

	// The retired layout stores the same bytes for both splits.
	legacyLeft := createIndexKey(legacyOperationNameIndexKey, []byte("abc"), start, left)
	legacyRight := createIndexKey(legacyOperationNameIndexKey, []byte("abc"), start, right)
	assert.Equal(t, legacyOperationNameIndexKey, legacyLeft[0])
	assert.Equal(t, legacyLeft[:len(legacyLeft)-sizeOfTraceID], legacyRight[:len(legacyRight)-sizeOfTraceID])
}

func TestOperationAndTagIndexCollisions(t *testing.T) {
	runWithBadger(t, func(store *badger.DB, t *testing.T) {
		cache := NewCacheStore(store, time.Hour)
		writer := NewSpanWriter(store, cache, time.Hour)
		reader := NewTraceReader(store, cache, false)
		start := time.Now()

		writeCollidingSpan(t, writer, "a", "bc", 1, start, model.String("bc", "v"))
		writeCollidingSpan(t, writer, "ab", "c", 2, start.Add(time.Millisecond), model.String("c", "v"))
		writeCollidingSpan(t, writer, "svc", "op", 3, start, model.String("a", "bc"))
		writeCollidingSpan(t, writer, "svc", "op", 4, start.Add(time.Millisecond), model.String("ab", "c"))

		assertOnlyTrace(t, reader, queryFor("a", "bc", nil, start), 1)
		assertOnlyTrace(t, reader, queryFor("ab", "c", nil, start), 2)
		assertOnlyTrace(t, reader, queryFor("a", "", map[string]string{"bc": "v"}, start), 1)
		assertOnlyTrace(t, reader, queryFor("ab", "", map[string]string{"c": "v"}, start), 2)
		assertOnlyTrace(t, reader, queryFor("svc", "", map[string]string{"a": "bc"}, start), 3)
		assertOnlyTrace(t, reader, queryFor("svc", "", map[string]string{"ab": "c"}, start), 4)
	})
}

func TestPreloadOperationsCollision(t *testing.T) {
	runWithBadger(t, func(store *badger.DB, t *testing.T) {
		writeCache := NewCacheStore(store, 3*time.Hour)
		shortWriter := NewSpanWriter(store, writeCache, time.Hour)
		longWriter := NewSpanWriter(store, writeCache, 3*time.Hour)
		start := time.Now()

		// Identical concatenated bytes ("abc") with different expiries. The
		// unframed index would keep the later expiry for both services.
		writeCollidingSpan(t, shortWriter, "a", "bc", 1, start, model.KeyValue{})
		writeCollidingSpan(t, longWriter, "ab", "c", 2, start.Add(time.Millisecond), model.KeyValue{})

		readCache := NewCacheStore(store, 3*time.Hour)
		reader := NewTraceReader(store, readCache, true)

		opsA, err := reader.GetOperations(context.Background(), tracestore.OperationQueryParams{ServiceName: "a"})
		require.NoError(t, err)
		assert.Equal(t, []tracestore.Operation{{Name: "bc"}}, opsA)

		opsAB, err := reader.GetOperations(context.Background(), tracestore.OperationQueryParams{ServiceName: "ab"})
		require.NoError(t, err)
		assert.Equal(t, []tracestore.Operation{{Name: "c"}}, opsAB)

		require.Contains(t, readCache.operations["a"], "bc")
		require.Contains(t, readCache.operations["ab"], "c")
		assert.Less(t, readCache.operations["a"]["bc"], readCache.operations["ab"]["c"])
	})
}

func TestLegacyCompositeIndexesAreNotRead(t *testing.T) {
	runWithBadger(t, func(store *badger.DB, t *testing.T) {
		span := createDummySpan()
		span.Process.ServiceName = "a"
		span.OperationName = "bc"
		span.Tags = []model.KeyValue{model.String("k", "v")}
		span.Process.Tags = nil
		span.Logs = nil
		startTime := model.TimeAsEpochMicroseconds(span.StartTime)
		key, val, err := createTraceKV(&span, protoEncoding, startTime)
		require.NoError(t, err)

		expire := uint64(time.Now().Add(time.Hour).Unix())
		legacyOp := createIndexKey(legacyOperationNameIndexKey, []byte("abc"), startTime, span.TraceID)
		legacyTag := createIndexKey(legacyTagIndexKey, []byte("akv"), startTime, span.TraceID)
		require.NoError(t, store.Update(func(txn *badger.Txn) error {
			entry := &badger.Entry{Key: key, Value: val, ExpiresAt: expire}
			entry.UserMeta = protoEncoding
			if err := txn.SetEntry(entry); err != nil {
				return err
			}
			if err := txn.SetEntry(&badger.Entry{Key: legacyOp, ExpiresAt: expire}); err != nil {
				return err
			}
			return txn.SetEntry(&badger.Entry{Key: legacyTag, ExpiresAt: expire})
		}))

		cache := NewCacheStore(store, time.Hour)
		reader := NewTraceReader(store, cache, false)
		got, err := reader.GetTrace(context.Background(), spanstore.GetTraceParameters{TraceID: span.TraceID})
		require.NoError(t, err)
		require.Len(t, got.Spans, 1)
		assert.Equal(t, "bc", got.Spans[0].OperationName)

		assertOnlyTrace(t, reader, queryFor("a", "bc", nil, span.StartTime))
		assertOnlyTrace(t, reader, queryFor("a", "", map[string]string{"k": "v"}, span.StartTime))

		reader.preloadOperations("a")
		assert.Empty(t, cache.operations["a"])
	})
}

func writeCollidingSpan(t *testing.T, w *SpanWriter, service, operation string, traceLow uint64, start time.Time, tag model.KeyValue) {
	t.Helper()

	span := model.Span{
		TraceID:       model.TraceID{High: 1, Low: traceLow},
		SpanID:        model.SpanID(1),
		OperationName: operation,
		Process:       &model.Process{ServiceName: service},
		StartTime:     start,
		Duration:      time.Millisecond,
	}
	if tag.Key != "" {
		span.Tags = []model.KeyValue{tag}
	}
	require.NoError(t, w.WriteSpan(context.Background(), &span))
}

func queryFor(service, operation string, tags map[string]string, start time.Time) *spanstore.TraceQueryParameters {
	return &spanstore.TraceQueryParameters{
		ServiceName:   service,
		OperationName: operation,
		Tags:          tags,
		StartTimeMin:  start.Add(-time.Minute),
		StartTimeMax:  start.Add(time.Minute),
		NumTraces:     10,
	}
}

func assertOnlyTrace(t *testing.T, reader *TraceReader, query *spanstore.TraceQueryParameters, traceLows ...uint64) {
	t.Helper()

	want := make([]model.TraceID, len(traceLows))
	for i, low := range traceLows {
		want[i] = model.TraceID{High: 1, Low: low}
	}

	ids, err := reader.FindTraceIDs(context.Background(), query)
	require.NoError(t, err)
	assert.ElementsMatch(t, want, ids)

	traces, err := reader.FindTraces(context.Background(), query)
	require.NoError(t, err)
	got := make([]model.TraceID, 0, len(traces))
	for _, tr := range traces {
		require.NotEmpty(t, tr.Spans)
		got = append(got, tr.Spans[0].TraceID)
		assert.Equal(t, query.ServiceName, tr.Spans[0].Process.ServiceName)
	}
	assert.ElementsMatch(t, want, got)
}
