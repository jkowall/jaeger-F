// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"

	"github.com/jaegertracing/jaeger/cmd/internal/storageconfig"
	"github.com/jaegertracing/jaeger/cmd/remote-storage/app"
	"github.com/jaegertracing/jaeger/internal/auth/bearertoken"
	"github.com/jaegertracing/jaeger/internal/proto-gen/storage/v2"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/depstore"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
	"github.com/jaegertracing/jaeger/internal/telemetry"
	"github.com/jaegertracing/jaeger/internal/tenancy"
	"github.com/jaegertracing/jaeger/ports"
)

// placeholderToken is not a credential the server is configured with.
// Sending it must not authorize a call.
const placeholderToken = "placeholder-token"

const (
	parentService = "parent-svc"
	childService  = "child-svc"
	parentOp      = "parent-op"
	childOp       = "child-op"
	injectedSvc   = "injected-service"
)

var (
	storedTraceID = pcommon.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	parentSpanID  = pcommon.SpanID{1}
	childSpanID   = pcommon.SpanID{2}
	injectedID    = pcommon.TraceID{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9}
	spanStart     = time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
)

// recordingWriter counts writes that reach the real memory store. A refusal
// that persists the batch first still increments calls.
type recordingWriter struct {
	inner tracestore.Writer
	calls int
}

func (w *recordingWriter) WriteTraces(ctx context.Context, td ptrace.Traces) error {
	w.calls++
	return w.inner.WriteTraces(ctx, td)
}

// observingFactory is the memory factory main builds for the default config.
// CreateTraceWriter wraps that factory's writer so a test can see a write.
type observingFactory struct {
	traces tracestore.Factory
	deps   depstore.Factory
	writer *recordingWriter
}

func (f *observingFactory) CreateTraceReader() (tracestore.Reader, error) {
	return f.traces.CreateTraceReader()
}

func (f *observingFactory) CreateTraceWriter() (tracestore.Writer, error) {
	writer, err := f.traces.CreateTraceWriter()
	if err != nil {
		return nil, err
	}
	f.writer = &recordingWriter{inner: writer}
	return f.writer, nil
}

func (f *observingFactory) CreateDependencyReader() (depstore.Reader, error) {
	return f.deps.CreateDependencyReader()
}

type startedServer struct {
	server *app.Server
	addr   string
	tls    bool
	reader tracestore.Reader
	deps   depstore.Reader
	writer *recordingWriter
}

// startNoConfigServer follows the process main runs when --config-file is
// omitted: loadConfig returns the default, the memory backend is created with
// no auth resolver, and the gRPC server is started from that config. The port
// is 0 so the test can bind, and the host is the host from the default
// endpoint, which is empty.
func startNoConfigServer(t *testing.T) *startedServer {
	t.Helper()
	cfg, err := loadConfig(context.Background(), viper.New())
	require.NoError(t, err)

	_, port, err := net.SplitHostPort(cfg.GRPC.NetAddr.Endpoint)
	require.NoError(t, err)
	require.Equal(t, strconv.Itoa(ports.RemoteStorageGRPC), port)
	require.Equal(t, "memory", cfg.GetStorageName())

	host, _, err := net.SplitHostPort(cfg.GRPC.NetAddr.Endpoint)
	require.NoError(t, err)
	grpcCfg := cfg.GRPC
	grpcCfg.NetAddr.Endpoint = net.JoinHostPort(host, "0")

	telset := telemetry.NoopSettings()
	backend := cfg.Storage.TraceBackends[cfg.GetStorageName()]
	traceFactory, err := storageconfig.CreateTraceStorageFactory(
		context.Background(),
		cfg.GetStorageName(),
		backend,
		telset,
		nil,
	)
	require.NoError(t, err)
	depFactory, ok := traceFactory.(depstore.Factory)
	require.True(t, ok)

	seed, err := traceFactory.CreateTraceWriter()
	require.NoError(t, err)
	require.NoError(t, seed.WriteTraces(context.Background(), storedTrace()))

	obs := &observingFactory{traces: traceFactory, deps: depFactory}
	tm := tenancy.NewManager(&cfg.Tenancy)
	server, err := app.NewServer(context.Background(), grpcCfg, obs, obs, tm, telset)
	require.NoError(t, err)
	require.NoError(t, server.Start(context.Background()))
	t.Cleanup(func() {
		require.NoError(t, server.Close())
	})
	require.NotNil(t, obs.writer)

	reader, err := traceFactory.CreateTraceReader()
	require.NoError(t, err)
	deps, err := depFactory.CreateDependencyReader()
	require.NoError(t, err)

	return &startedServer{
		server: server,
		addr:   server.GRPCAddr(),
		tls:    grpcCfg.TLS.HasValue(),
		reader: reader,
		deps:   deps,
		writer: obs.writer,
	}
}

func (s *startedServer) dial(t *testing.T) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(s.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
	})
	return conn
}

