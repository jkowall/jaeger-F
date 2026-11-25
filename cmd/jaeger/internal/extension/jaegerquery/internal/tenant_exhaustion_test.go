// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/confignet"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger-idl/proto-gen/api_v2"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/querysvc"
	"github.com/jaegertracing/jaeger/internal/proto/api_v3"
	"github.com/jaegertracing/jaeger/internal/storage/v2/memory"
	"github.com/jaegertracing/jaeger/internal/tenancy"
)

// allocationBudget is far below one trace ring at the all-in-one max_traces
// (100000) and far above a rejected services response.
const allocationBudget = 1 << 20

// TestEmptyAllowListQueryRequestsDoNotAllocateTenantStores drives the query
// server's HTTP and gRPC handlers. With multi-tenancy on and no allow-list,
// each new tenant header currently reaches the memory store and allocates a
// ring of length max_traces. The requests must be refused before that.
func TestEmptyAllowListQueryRequestsDoNotAllocateTenantStores(t *testing.T) {
	const maxTraces = 100000
	store, server := startTenantQueryServer(t, maxTraces, tenancy.Options{Enabled: true, Header: "x-tenant"})
	if server == nil {
		require.Zero(t, retainedTenants(store))
		return
	}

	windowStart := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(24 * time.Hour)
	traceQuery := "/api/v3/traces?query.serviceName=svc&query.startTimeMin=2020-01-01T00:00:00Z&query.startTimeMax=2020-01-02T00:00:00Z"

	// A missing header is already refused and does not allocate a ring. It pays
	// for one-time HTTP setup so the next measurement is the tenant ring.
	_, _ = measuredRequest(t, server, http.MethodGet, "/api/v3/services", "")

	// One accepted services lookup is enough to reserve the ring. Measure that
	// request, then require every other query operation to be refused as well.
	statusCode, delta := measuredRequest(t, server, http.MethodGet, "/api/v3/services", "attacker-services")
	require.Less(t, delta, uint64(allocationBudget), "GET /api/v3/services allocated %d bytes for one new tenant header", delta)
	require.Equal(t, http.StatusUnauthorized, statusCode)
	require.Zero(t, traceRingCap(store, "attacker-services"))

	for _, route := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "operations", method: http.MethodGet, path: "/api/v3/operations?service=svc"},
		{name: "get trace", method: http.MethodGet, path: "/api/v3/traces/00000000000000000000000000000001"},
		{name: "find traces", method: http.MethodGet, path: traceQuery},
		{name: "trace summaries", method: http.MethodGet, path: "/api/v3/trace-summaries?query.serviceName=svc&query.startTimeMin=2020-01-01T00:00:00Z&query.startTimeMax=2020-01-02T00:00:00Z"},
		{name: "find spans", method: http.MethodGet, path: "/api/v3/spans?query.startTimeMin=2020-01-01T00:00:00Z&query.startTimeMax=2020-01-02T00:00:00Z"},
		{name: "dependencies", method: http.MethodGet, path: "/api/dependencies"},
		{name: "archive", method: http.MethodPost, path: "/api/archive/00000000000000000000000000000001"},
		{name: "capabilities", method: http.MethodGet, path: "/api/v3/capabilities"},
	} {
		t.Run(route.name, func(t *testing.T) {
			code, spent := measuredRequest(t, server, route.method, route.path, "attacker-"+route.name)
			require.Less(t, spent, uint64(allocationBudget), "%s allocated %d bytes", route.path, spent)
			require.Equal(t, http.StatusUnauthorized, code)
		})
	}

	conn := dialQuery(t, server.GRPCAddr())
	v2Client := api_v2.NewQueryServiceClient(conn)
	v3Client := api_v3.NewQueryServiceClient(conn)
	_, _ = v3Client.GetServices(context.Background(), &api_v3.GetServicesRequest{})
	v3Query := &api_v3.TraceQueryParameters{
		ServiceName:  "svc",
		StartTimeMin: windowStart,
		StartTimeMax: windowEnd,
	}
	v2Query := &api_v2.TraceQueryParameters{
		ServiceName:  "svc",
		StartTimeMin: windowStart,
		StartTimeMax: windowEnd,
	}
	calls := []struct {
		name string
		call func(context.Context) error
	}{
		{name: "v3 GetServices", call: func(ctx context.Context) error {
			_, err := v3Client.GetServices(ctx, &api_v3.GetServicesRequest{})
			return err
		}},
		{name: "v3 GetOperations", call: func(ctx context.Context) error {
			_, err := v3Client.GetOperations(ctx, &api_v3.GetOperationsRequest{Service: "svc"})
			return err
		}},
		{name: "v3 GetDependencies", call: func(ctx context.Context) error {
			_, err := v3Client.GetDependencies(ctx, &api_v3.GetDependenciesRequest{StartTime: windowStart, EndTime: windowEnd})
			return err
		}},
		{name: "v3 FindTraces", call: func(ctx context.Context) error {
			stream, err := v3Client.FindTraces(ctx, &api_v3.FindTracesRequest{Query: v3Query})
			return streamResult(err, func() error { _, recvErr := stream.Recv(); return recvErr })
		}},
		{name: "v3 FindSpans", call: func(ctx context.Context) error {
			stream, err := v3Client.FindSpans(ctx, &api_v3.FindSpansRequest{Query: &api_v3.SpanQueryParameters{StartTimeMin: windowStart, StartTimeMax: windowEnd}})
			return streamResult(err, func() error { _, recvErr := stream.Recv(); return recvErr })
		}},
		{name: "v3 FindTraceSummaries", call: func(ctx context.Context) error {
			stream, err := v3Client.FindTraceSummaries(ctx, &api_v3.FindTraceSummariesRequest{Query: v3Query})
			return streamResult(err, func() error { _, recvErr := stream.Recv(); return recvErr })
		}},
		{name: "v3 GetTrace", call: func(ctx context.Context) error {
			stream, err := v3Client.GetTrace(ctx, &api_v3.GetTraceRequest{TraceId: "00000000000000000000000000000001"})
			return streamResult(err, func() error { _, recvErr := stream.Recv(); return recvErr })
		}},
		{name: "v2 GetServices", call: func(ctx context.Context) error {
			_, err := v2Client.GetServices(ctx, &api_v2.GetServicesRequest{})
			return err
		}},
		{name: "v2 GetOperations", call: func(ctx context.Context) error {
			_, err := v2Client.GetOperations(ctx, &api_v2.GetOperationsRequest{Service: "svc"})
			return err
		}},
		{name: "v2 GetDependencies", call: func(ctx context.Context) error {
			_, err := v2Client.GetDependencies(ctx, &api_v2.GetDependenciesRequest{StartTime: windowStart, EndTime: windowEnd})
			return err
		}},
		{name: "v2 FindTraces", call: func(ctx context.Context) error {
			stream, err := v2Client.FindTraces(ctx, &api_v2.FindTracesRequest{Query: v2Query})
			return streamResult(err, func() error { _, recvErr := stream.Recv(); return recvErr })
		}},
		{name: "v2 GetTrace", call: func(ctx context.Context) error {
			stream, err := v2Client.GetTrace(ctx, &api_v2.GetTraceRequest{TraceID: model.NewTraceID(0, 1)})
			return streamResult(err, func() error { _, recvErr := stream.Recv(); return recvErr })
		}},
	}
	for _, test := range calls {
		t.Run(test.name, func(t *testing.T) {
			ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-tenant", "attacker-"+test.name))
			var err error
			spent := allocatedBytes(func() { err = test.call(ctx) })
			require.Less(t, spent, uint64(allocationBudget), "%s allocated %d bytes", test.name, spent)
			require.Equal(t, codes.PermissionDenied, status.Code(err), "err=%v", err)
		})
	}
	require.Zero(t, retainedTenants(store), "query requests retained %d tenant stores", retainedTenants(store))
}

