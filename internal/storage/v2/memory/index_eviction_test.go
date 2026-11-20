// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/translator/zipkin/zipkinv2"
	zipkinmodel "github.com/openzipkin/zipkin-go/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/jaegertracing/jaeger/internal/jptrace/sanitizer"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
	"github.com/jaegertracing/jaeger/internal/telemetry/otelsemconv"
	"github.com/jaegertracing/jaeger/internal/tenancy"
)

// The in-memory store may keep a service name or operation only while some
// trace still in the ring carries it. After the ring overwrites a trace, names
// that belonged only to that trace must be gone from the maps GetServices and
// GetOperations read, not merely filtered at read time. These tests fail while
// those maps keep every name ever written.

func TestServiceAndOperationIndexesMatchRetainedTraces(t *testing.T) {
	const maxTraces = 3
	const written = 20
	store := newBoundedStore(t, maxTraces)
	ctx := context.Background()

	for n := 1; n <= written; n++ {
		require.NoError(t, store.WriteTraces(ctx, otlpProtoTrace(t, n, fmt.Sprintf("svc-%d", n), fmt.Sprintf("op-%d", n), ptrace.SpanKindServer)))
	}

	// The ring's three slots hold the newest traces. Earlier service and
	// operation names have no remaining trace and must not stay reachable.
	kept := []int{written - 2, written - 1, written}
	keptServices := make([]string, len(kept))
	for i, n := range kept {
		keptServices[i] = fmt.Sprintf("svc-%d", n)
		assertTraceStored(ctx, t, store, n, true)
		ops, err := store.GetOperations(ctx, tracestore.OperationQueryParams{ServiceName: keptServices[i]})
		require.NoError(t, err)
		assert.Equal(t, []tracestore.Operation{{
			Name:     fmt.Sprintf("op-%d", n),
			SpanKind: "server",
		}}, ops)
	}
	for n := 1; n <= written-maxTraces; n++ {
		assertTraceStored(ctx, t, store, n, false)
		ops, err := store.GetOperations(ctx, tracestore.OperationQueryParams{ServiceName: fmt.Sprintf("svc-%d", n)})
		require.NoError(t, err)
		assert.Empty(t, ops, "operations for evicted svc-%d are still returned", n)
	}

	services, err := store.GetServices(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, keptServices, services)

	assertIndexesMatchRing(t, store, "")
}

func TestAppendedAndSharedNamesAreDroppedWhenTheirTraceIsEvicted(t *testing.T) {
	store := newBoundedStore(t, 2)
	ctx := context.Background()

	// Two services and two operations arrive on trace 1, the second batch
	// appended to the same trace id. A later trace reuses the service with a
	// different operation and span kind.
	require.NoError(t, store.WriteTraces(ctx, otlpProtoTrace(t, 1, "shared", "op-old", ptrace.SpanKindServer)))
	require.NoError(t, store.WriteTraces(ctx, otlpProtoTrace(t, 1, "only-on-first", "op-extra", ptrace.SpanKindClient)))
	require.NoError(t, store.WriteTraces(ctx, otlpProtoTrace(t, 2, "other", "op-b", ptrace.SpanKindServer)))
	require.NoError(t, store.WriteTraces(ctx, otlpProtoTrace(t, 3, "shared", "op-new", ptrace.SpanKindClient)))

	assertTraceStored(ctx, t, store, 1, false)
	assertTraceStored(ctx, t, store, 2, true)
	assertTraceStored(ctx, t, store, 3, true)

	services, err := store.GetServices(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"other", "shared"}, services)

	shared, err := store.GetOperations(ctx, tracestore.OperationQueryParams{ServiceName: "shared"})
	require.NoError(t, err)
	assert.Equal(t, []tracestore.Operation{{Name: "op-new", SpanKind: "client"}}, shared)

	extra, err := store.GetOperations(ctx, tracestore.OperationQueryParams{ServiceName: "only-on-first"})
	require.NoError(t, err)
	assert.Empty(t, extra)

	assertIndexesMatchRing(t, store, "")
}

