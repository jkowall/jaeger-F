// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"io"
	"reflect"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/confignet"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/jaegertracing/jaeger/internal/proto-gen/storage/v2"
	"github.com/jaegertracing/jaeger/internal/storage/v2/memory"
	"github.com/jaegertracing/jaeger/internal/telemetry"
	"github.com/jaegertracing/jaeger/internal/tenancy"
)

// allocationBudget is far below one trace ring at max_traces 100000 and far
// above a refused storage RPC.
const allocationBudget = 1 << 20

// TestEmptyAllowListRemoteStorageDoesNotAllocateTenantStores sends tenant
// metadata to the remote-storage gRPC server. The server uses the same
// tenancy interceptor and memory store as a memory-backed deployment. An
// empty allow-list must refuse the call before a tenant ring is allocated.
func TestEmptyAllowListRemoteStorageDoesNotAllocateTenantStores(t *testing.T) {
	const maxTraces = 100000
	store, server := startRemoteMemoryServer(t, maxTraces, tenancy.Options{Enabled: true, Header: "x-tenant"})
	if server == nil {
		require.Zero(t, retainedTenants(store))
		return
	}
	conn := dialRemote(t, server.GRPCAddr(), tenancy.NewManager(&tenancy.Options{Enabled: true, Header: "x-tenant"}))
	reader := storage.NewTraceReaderClient(conn)
	deps := storage.NewDependencyReaderClient(conn)
	writer := ptraceotlp.NewGRPCClient(conn)
	windowStart := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(24 * time.Hour)
	query := &storage.TraceQueryParameters{
		ServiceName:  "svc",
		StartTimeMin: windowStart,
		StartTimeMax: windowEnd,
		SearchDepth:  5,
	}

	// A missing tenant header is already refused and does not allocate a ring.
	// It pays for the gRPC connection so the next measurement is the tenant ring.
	_, _ = reader.GetServices(context.Background(), &storage.GetServicesRequest{})
	_, _ = writer.Export(context.Background(), ptraceotlp.NewExportRequestFromTraces(ptrace.NewTraces()))

	var err error
	spent := allocatedBytes(func() {
		ctx := tenancy.WithTenant(context.Background(), "attacker-services")
		_, err = reader.GetServices(ctx, &storage.GetServicesRequest{})
	})
	require.Less(t, spent, uint64(allocationBudget), "GetServices allocated %d bytes for one new tenant", spent)
	require.Equal(t, codes.PermissionDenied, status.Code(err), "err=%v", err)
	require.Zero(t, traceRingCap(store, "attacker-services"))

	calls := []struct {
		name string
		call func(context.Context) error
	}{
		{name: "GetOperations", call: func(ctx context.Context) error {
			_, callErr := reader.GetOperations(ctx, &storage.GetOperationsRequest{Service: "svc"})
			return callErr
		}},
		{name: "GetTraces", call: func(ctx context.Context) error {
			stream, callErr := reader.GetTraces(ctx, &storage.GetTracesRequest{Query: []*storage.GetTraceParams{{
				TraceId: []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
			}}})
			return streamResult(callErr, func() error { _, recvErr := stream.Recv(); return recvErr })
		}},
		{name: "GetDependencies", call: func(ctx context.Context) error {
			_, callErr := deps.GetDependencies(ctx, &storage.GetDependenciesRequest{StartTime: windowStart, EndTime: windowEnd})
			return callErr
		}},
		{name: "FindTraces", call: func(ctx context.Context) error {
			stream, callErr := reader.FindTraces(ctx, &storage.FindTracesRequest{Query: query})
			return streamResult(callErr, func() error { _, recvErr := stream.Recv(); return recvErr })
		}},
		{name: "FindTraceIDs", call: func(ctx context.Context) error {
			_, callErr := reader.FindTraceIDs(ctx, &storage.FindTraceIDsRequest{Query: query})
			return callErr
		}},
		{name: "FindSpans", call: func(ctx context.Context) error {
			stream, callErr := reader.FindSpans(ctx, &storage.FindSpansRequest{Query: &storage.SpanQueryParameters{
				StartTimeMin: windowStart,
				StartTimeMax: windowEnd,
			}})
			return streamResult(callErr, func() error { _, recvErr := stream.Recv(); return recvErr })
		}},
		{name: "FindTraceSummaries", call: func(ctx context.Context) error {
			stream, callErr := reader.FindTraceSummaries(ctx, &storage.FindTraceSummariesRequest{Query: query})
			return streamResult(callErr, func() error { _, recvErr := stream.Recv(); return recvErr })
		}},
		{name: "Export", call: func(ctx context.Context) error {
			_, callErr := writer.Export(ctx, ptraceotlp.NewExportRequestFromTraces(oneRemoteTrace()))
			return callErr
		}},
	}
	for _, test := range calls {
		t.Run(test.name, func(t *testing.T) {
			ctx := tenancy.WithTenant(context.Background(), "attacker-"+test.name)
			var callErr error
			callSpent := allocatedBytes(func() { callErr = test.call(ctx) })
			require.Less(t, callSpent, uint64(allocationBudget), "%s allocated %d bytes", test.name, callSpent)
			require.Equal(t, codes.PermissionDenied, status.Code(callErr), "err=%v", callErr)
		})
	}
	require.Zero(t, retainedTenants(store))
}