// TestAllowListedTenantReadDoesNotAllocateRing checks the same handlers when
// the header is on the allow-list. The lookup must succeed, and it must not
// reserve a trace ring before any trace is stored. A different header must
// not create a store for itself or for the listed tenant.
func TestAllowListedTenantReadDoesNotAllocateRing(t *testing.T) {
	const maxTraces = 100000
	store, server := startTenantQueryServer(t, maxTraces, tenancy.Options{
		Enabled: true,
		Header:  "x-tenant",
		Tenants: []string{"acme"},
	})
	require.NotNil(t, server)

	_, _ = measuredRequest(t, server, http.MethodGet, "/api/v3/services", "")
	statusCode, delta := measuredRequest(t, server, http.MethodGet, "/api/v3/services", "acme")
	require.Less(t, delta, uint64(allocationBudget), "allow-listed GetServices allocated %d bytes", delta)
	require.Equal(t, http.StatusOK, statusCode)
	require.Zero(t, traceRingCap(store, "acme"))

	otherCode, _ := measuredRequest(t, server, http.MethodGet, "/api/v3/services", "other")
	require.Equal(t, http.StatusUnauthorized, otherCode)
	require.Zero(t, traceRingCap(store, "other"))
	require.Zero(t, traceRingCap(store, "acme"))

	conn := dialQuery(t, server.GRPCAddr())
	client := api_v3.NewQueryServiceClient(conn)
	_, _ = client.GetServices(context.Background(), &api_v3.GetServicesRequest{})
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-tenant", "acme"))
	var err error
	spent := allocatedBytes(func() {
		_, err = client.GetServices(ctx, &api_v3.GetServicesRequest{})
	})
	require.NoError(t, err)
	require.Less(t, spent, uint64(allocationBudget), "allow-listed gRPC GetServices allocated %d bytes", spent)
	require.Zero(t, traceRingCap(store, "acme"))
}