func TestIngestFormatsDoNotBypassIndexEviction(t *testing.T) {
	const maxTraces = 2
	const written = 5
	formats := []struct {
		name  string
		build func(t *testing.T, n int) ptrace.Traces
	}{
		{name: "otlp-json", build: otlpJSONTrace},
		{name: "zipkin-json", build: zipkinJSONTrace},
	}
	for _, format := range formats {
		t.Run(format.name, func(t *testing.T) {
			store := newBoundedStore(t, maxTraces)
			ctx := context.Background()
			for n := 1; n <= written; n++ {
				td := sanitizer.Sanitize(format.build(t, n))
				require.NoError(t, store.WriteTraces(ctx, td))
			}
			services, err := store.GetServices(ctx)
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"svc-4", "svc-5"}, services)
			for _, gone := range []string{"svc-1", "svc-2", "svc-3"} {
				ops, opErr := store.GetOperations(ctx, tracestore.OperationQueryParams{ServiceName: gone})
				require.NoError(t, opErr)
				assert.Empty(t, ops)
			}
			kept, err := store.GetOperations(ctx, tracestore.OperationQueryParams{ServiceName: "svc-5"})
			require.NoError(t, err)
			assert.Equal(t, []tracestore.Operation{{Name: "op-5", SpanKind: "server"}}, kept)
			assertIndexesMatchRing(t, store, "")
		})
	}
}

func TestTenantIndexesEvictIndependently(t *testing.T) {
	store := newBoundedStore(t, 2)
	ctxA := tenancy.WithTenant(context.Background(), "tenant-a")
	ctxB := tenancy.WithTenant(context.Background(), "tenant-b")

	for n := 1; n <= 4; n++ {
		require.NoError(t, store.WriteTraces(ctxA, otlpProtoTrace(t, n, fmt.Sprintf("a-%d", n), fmt.Sprintf("op-%d", n), ptrace.SpanKindServer)))
	}
	require.NoError(t, store.WriteTraces(ctxB, otlpProtoTrace(t, 1, "b-live", "b-op", ptrace.SpanKindServer)))

	servicesA, err := store.GetServices(ctxA)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a-3", "a-4"}, servicesA)
	servicesB, err := store.GetServices(ctxB)
	require.NoError(t, err)
	assert.Equal(t, []string{"b-live"}, servicesB)

	assertIndexesMatchRing(t, store, "tenant-a")
	assertIndexesMatchRing(t, store, "tenant-b")
}

func newBoundedStore(t *testing.T, maxTraces uint32) *Store {
	t.Helper()
	store, err := NewStore(Configuration{MaxTraces: maxTraces})
	require.NoError(t, err)
	return store
}

func assertTraceStored(ctx context.Context, t *testing.T, store *Store, n int, want bool) {
	t.Helper()
	var got int
	for traces, err := range store.GetTraces(ctx, tracestore.GetTraceParams{TraceID: numberedTraceID(t, n)}) {
		require.NoError(t, err)
		got += len(traces)
	}
	if want {
		assert.Equal(t, 1, got, "trace %d should still be stored", n)
	} else {
		assert.Zero(t, got, "trace %d should have been evicted", n)
	}
}

// assertIndexesMatchRing fails when the service or operation maps retain a
// name that no trace still in the ring carries, or drop a name that one does.
func assertIndexesMatchRing(t *testing.T, store *Store, tenantID string) {
	t.Helper()
	tenant := store.getTenant(tenantID)
	tenant.mu.RLock()
	defer tenant.mu.RUnlock()
	wantServices, wantOps := namesHeldByRing(tenant)
	assert.Equal(t, wantServices, tenant.services, "service index retains names the ring has already dropped")
	assert.Equal(t, wantOps, tenant.operations, "operation index retains names the ring has already dropped")
}

