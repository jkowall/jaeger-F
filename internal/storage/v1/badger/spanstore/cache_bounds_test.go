// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package spanstore

import (
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger/internal/storage/v2/v1adapter"
)

// Limits the Badger service and operation cache must enforce. A client who can
// submit spans chooses service.name and the span name. Those strings are kept
// in process memory for the span TTL (72h by default) and, after that, until
// some later list call. The cache has to stay within these limits on every
// write path, and it has to drop entries whose stored expiry is at or before
// the current second without waiting for GetServices or GetOperations.
const (
	maxServiceCacheEntries             = 1024
	maxOperationCacheEntriesPerService = 1024
)

func TestOTLPWriteTracesBoundsServiceAndOperationCache(t *testing.T) {
	t.Parallel()
	runWithBadger(t, func(store *badger.DB, t *testing.T) {
		cache := NewCacheStore(store, 72*time.Hour)
		writer := NewSpanWriter(store, cache, 72*time.Hour)
		traces := v1adapter.NewTraceWriter(writer)
		ctx := t.Context()

		// Names below the cap are retained, which is what ties the assertion to
		// the OTLP service.name attribute and the span name rather than to an
		// empty cache.
		for i := 0; i < 3; i++ {
			service := fmt.Sprintf("kept-svc-%d", i)
			operation := fmt.Sprintf("kept-op-%d", i)
			require.NoError(t, traces.WriteTraces(ctx, otlpTraces(service, operation, i)))
			assert.Contains(t, cache.services, service)
			require.Contains(t, cache.operations, service)
			assert.Contains(t, cache.operations[service], operation)
		}

		for i := 0; i < maxServiceCacheEntries+32; i++ {
			require.NoError(t, traces.WriteTraces(ctx, otlpTraces(
				fmt.Sprintf("svc-%d", i),
				fmt.Sprintf("op-%d", i),
				1000+i,
			)))
		}
		assertCacheWithinBounds(t, cache)
		assert.NotEmpty(t, cache.services)
		assert.NotEmpty(t, cache.operations)

		for i := 0; i < maxOperationCacheEntriesPerService+32; i++ {
			require.NoError(t, traces.WriteTraces(ctx, otlpTraces(
				"one-service",
				fmt.Sprintf("only-op-%d", i),
				100000+i,
			)))
		}
		assertCacheWithinBounds(t, cache)
		ops := cache.operations["one-service"]
		assert.LessOrEqual(t, len(ops), maxOperationCacheEntriesPerService)
		assert.NotEmpty(t, ops)
	})
}

func TestWriteSpanDropsExpiredNamesWithoutListing(t *testing.T) {
	t.Parallel()
	runWithBadger(t, func(store *badger.DB, t *testing.T) {
		cache := NewCacheStore(store, 72*time.Hour)
		writer := NewSpanWriter(store, cache, 72*time.Hour)
		past := unixSeconds(time.Now().Add(-time.Hour))

		// Names that aged out while sitting in the map. Nothing lists services
		// or operations; the next accepted span has to sweep them.
		cache.services["stale-svc"] = past
		cache.operations["stale-svc"] = map[string]uint64{"stale-op": past}
		cache.services["stale-svc-2"] = past
		cache.operations["stale-svc-2"] = map[string]uint64{"stale-op-2": past}

		require.NoError(t, writer.WriteSpan(t.Context(), namedSpan("fresh-svc", "fresh-op", 1)))

		assert.NotContains(t, cache.services, "stale-svc")
		assert.NotContains(t, cache.operations, "stale-svc")
		assert.NotContains(t, cache.services, "stale-svc-2")
		assert.NotContains(t, cache.operations, "stale-svc-2")
		assert.Contains(t, cache.services, "fresh-svc")
		require.Contains(t, cache.operations, "fresh-svc")
		assert.Contains(t, cache.operations["fresh-svc"], "fresh-op")
	})
}

