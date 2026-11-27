// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package spanstore

import (
	"context"
	"encoding/binary"
	"runtime"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/stretchr/testify/require"

	"github.com/jaegertracing/jaeger-idl/model/v1"
)

// TestScanTimeRangeDoesNotBufferEveryKey fails while a limited time scan
// still copies every span key before truncating to plan.limit. The
// dependency reader depends on this scan: it asks for math.MaxUint32 traces,
// so the truncation keeps every key and the following getTraces call loads
// every span body. A fix passes this test by stopping the iterator, or
// retaining only plan.limit keys, once the cap is reached.
func TestScanTimeRangeDoesNotBufferEveryKey(t *testing.T) {
	measure := func(n int) (uint64, int, error) {
		var delta uint64
		var got int
		var scanErr error
		runWithBadger(t, func(store *badger.DB, t *testing.T) {
			cache := NewCacheStore(store, time.Hour)
			writer := NewSpanWriter(store, cache, time.Hour)
			reader := NewTraceReader(store, cache, false)
			start := time.Now().Add(-time.Minute)
			for i := range n {
				err := writer.WriteSpan(context.Background(), &model.Span{
					TraceID:       model.TraceID{Low: uint64(i + 1), High: 4},
					SpanID:        1,
					OperationName: "op",
					Process:       &model.Process{ServiceName: "frontend"},
					StartTime:     start.Add(time.Duration(i) * time.Millisecond),
					Duration:      time.Millisecond,
				})
				require.NoError(t, err)
			}
			startBytes := make([]byte, 8)
			endBytes := make([]byte, 8)
			binary.BigEndian.PutUint64(startBytes, model.TimeAsEpochMicroseconds(start.Add(-time.Hour)))
			binary.BigEndian.PutUint64(endBytes, model.TimeAsEpochMicroseconds(time.Now().Add(time.Hour)))
			plan := &executionPlan{
				startTimeMin: startBytes,
				startTimeMax: endBytes,
				limit:        1,
			}
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			ids, err := reader.scanTimeRange(plan)
			runtime.ReadMemStats(&after)
			delta = after.TotalAlloc - before.TotalAlloc
			got = len(ids)
			scanErr = err
		})
		return delta, got, scanErr
	}

	smallAlloc, smallN, smallErr := measure(50)
	largeAlloc, largeN, largeErr := measure(400)
	if smallErr == nil {
		require.LessOrEqual(t, smallN, 1)
	}
	if largeErr == nil {
		require.LessOrEqual(t, largeN, 1)
	}
	require.NotZero(t, smallAlloc)

	// Vulnerable scan, limit 1: 50 traces allocate ~7KB and 400 traces
	// allocate ~52KB, about 7.6x, because every key is copied before the
	// limit is applied. Stopping at the cap keeps the two close.
	require.Lessf(t, float64(largeAlloc), float64(smallAlloc)*3,
		"a limit of 1 still buffered every key (%d bytes for 50 traces, %d bytes for 400 traces)",
		smallAlloc, largeAlloc)
}
