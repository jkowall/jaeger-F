// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package spanstore

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/stretchr/testify/require"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/spanstore"
)

// outsideSpans is large enough that buffering one duration-index key per stored
// span is distinguishable from buffering one key per span inside the window.
const outsideSpans = 400

// TestDurationIndexScanDropsSpansBeforeWindow pins the duration-index walk to
// the requested time window.
//
// Duration keys are ordered by duration, then start time, so a seek to
// (minDuration, windowStart) only skips earlier start times at that first
// duration. Spans with a longer duration that started before the window still
// sort after that seek point. The scan must not copy those keys, and a search
// must still return the span that actually started inside the window.
func TestDurationIndexScanDropsSpansBeforeWindow(t *testing.T) {
	// Not parallel: AllocsPerRun refuses to run while a test is parallel, and the
	// allocation bound is what rejects a scan that copies every key and then drops it.
	for _, tc := range []struct {
		name         string
		durationMin  time.Duration
		durationMax  time.Duration
		operation    string
		serviceQuery bool
	}{
		{name: "min only", durationMin: time.Microsecond, serviceQuery: true},
		{name: "max only", durationMax: 10 * time.Millisecond, serviceQuery: true},
		{name: "min and max", durationMin: time.Microsecond, durationMax: 10 * time.Millisecond, serviceQuery: true},
		{name: "min only by operation", durationMin: time.Microsecond, operation: "op", serviceQuery: true},
		{name: "min only no service", durationMin: time.Microsecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runWithBadger(t, func(store *badger.DB, t *testing.T) {
				windowEnd := time.Now().UTC().Truncate(time.Microsecond)
				windowStart := windowEnd.Add(-time.Minute)
				insideAt := windowStart.Add(10 * time.Second)
				beforeAt := windowStart.Add(-2 * time.Hour)
				afterAt := windowEnd.Add(time.Hour)

				cache := NewCacheStore(store, 72*time.Hour)
				writer := NewSpanWriter(store, cache, 72*time.Hour)
				reader := NewTraceReader(store, cache, false)

				// Longer than the minimum, so these keys sort after the seek point
				// even when their start time is before the window.
				const matchDuration = 5 * time.Millisecond
				insideID := model.TraceID{High: 1, Low: 1}
				writeSpan(t, writer, spanSpec{
					id: insideID, service: "svc", operation: "op",
					start: insideAt, duration: matchDuration,
				})
				// Same duration as the lower bound, inside the window: the seek
				// must still find it.
				minDurationID := model.TraceID{High: 1, Low: 2}
				writeSpan(t, writer, spanSpec{
					id: minDurationID, service: "svc", operation: "op",
					start: insideAt.Add(time.Millisecond), duration: time.Microsecond,
				})
				// Same duration as the lower bound but before the window. A seek
				// to (minDuration, windowStart) already steps over this key; it
				// must stay excluded.
				writeSpan(t, writer, spanSpec{
					id: model.TraceID{High: 1, Low: 3}, service: "svc", operation: "op",
					start: beforeAt, duration: time.Microsecond,
				})
				// After the window: already dropped by the upper-bound check.
				writeSpan(t, writer, spanSpec{
					id: model.TraceID{High: 1, Low: 4}, service: "svc", operation: "op",
					start: afterAt, duration: matchDuration,
				})
				// Below every minimum this test uses, inside the window.
				writeSpan(t, writer, spanSpec{
					id: model.TraceID{High: 1, Low: 5}, service: "svc", operation: "op",
					start: insideAt, duration: 100 * time.Nanosecond,
				})
				// Above the maximum used by the bounded cases, before the window.
				writeSpan(t, writer, spanSpec{
					id: model.TraceID{High: 1, Low: 6}, service: "other", operation: "op",
					start: beforeAt, duration: 50 * time.Millisecond,
				})

				beforeIDs := make(map[model.TraceID]struct{}, outsideSpans)
				for i := range outsideSpans {
					id := model.TraceID{High: 2, Low: uint64(i + 1)}
					beforeIDs[id] = struct{}{}
					writeSpan(t, writer, spanSpec{
						id: id, service: "other", operation: "other-op",
						start:    beforeAt.Add(time.Duration(i) * time.Microsecond),
						duration: matchDuration,
					})
				}

				query := &spanstore.TraceQueryParameters{
					StartTimeMin: windowStart,
					StartTimeMax: windowEnd,
					DurationMin:  tc.durationMin,
					DurationMax:  tc.durationMax,
					NumTraces:    20,
				}
				if tc.serviceQuery {
					query.ServiceName = "svc"
					query.OperationName = tc.operation
				}

				require.NoError(t, validateQuery(query))
				setQueryDefaults(query)
				plan := planFor(query)

				keys, err := reader.scanRangeIndex(plan, durationStartKey(query), durationEndKey(query))
				require.NoError(t, err)
				scanAllocs := testing.AllocsPerRun(3, func() {
					_, scanErr := reader.scanRangeIndex(plan, durationStartKey(query), durationEndKey(query))
					if scanErr != nil {
						panic(scanErr)
					}
				})
				findAllocs := testing.AllocsPerRun(3, func() {
					_, findErr := reader.FindTraceIDs(context.Background(), query)
					if findErr != nil {
						panic(findErr)
					}
				})
				for _, key := range keys {
					start := indexKeyStartTime(t, key)
					require.Falsef(t, start.Before(windowStart),
						"duration scan copied a key starting at %s, before the window %s", start, windowStart)
					require.Falsef(t, start.After(windowEnd),
						"duration scan copied a key starting at %s, after the window %s", start, windowEnd)
				}
				// In-window spans whose stored duration (microseconds) falls in range:
				//   5ms  — every case
				//   1us  — minimum is 0 or 1us, and maximum is unset or at least 1us
				//   100ns is stored as 0us, so only a minimum of 0 includes it
				wantKeys := 1
				if tc.durationMin <= time.Microsecond && (tc.durationMax == 0 || tc.durationMax >= time.Microsecond) {
					wantKeys++
				}
				if tc.durationMin == 0 {
					wantKeys++
				}
				require.Len(t, keys, wantKeys)

				matched := reader.durationQueries(plan, query)
				for id := range beforeIDs {
					_, ok := matched[id]
					require.Falsef(t, ok, "durationQueries retained trace %v from before the window", id)
				}
				require.LessOrEqual(t, len(matched), wantKeys)

				// A walk that copies one key per stored span allocates hundreds of
				// slices. Skipping those keys, or seeking to the next duration,
				// stays near the number of in-window matches. The bound is on the
				// allocations made while scanning, so filtering the slice after
				// copying every key still fails.
				require.LessOrEqualf(t, scanAllocs, float64(wantKeys*8+40),
					"scanRangeIndex allocated %.0f times; outside spans=%d in-window keys=%d",
					scanAllocs, outsideSpans, wantKeys)
				// A service (or operation) query is the path the query API uses. Without a
				// service name the reader also walks the primary keys, which is a separate
				// scan; the duration-index budget above still applies to that case.
				if tc.serviceQuery {
					require.LessOrEqualf(t, findAllocs, float64(wantKeys*8+120),
						"FindTraceIDs allocated %.0f times for %d outside spans and %d in-window keys",
						findAllocs, outsideSpans, wantKeys)
				}

				ids, err := reader.FindTraceIDs(context.Background(), query)
				require.NoError(t, err)
				got := map[model.TraceID]struct{}{}
				for _, id := range ids {
					got[id] = struct{}{}
					_, outside := beforeIDs[id]
					require.False(t, outside, "FindTraceIDs returned a trace that started before the window")
				}
				require.Contains(t, got, insideID)
				if tc.durationMin <= time.Microsecond && (tc.durationMax == 0 || tc.durationMax >= time.Microsecond) {
					require.Contains(t, got, minDurationID)
				}

				traces, err := reader.FindTraces(context.Background(), query)
				require.NoError(t, err)
				require.NotEmpty(t, traces)
				for _, tr := range traces {
					for _, sp := range tr.Spans {
						require.Falsef(t, sp.StartTime.Before(windowStart),
							"FindTraces returned span %v starting before the window", sp.SpanID)
					}
				}
			})
		})
	}
}