func TestWriteSpanDoesNotRetainAlreadyExpiredNames(t *testing.T) {
	t.Parallel()
	runWithBadger(t, func(store *badger.DB, t *testing.T) {
		cache := NewCacheStore(store, -time.Minute)
		// A non-positive TTL makes WriteSpan store an expiry at or before now.
		writer := NewSpanWriter(store, cache, -time.Minute)
		// The storage write may reject an already-expired key. The cache update
		// still runs, and it must not keep the name.
		_ = writer.WriteSpan(t.Context(), namedSpan("expired-svc", "expired-op", 1))
		assert.NotContains(t, cache.services, "expired-svc")
		assert.NotContains(t, cache.operations, "expired-svc")
		assert.Empty(t, cache.services)
		assert.Empty(t, cache.operations)
	})
}

func TestWriteSpanCacheStaysBoundedWhenStorageRejectsTheWrite(t *testing.T) {
	t.Parallel()
	runWithBadger(t, func(store *badger.DB, t *testing.T) {
		cache := NewCacheStore(store, 72*time.Hour)
		writer := NewSpanWriter(store, cache, 72*time.Hour)
		require.NoError(t, store.Close())

		for i := 0; i < maxServiceCacheEntries+32; i++ {
			err := writer.WriteSpan(t.Context(), namedSpan(
				fmt.Sprintf("rejected-svc-%d", i),
				fmt.Sprintf("rejected-op-%d", i),
				i,
			))
			require.Error(t, err)
		}
		assertCacheWithinBounds(t, cache)
	})
}

func TestAddServiceBoundsNamesAndDropsExpired(t *testing.T) {
	t.Parallel()
	cache := NewCacheStore(nil, 72*time.Hour)
	past := unixSeconds(time.Now().Add(-time.Hour))
	future := unixSeconds(time.Now().Add(time.Hour))

	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("stale-%d", i)
		cache.services[name] = past
		cache.operations[name] = map[string]uint64{"op": past}
	}
	cache.AddService("live", future)
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("stale-%d", i)
		assert.NotContains(t, cache.services, name)
		assert.NotContains(t, cache.operations, name)
	}
	assert.Equal(t, future, cache.services["live"])

	// An already-expired add must not leave the name behind.
	cache.AddService("already-expired", past)
	assert.NotContains(t, cache.services, "already-expired")

	for i := 0; i < maxServiceCacheEntries+32; i++ {
		cache.AddService(fmt.Sprintf("svc-%d", i), future+uint64(i)+1)
	}
	assert.LessOrEqual(t, len(cache.services), maxServiceCacheEntries)
	assert.NotEmpty(t, cache.services)
	assert.LessOrEqual(t, len(cache.operations), maxServiceCacheEntries)
}

func TestAddOperationBoundsNamesAndDropsExpired(t *testing.T) {
	t.Parallel()
	cache := NewCacheStore(nil, 72*time.Hour)
	past := unixSeconds(time.Now().Add(-time.Hour))
	future := unixSeconds(time.Now().Add(time.Hour))

	cache.operations["svc"] = map[string]uint64{}
	for i := 0; i < 8; i++ {
		cache.operations["svc"][fmt.Sprintf("stale-op-%d", i)] = past
	}
	cache.AddOperation("svc", "live-op", future)
	for i := 0; i < 8; i++ {
		assert.NotContains(t, cache.operations["svc"], fmt.Sprintf("stale-op-%d", i))
	}
	assert.Equal(t, future, cache.operations["svc"]["live-op"])

	cache.AddOperation("svc", "already-expired", past)
	assert.NotContains(t, cache.operations["svc"], "already-expired")

	for i := 0; i < maxOperationCacheEntriesPerService+32; i++ {
		cache.AddOperation("svc", fmt.Sprintf("op-%d", i), future+uint64(i)+1)
	}
	require.Contains(t, cache.operations, "svc")
	assert.LessOrEqual(t, len(cache.operations["svc"]), maxOperationCacheEntriesPerService)
	assert.NotEmpty(t, cache.operations["svc"])

	for i := 0; i < maxServiceCacheEntries+32; i++ {
		cache.AddOperation(fmt.Sprintf("svc-%d", i), "op", future+uint64(i)+1)
	}
	assert.LessOrEqual(t, len(cache.operations), maxServiceCacheEntries)
	for svc, ops := range cache.operations {
		assert.LessOrEqual(t, len(ops), maxOperationCacheEntriesPerService, svc)
	}
}