func storedTrace() ptrace.Traces {
	td := ptrace.NewTraces()
	parent := td.ResourceSpans().AppendEmpty()
	parent.Resource().Attributes().PutStr("service.name", parentService)
	parentSpan := parent.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	parentSpan.SetTraceID(storedTraceID)
	parentSpan.SetSpanID(parentSpanID)
	parentSpan.SetName(parentOp)
	parentSpan.SetKind(ptrace.SpanKindServer)
	parentSpan.SetStartTimestamp(pcommon.NewTimestampFromTime(spanStart))
	parentSpan.SetEndTimestamp(pcommon.NewTimestampFromTime(spanStart.Add(time.Second)))

	child := td.ResourceSpans().AppendEmpty()
	child.Resource().Attributes().PutStr("service.name", childService)
	childSpan := child.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	childSpan.SetTraceID(storedTraceID)
	childSpan.SetSpanID(childSpanID)
	childSpan.SetParentSpanID(parentSpanID)
	childSpan.SetName(childOp)
	childSpan.SetKind(ptrace.SpanKindClient)
	childSpan.SetStartTimestamp(pcommon.NewTimestampFromTime(spanStart))
	childSpan.SetEndTimestamp(pcommon.NewTimestampFromTime(spanStart.Add(2 * time.Second)))
	return td
}

func injectedTrace() ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", injectedSvc)
	span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.SetTraceID(injectedID)
	span.SetSpanID(pcommon.SpanID{9})
	span.SetName("injected-op")
	span.SetKind(ptrace.SpanKindServer)
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(spanStart))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(spanStart.Add(time.Second)))
	return td
}

func traceQuery() *storage.TraceQueryParameters {
	return &storage.TraceQueryParameters{
		ServiceName: parentService,
		SearchDepth: 10,
	}
}

func callContext(t *testing.T, mode string) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	switch mode {
	case "empty-token":
		return metadata.AppendToOutgoingContext(ctx, bearertoken.Key, "")
	case "unknown-token":
		return metadata.AppendToOutgoingContext(ctx, bearertoken.Key, placeholderToken)
	default:
		return ctx
	}
}

func requireUnauthenticated(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, codes.Unauthenticated, status.Code(err), "%v", err)
}

func requireStreamRejected(t *testing.T, payload any, err error) {
	t.Helper()
	require.Nil(t, payload, "unauthenticated call returned a payload: %#v", payload)
	requireUnauthenticated(t, err)
}

// TestNoConfigFile_DoesNotBindEveryInterfaceWithoutTLS fails while the default
// listener has an empty host and no TLS. A fix binds a loopback host, or sets
// TLS before listening on an unspecified address.
func TestNoConfigFile_DoesNotBindEveryInterfaceWithoutTLS(t *testing.T) {
	fx := startNoConfigServer(t)
	tcpAddr, err := net.ResolveTCPAddr("tcp", fx.addr)
	require.NoError(t, err)
	if tcpAddr.IP == nil || tcpAddr.IP.IsUnspecified() {
		require.Truef(t, fx.tls, "listener %s is on every interface and TLS is unset", fx.addr)
	}
}