func startTenantQueryServer(t *testing.T, maxTraces uint32, tenancyOptions tenancy.Options) (*memory.Store, *Server) {
	t.Helper()
	store, err := memory.NewStore(memory.Configuration{MaxTraces: maxTraces})
	require.NoError(t, err)
	qs := querysvc.NewQueryService(store, store, querysvc.QueryServiceOptions{})
	opts := &QueryOptions{
		HTTP: confighttp.ServerConfig{NetAddr: confignet.AddrConfig{Endpoint: "127.0.0.1:0", Transport: confignet.TransportTypeTCP}},
		GRPC: configgrpc.ServerConfig{NetAddr: confignet.AddrConfig{Endpoint: "127.0.0.1:0", Transport: confignet.TransportTypeTCP}},
	}
	telset := initTelSet(zaptest.NewLogger(t), nooptrace.NewTracerProvider())
	server, err := NewServer(context.Background(), qs, nil, opts, noBackendCaps, tenancy.NewManager(&tenancyOptions), telset)
	if err != nil {
		require.ErrorContains(t, err, "tenant")
		return store, nil
	}
	require.NoError(t, server.Start(context.Background()))
	t.Cleanup(func() {
		require.NoError(t, server.Close())
	})
	return store, server
}

func measuredRequest(t *testing.T, server *Server, method, path, tenant string) (int, uint64) {
	t.Helper()
	var code int
	spent := allocatedBytes(func() {
		req, err := http.NewRequest(method, "http://"+server.HTTPAddr()+path, http.NoBody)
		require.NoError(t, err)
		if tenant != "" {
			req.Header.Set("x-tenant", tenant)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		require.NoError(t, resp.Body.Close())
		code = resp.StatusCode
	})
	return code, spent
}

func dialQuery(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
	})
	return conn
}

func streamResult(err error, recv func() error) error {
	if err != nil {
		return err
	}
	err = recv()
	if errors.Is(err, io.EOF) {
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