func namesHeldByRing(tenant *Tenant) (map[string]struct{}, map[string]map[tracestore.Operation]struct{}) {
	services := map[string]struct{}{}
	operations := map[string]map[tracestore.Operation]struct{}{}
	for _, entry := range tenant.traces {
		if entry.id.IsEmpty() {
			continue
		}
		for _, resourceSpan := range entry.trace.ResourceSpans().All() {
			serviceName := getServiceNameFromResource(resourceSpan.Resource())
			if serviceName == "" {
				continue
			}
			services[serviceName] = struct{}{}
			if _, ok := operations[serviceName]; !ok {
				operations[serviceName] = map[tracestore.Operation]struct{}{}
			}
			for _, scopeSpan := range resourceSpan.ScopeSpans().All() {
				for _, span := range scopeSpan.Spans().All() {
					operations[serviceName][tracestore.Operation{
						Name:     span.Name(),
						SpanKind: fromOTELSpanKind(span.Kind()),
					}] = struct{}{}
				}
			}
		}
	}
	return services, operations
}

func numberedTraceID(t *testing.T, n int) pcommon.TraceID {
	t.Helper()
	return fromString(t, fmt.Sprintf("%032x", n))
}

func numberedSpanID(t *testing.T, n int) pcommon.SpanID {
	t.Helper()
	return spanIdFromString(t, fmt.Sprintf("%016x", n))
}

// otlpProtoTrace builds one span and passes it through the OTLP protobuf
// unmarshaler the collector receiver uses, then the storage exporter's sanitizer.
func otlpProtoTrace(t *testing.T, n int, service, operation string, kind ptrace.SpanKind) ptrace.Traces {
	t.Helper()
	return ingestOTLP(t, ptraceotlp.ExportRequest.MarshalProto, ptraceotlp.ExportRequest.UnmarshalProto, n, service, operation, kind)
}

func otlpJSONTrace(t *testing.T, n int) ptrace.Traces {
	t.Helper()
	return ingestOTLP(t, ptraceotlp.ExportRequest.MarshalJSON, ptraceotlp.ExportRequest.UnmarshalJSON, n, fmt.Sprintf("svc-%d", n), fmt.Sprintf("op-%d", n), ptrace.SpanKindServer)
}

func ingestOTLP(
	t *testing.T,
	marshal func(ptraceotlp.ExportRequest) ([]byte, error),
	unmarshal func(ptraceotlp.ExportRequest, []byte) error,
	n int,
	service, operation string,
	kind ptrace.SpanKind,
) ptrace.Traces {
	t.Helper()
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr(otelsemconv.ServiceNameKey, service)
	span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.SetTraceID(numberedTraceID(t, n))
	span.SetSpanID(numberedSpanID(t, n))
	span.SetName(operation)
	span.SetKind(kind)
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(time.Unix(1_700_000_000, 0).UTC()))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(time.Unix(1_700_000_001, 0).UTC()))

	req := ptraceotlp.NewExportRequestFromTraces(td)
	body, err := marshal(req)
	require.NoError(t, err)
	decoded := ptraceotlp.NewExportRequest()
	require.NoError(t, unmarshal(decoded, body))
	return sanitizer.Sanitize(decoded.Traces())
}

func zipkinJSONTrace(t *testing.T, n int) ptrace.Traces {
	t.Helper()
	span := &zipkinmodel.SpanModel{
		TraceID:   zipkinmodel.TraceID{Low: uint64(n)},
		ID:        zipkinmodel.ID(n),
		Name:      fmt.Sprintf("op-%d", n),
		Kind:      zipkinmodel.Server,
		Timestamp: time.Unix(1_700_000_000, 0).UTC(),
		Duration:  time.Millisecond,
		LocalEndpoint: &zipkinmodel.Endpoint{
			ServiceName: fmt.Sprintf("svc-%d", n),
		},
	}
	body, err := json.Marshal([]*zipkinmodel.SpanModel{span})
	require.NoError(t, err)
	td, err := zipkinv2.NewJSONTracesUnmarshaler(false).UnmarshalTraces(body)
	require.NoError(t, err)
	return td
}