func TestReaderPrefillBoundsNamesLoadedFromDisk(t *testing.T) {
	t.Parallel()
	runWithBadger(t, func(store *badger.DB, t *testing.T) {
		cache := NewCacheStore(store, 72*time.Hour)
		writer := NewSpanWriter(store, cache, 72*time.Hour)
		ctx := t.Context()

		for i := 0; i < maxServiceCacheEntries+32; i++ {
			require.NoError(t, writer.WriteSpan(ctx, namedSpan(
				fmt.Sprintf("disk-svc-%d", i),
				"op",
				i,
			)))
		}
		for i := 0; i < maxOperationCacheEntriesPerService+32; i++ {
			require.NoError(t, writer.WriteSpan(ctx, namedSpan(
				"disk-one-service",
				fmt.Sprintf("disk-op-%d", i),
				100000+i,
			)))
		}

		// Same prefill CreateSpanReader performs after a restart: every index
		// key still inside Badger's TTL is offered to AddService / AddOperation.
		reloaded := NewCacheStore(store, 72*time.Hour)
		_ = NewTraceReader(store, reloaded, true)
		assertCacheWithinBounds(t, reloaded)
		assert.NotEmpty(t, reloaded.services)
		if ops, ok := reloaded.operations["disk-one-service"]; ok {
			assert.LessOrEqual(t, len(ops), maxOperationCacheEntriesPerService)
		}
	})
}

func TestUpdateDropsExpiredNamesWithoutListing(t *testing.T) {
	t.Parallel()
	cache := NewCacheStore(nil, 72*time.Hour)
	past := unixSeconds(time.Now().Add(-time.Hour))
	future := unixSeconds(time.Now().Add(time.Hour))

	cache.Update("stale-svc", "stale-op", past)
	assert.NotContains(t, cache.services, "stale-svc")
	assert.NotContains(t, cache.operations, "stale-svc")

	cache.services["stale-svc"] = past
	cache.operations["stale-svc"] = map[string]uint64{"stale-op": past}
	cache.Update("fresh-svc", "fresh-op", future)
	assert.NotContains(t, cache.services, "stale-svc")
	assert.NotContains(t, cache.operations, "stale-svc")
	assert.Equal(t, future, cache.services["fresh-svc"])
	assert.Equal(t, future, cache.operations["fresh-svc"]["fresh-op"])

	for i := 0; i < maxServiceCacheEntries+32; i++ {
		cache.Update(fmt.Sprintf("svc-%d", i), fmt.Sprintf("op-%d", i), future+uint64(i)+1)
	}
	assertCacheWithinBounds(t, cache)
	assert.NotEmpty(t, cache.services)
}

func assertCacheWithinBounds(t *testing.T, cache *CacheStore) {
	t.Helper()
	assert.LessOrEqual(t, len(cache.services), maxServiceCacheEntries)
	assert.LessOrEqual(t, len(cache.operations), maxServiceCacheEntries)
	for svc, ops := range cache.operations {
		assert.LessOrEqual(t, len(ops), maxOperationCacheEntriesPerService, svc)
	}
}

func namedSpan(service, operation string, n int) *model.Span {
	return &model.Span{
		TraceID:       model.TraceID{High: uint64(n + 1), Low: 1},
		SpanID:        model.SpanID(n + 1),
		OperationName: operation,
		Process:       &model.Process{ServiceName: service},
		StartTime:     time.Unix(1_700_000_000, 0).UTC(),
		Duration:      time.Millisecond,
	}
}

func otlpTraces(service, operation string, n int) ptrace.Traces {
	traces := ptrace.NewTraces()
	rs := traces.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", service)
	span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.SetName(operation)

	var traceID pcommon.TraceID
	var spanID pcommon.SpanID
	binary.BigEndian.PutUint64(traceID[:8], uint64(n+1))
	binary.BigEndian.PutUint64(traceID[8:], uint64(n+1))
	binary.BigEndian.PutUint64(spanID[:], uint64(n+1))
	span.SetTraceID(traceID)
	span.SetSpanID(spanID)

	start := time.Unix(1_700_000_000, 0).UTC()
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(start))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(start.Add(time.Millisecond)))
	return traces
}

func unixSeconds(ts time.Time) uint64 {
	return uint64(ts.Unix())
}