// TestNoConfigFile_UnauthenticatedClientsCannotReadOrWriteTraces fails while a
// plaintext client with no credential, an empty token, or a token the server
// was not configured with can read the memory store or export into it.
//
// grpc.health.v1.Health is not a trace read or write and is not covered here.
func TestNoConfigFile_UnauthenticatedClientsCannotReadOrWriteTraces(t *testing.T) {
	fx := startNoConfigServer(t)
	fx.assertStoreHoldsSeed(t)

	for _, mode := range []string{"no-token", "empty-token", "unknown-token"} {
		t.Run(mode, func(t *testing.T) {
			ctx := callContext(t, mode)
			conn := fx.dial(t)
			reader := storage.NewTraceReaderClient(conn)
			deps := storage.NewDependencyReaderClient(conn)
			caps := storage.NewCapabilitiesClient(conn)
			otlp := ptraceotlp.NewGRPCClient(conn)

			t.Run("GetServices", func(t *testing.T) {
				resp, err := reader.GetServices(ctx, &storage.GetServicesRequest{})
				if resp != nil && len(resp.GetServices()) > 0 {
					t.Fatalf("GetServices returned %v", resp.GetServices())
				}
				requireUnauthenticated(t, err)
			})

			t.Run("GetOperations", func(t *testing.T) {
				resp, err := reader.GetOperations(ctx, &storage.GetOperationsRequest{Service: parentService})
				if resp != nil && len(resp.GetOperations()) > 0 {
					t.Fatalf("GetOperations returned %#v", resp.GetOperations())
				}
				requireUnauthenticated(t, err)
			})

			t.Run("FindTraces", func(t *testing.T) {
				stream, err := reader.FindTraces(ctx, &storage.FindTracesRequest{Query: traceQuery()})
				if err != nil {
					requireUnauthenticated(t, err)
					return
				}
				td, recvErr := stream.Recv()
				requireStreamRejected(t, td, recvErr)
			})

			t.Run("GetTraces", func(t *testing.T) {
				stream, err := reader.GetTraces(ctx, &storage.GetTracesRequest{
					Query: []*storage.GetTraceParams{{TraceId: storedTraceID[:]}},
				})
				if err != nil {
					requireUnauthenticated(t, err)
					return
				}
				td, recvErr := stream.Recv()
				requireStreamRejected(t, td, recvErr)
			})

			t.Run("FindTraceIDs", func(t *testing.T) {
				resp, err := reader.FindTraceIDs(ctx, &storage.FindTraceIDsRequest{Query: traceQuery()})
				if resp != nil && len(resp.GetTraceIds()) > 0 {
					t.Fatalf("FindTraceIDs returned %#v", resp.GetTraceIds())
				}
				requireUnauthenticated(t, err)
			})

			t.Run("FindSpans", func(t *testing.T) {
				stream, err := reader.FindSpans(ctx, &storage.FindSpansRequest{
					Query: &storage.SpanQueryParameters{},
				})
				if err != nil {
					requireUnauthenticated(t, err)
					return
				}
				resp, recvErr := stream.Recv()
				if resp != nil && resp.GetSpans() != nil && ptrace.Traces(*resp.GetSpans()).SpanCount() > 0 {
					t.Fatalf("FindSpans returned %d spans", ptrace.Traces(*resp.GetSpans()).SpanCount())
				}
				requireStreamRejected(t, nil, recvErr)
			})

			t.Run("FindTraceSummaries", func(t *testing.T) {
				stream, err := reader.FindTraceSummaries(ctx, &storage.FindTraceSummariesRequest{Query: traceQuery()})
				if err != nil {
					requireUnauthenticated(t, err)
					return
				}
				resp, recvErr := stream.Recv()
				if resp != nil && len(resp.GetSummaries()) > 0 {
					t.Fatalf("FindTraceSummaries returned %#v", resp.GetSummaries())
				}
				requireStreamRejected(t, nil, recvErr)
			})

			t.Run("GetDependencies", func(t *testing.T) {
				resp, err := deps.GetDependencies(ctx, &storage.GetDependenciesRequest{
					StartTime: spanStart.Add(-time.Hour),
					EndTime:   spanStart.Add(time.Hour),
				})
				if resp != nil && len(resp.GetDependencies()) > 0 {
					t.Fatalf("GetDependencies returned %#v", resp.GetDependencies())
				}
				requireUnauthenticated(t, err)
			})

			t.Run("GetCapabilities", func(t *testing.T) {
				resp, err := caps.GetCapabilities(ctx, &storage.GetCapabilitiesRequest{})
				if err == nil {
					t.Fatalf("GetCapabilities succeeded: %+v", resp)
				}
				requireUnauthenticated(t, err)
			})

			t.Run("Export", func(t *testing.T) {
				before := fx.writer.calls
				_, err := otlp.Export(ctx, ptraceotlp.NewExportRequestFromTraces(injectedTrace()))
				requireUnauthenticated(t, err)
				require.Equal(t, before, fx.writer.calls, "Export reached the store before the call was refused")
				fx.assertInjectedTraceAbsent(ctx, t)
			})

			t.Run("reflection", func(t *testing.T) {
				reflector := grpc_reflection_v1.NewServerReflectionClient(conn)
				stream, err := reflector.ServerReflectionInfo(ctx)
				if err != nil {
					requireReflectionClosed(t, err)
					return
				}
				err = stream.Send(&grpc_reflection_v1.ServerReflectionRequest{
					MessageRequest: &grpc_reflection_v1.ServerReflectionRequest_ListServices{
						ListServices: "*",
					},
				})
				if err != nil {
					requireReflectionClosed(t, err)
					return
				}
				resp, recvErr := stream.Recv()
				if recvErr == nil {
					t.Fatalf("reflection listed services without a credential: %v", resp.GetListServicesResponse().GetService())
				}
				requireReflectionClosed(t, recvErr)
			})
		})
	}

	fx.assertStoreHoldsSeed(t)
}

func requireReflectionClosed(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	code := status.Code(err)
	require.Contains(t, []codes.Code{codes.Unauthenticated, codes.Unimplemented}, code, "%v", err)
}

func (s *startedServer) assertStoreHoldsSeed(t *testing.T) {
	t.Helper()
	services, err := s.reader.GetServices(context.Background())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{parentService, childService}, services)

	deps, err := s.deps.GetDependencies(context.Background(), depstore.QueryParameters{
		StartTime: spanStart.Add(-time.Hour),
		EndTime:   spanStart.Add(time.Hour),
	})
	require.NoError(t, err)
	require.Len(t, deps, 1)
	require.Equal(t, parentService, deps[0].Parent)
	require.Equal(t, childService, deps[0].Child)
}

func (s *startedServer) assertInjectedTraceAbsent(ctx context.Context, t *testing.T) {
	t.Helper()
	var found int
	for traces, err := range s.reader.GetTraces(ctx, tracestore.GetTraceParams{TraceID: injectedID}) {
		require.NoError(t, err)
		found += len(traces)
	}
	require.Zero(t, found, "exported trace was stored")
	services, err := s.reader.GetServices(ctx)
	require.NoError(t, err)
	require.NotContains(t, services, injectedSvc)
}