// TestAllowListedRemoteWriteDoesNotPreallocateRing writes one trace for a
// listed tenant through Export. The store may keep that trace, and it must
// not reserve max_traces slots to do it. An unlisted tenant must not create
// a store.
func TestAllowListedRemoteWriteDoesNotPreallocateRing(t *testing.T) {
	const maxTraces = 100000
	store, server := startRemoteMemoryServer(t, maxTraces, tenancy.Options{
		Enabled: true,
		Header:  "x-tenant",
		Tenants: []string{"acme"},
	})
	require.NotNil(t, server)
	conn := dialRemote(t, server.GRPCAddr(), tenancy.NewManager(&tenancy.Options{
		Enabled: true,
		Header:  "x-tenant",
		Tenants: []string{"acme"},
	}))
	writer := ptraceotlp.NewGRPCClient(conn)
	reader := storage.NewTraceReaderClient(conn)

	_, _ = reader.GetServices(context.Background(), &storage.GetServicesRequest{})
	var err error
	spent := allocatedBytes(func() {
		_, err = writer.Export(tenancy.WithTenant(context.Background(), "acme"), ptraceotlp.NewExportRequestFromTraces(oneRemoteTrace()))
	})
	require.NoError(t, err)
	require.Less(t, spent, uint64(allocationBudget), "Export allocated %d bytes for one trace", spent)
	require.Less(t, traceRingCap(store, "acme"), int(maxTraces))

	_, err = reader.GetServices(tenancy.WithTenant(context.Background(), "other"), &storage.GetServicesRequest{})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Zero(t, traceRingCap(store, "other"))
}

func startRemoteMemoryServer(t *testing.T, maxTraces uint32, options tenancy.Options) (*memory.Store, *Server) {
	t.Helper()
	telset := telemetry.NoopSettings()
	telset.Logger = zap.NewNop()
	factory, err := memory.NewFactory(memory.Configuration{MaxTraces: maxTraces}, telset)
	require.NoError(t, err)
	server, err := NewServer(
		context.Background(),
		configgrpc.ServerConfig{NetAddr: confignet.AddrConfig{Endpoint: "127.0.0.1:0", Transport: confignet.TransportTypeTCP}},
		factory,
		factory,
		tenancy.NewManager(&options),
		telset,
	)
	if err != nil {
		require.ErrorContains(t, err, "tenant")
		return factoryStore(factory), nil
	}
	require.NoError(t, server.Start(context.Background()))
	t.Cleanup(func() {
		require.NoError(t, server.Close())
	})
	return factoryStore(factory), server
}

func dialRemote(t *testing.T, addr string, tm *tenancy.Manager) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(tenancy.NewClientUnaryInterceptor(tm)),
		grpc.WithStreamInterceptor(tenancy.NewClientStreamInterceptor(tm)),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
	})
	return conn
}

func oneRemoteTrace() ptrace.Traces {
	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	var id pcommon.TraceID
	id[15] = 1
	span.SetTraceID(id)
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(time.Unix(1, 0).UTC()))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(time.Unix(2, 0).UTC()))
	return td
}

func streamResult(err error, recv func() error) error {
	if err != nil {
		return err
	}
	err = recv()
	if err == io.EOF {
		return nil
	}
	return err
}

func allocatedBytes(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func factoryStore(factory *memory.Factory) *memory.Store {
	field := reflect.ValueOf(factory).Elem().FieldByName("store")
	return reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface().(*memory.Store)
}

func retainedTenants(store *memory.Store) int {
	return tenantMap(store).Len()
}

func traceRingCap(store *memory.Store, tenant string) int {
	entry := tenantMap(store).MapIndex(reflect.ValueOf(tenant))
	if !entry.IsValid() {
		return 0
	}
	tracesField := entry.Elem().FieldByName("traces")
	traces := reflect.NewAt(tracesField.Type(), unsafe.Pointer(tracesField.UnsafeAddr())).Elem()
	return traces.Cap()
}

func tenantMap(store *memory.Store) reflect.Value {
	field := reflect.ValueOf(store).Elem().FieldByName("perTenant")
	return reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
}
