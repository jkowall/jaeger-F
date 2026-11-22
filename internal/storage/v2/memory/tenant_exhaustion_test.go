// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/jaegertracing/jaeger/internal/storage/v2/api/depstore"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
	"github.com/jaegertracing/jaeger/internal/tenancy"
)

// These tests fail while getTenant allocates one trace ring of length MaxTraces
// for every distinct tenant string and never releases it. A read must not
// retain a tenant record. A write of one trace must not reserve the whole ring.

func TestReadOnlyOperationsDoNotAllocateTenantRings(t *testing.T) {
	const maxTraces = 10000
	store := newExhaustionStore(t, maxTraces)
	// The default tenant is not the attack. Pay for it, if anything does,
	// outside the measurement of the attacker-controlled id.
	_, err := store.GetServices(context.Background())
	require.NoError(t, err)

	ringBytes := uint64(unsafe.Sizeof(traceAndId{})) * uint64(maxTraces)
	var delta uint64
	attacker := tenancy.WithTenant(context.Background(), "attacker")
	delta = allocatedBytes(func() {
		services, svcErr := store.GetServices(attacker)
		require.NoError(t, svcErr)
		require.Empty(t, services)
	})
	require.Less(t, delta, ringBytes/2, "GetServices allocated %d bytes; one ring of %d slots is %d bytes", delta, maxTraces, ringBytes)

	readWithNewTenant(t, store, "ops", func(ctx context.Context) {
		ops, opErr := store.GetOperations(ctx, tracestore.OperationQueryParams{ServiceName: "svc"})
		require.NoError(t, opErr)
		require.Empty(t, ops)
	})
	readWithNewTenant(t, store, "find-traces", func(ctx context.Context) {
		for _, iterErr := range store.FindTraces(ctx, traceQuery(0)) {
			require.NoError(t, iterErr)
		}
	})
	readWithNewTenant(t, store, "find-trace-ids", func(ctx context.Context) {
		for _, iterErr := range store.FindTraceIDs(ctx, traceQuery(0)) {
			require.NoError(t, iterErr)
		}
	})
	readWithNewTenant(t, store, "find-trace-ids-page", func(ctx context.Context) {
		query := traceQuery(0)
		query.SearchDepth = 0
		query.Pagination = &tracestore.Pagination{PageSize: 5}
		for _, iterErr := range store.FindTraceIDs(ctx, query) {
			require.NoError(t, iterErr)
		}
	})
	readWithNewTenant(t, store, "find-spans", func(ctx context.Context) {
		for _, iterErr := range store.FindSpans(ctx, tracestore.SpanQueryParams{}) {
			require.NoError(t, iterErr)
		}
	})
	readWithNewTenant(t, store, "get-traces", func(ctx context.Context) {
		var id pcommon.TraceID
		id[0] = 1
		for _, iterErr := range store.GetTraces(ctx, tracestore.GetTraceParams{TraceID: id}) {
			require.NoError(t, iterErr)
		}
	})
	readWithNewTenant(t, store, "dependencies", func(ctx context.Context) {
		deps, depErr := store.GetDependencies(ctx, depstore.QueryParameters{
			StartTime: time.Unix(1, 0).UTC(),
			EndTime:   time.Unix(2, 0).UTC(),
		})
		require.NoError(t, depErr)
		require.Empty(t, deps)
	})

	store.mu.RLock()
	defer store.mu.RUnlock()
	for id, tenant := range store.perTenant {
		require.Zero(t, cap(tenant.traces), "tenant %q holds a trace ring of cap %d after read-only calls", id, cap(tenant.traces))
		require.Falsef(t, strings.HasPrefix(id, "tenant-") || id == "attacker", "read retained tenant %q", id)
	}
}

func TestWriteTracesDoesNotPreallocateTenantRing(t *testing.T) {
	const maxTraces = 64
	store := newExhaustionStore(t, maxTraces)
	ctx := tenancy.WithTenant(context.Background(), "acme")
	first := singleSpanTrace(1)
	require.NoError(t, store.WriteTraces(ctx, first))

	store.mu.RLock()
	tenant := store.perTenant["acme"]
	store.mu.RUnlock()
	require.NotNil(t, tenant)
	require.Less(t, cap(tenant.traces), int(maxTraces), "the first trace reserved the whole ring of %d slots", maxTraces)

	var id pcommon.TraceID
	id[15] = 1
	found := false
	for batch, err := range store.GetTraces(ctx, tracestore.GetTraceParams{TraceID: id}) {
		require.NoError(t, err)
		found = found || len(batch) > 0
	}
	require.True(t, found, "the written trace was not readable")

	for n := 2; n <= int(maxTraces)+2; n++ {
		require.NoError(t, store.WriteTraces(ctx, singleSpanTrace(n)))
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	require.LessOrEqual(t, len(store.perTenant["acme"].ids), int(maxTraces), "MaxTraces no longer bounds retained traces")
}

func TestConcurrentTenantAccessDoesNotAllocateRings(t *testing.T) {
	const maxTraces = 128
	const tenants = 32
	store := newExhaustionStore(t, maxTraces)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range tenants {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			ctx := tenancy.WithTenant(context.Background(), tenantName("read", i))
			_, _ = store.GetServices(ctx)
		}()
		go func() {
			defer wg.Done()
			<-start
			ctx := tenancy.WithTenant(context.Background(), tenantName("write", i))
			_ = store.WriteTraces(ctx, singleSpanTrace(i+1))
		}()
	}
	close(start)
	wg.Wait()

	store.mu.RLock()
	defer store.mu.RUnlock()
	for id, tenant := range store.perTenant {
		require.Falsef(t, strings.HasPrefix(id, "read-"), "read created tenant %q", id)
		require.Less(t, cap(tenant.traces), int(maxTraces), "tenant %q reserved a ring of cap %d", id, cap(tenant.traces))
	}
}

func newExhaustionStore(t *testing.T, maxTraces uint32) *Store {
	t.Helper()
	store, err := NewStore(Configuration{MaxTraces: maxTraces})
	require.NoError(t, err)
	return store
}

func readWithNewTenant(t *testing.T, store *Store, name string, call func(context.Context)) {
	t.Helper()
	ctx := tenancy.WithTenant(context.Background(), "tenant-"+name)
	call(ctx)
	store.mu.RLock()
	defer store.mu.RUnlock()
	_, ok := store.perTenant["tenant-"+name]
	require.Falsef(t, ok, "%s retained a tenant store", name)
}

func traceQuery(depth uint32) tracestore.TraceQueryParams {
	if depth == 0 {
		depth = 5
	}
	return tracestore.TraceQueryParams{
		Attributes:  pcommon.NewMap(),
		SearchDepth: depth,
	}
}

func singleSpanTrace(n int) ptrace.Traces {
	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	var id pcommon.TraceID
	id[14] = byte(n >> 8)
	id[15] = byte(n)
	span.SetTraceID(id)
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(time.Unix(int64(n), 0).UTC()))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(time.Unix(int64(n)+1, 0).UTC()))
	return td
}

func tenantName(kind string, i int) string {
	return fmt.Sprintf("%s-%02d", kind, i)
}

func allocatedBytes(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}