type spanSpec struct {
	id        model.TraceID
	service   string
	operation string
	start     time.Time
	duration  time.Duration
}

func writeSpan(t *testing.T, writer *SpanWriter, spec spanSpec) {
	t.Helper()
	err := writer.WriteSpan(context.Background(), &model.Span{
		TraceID:       spec.id,
		SpanID:        model.SpanID(spec.id.Low),
		OperationName: spec.operation,
		Process:       &model.Process{ServiceName: spec.service},
		StartTime:     spec.start,
		Duration:      spec.duration,
	})
	require.NoError(t, err)
}

func planFor(query *spanstore.TraceQueryParameters) *executionPlan {
	start := make([]byte, 8)
	end := make([]byte, 8)
	binary.BigEndian.PutUint64(start, model.TimeAsEpochMicroseconds(query.StartTimeMin))
	binary.BigEndian.PutUint64(end, model.TimeAsEpochMicroseconds(query.StartTimeMax))
	return &executionPlan{
		startTimeMin: start,
		startTimeMax: end,
		limit:        query.NumTraces,
	}
}

// durationStartKey and durationEndKey mirror the key bounds durationQueries builds,
// so the test scans the same range the search does.
func durationStartKey(query *spanstore.TraceQueryParameters) []byte {
	key := make([]byte, 1+8)
	key[0] = durationIndexKey
	binary.BigEndian.PutUint64(key[1:], uint64(model.DurationAsMicroseconds(query.DurationMin)))
	return key
}

func durationEndKey(query *spanstore.TraceQueryParameters) []byte {
	durMax := uint64(model.DurationAsMicroseconds(query.DurationMax))
	if query.DurationMax == 0 {
		durMax = ^uint64(0)
	}
	key := make([]byte, 1+8)
	key[0] = durationIndexKey
	binary.BigEndian.PutUint64(key[1:], durMax)
	return key
}

func indexKeyStartTime(t *testing.T, key []byte) time.Time {
	t.Helper()
	require.GreaterOrEqual(t, len(key), sizeOfTraceID+8)
	ts := key[len(key)-(sizeOfTraceID+8) : len(key)-sizeOfTraceID]
	return model.EpochMicrosecondsAsTime(binary.BigEndian.Uint64(ts))
}
