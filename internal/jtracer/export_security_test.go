// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package jtracer

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
)

// These tests are the specification for internal trace export.
// A host:port OTLP endpoint, and an unset or non-true insecure variable, must
// open TLS. Plaintext is allowed only when the insecure variable is true.
// They fail while defaultGRPCOptions appends otlptracegrpc.WithInsecure for
// every endpoint that does not start with the literal prefix "https://".

const (
	exportSpanName = "jaeger-internal-export"
	// exportTraceID is a fixture standing in for a trace id a query span would carry.
	exportTraceID = "00112233445566778899aabbccddeeff"
)

func TestHostPortEndpointWithoutInsecureUsesTLS(t *testing.T) {
	isolateOTLPEnv(t)
	endpoint, got := captureDial(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)

	provider := startInternalTracer(t)
	exportSpan(t, provider, exportSpanName, exportTraceID)

	requireTLSClientHello(t, awaitDial(t, got))
}

func TestHTTPSSchemeCaseInsensitiveUsesTLS(t *testing.T) {
	isolateOTLPEnv(t)
	endpoint, got := captureDial(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "HTTPS://"+endpoint)

	provider := startInternalTracer(t)
	exportSpan(t, provider, exportSpanName, exportTraceID)

	requireTLSClientHello(t, awaitDial(t, got))
}

func TestHTTPSEndpointUsesTLS(t *testing.T) {
	isolateOTLPEnv(t)
	endpoint, got := captureDial(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+endpoint)

	provider := startInternalTracer(t)
	exportSpan(t, provider, exportSpanName, exportTraceID)

	requireTLSClientHello(t, awaitDial(t, got))
}

func TestInsecureFalseUsesTLS(t *testing.T) {
	isolateOTLPEnv(t)
	endpoint, got := captureDial(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "false")

	provider := startInternalTracer(t)
	exportSpan(t, provider, exportSpanName, exportTraceID)

	requireTLSClientHello(t, awaitDial(t, got))
}

func TestInsecureFalseCaseInsensitiveUsesTLS(t *testing.T) {
	isolateOTLPEnv(t)
	endpoint, got := captureDial(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "FALSE")

	provider := startInternalTracer(t)
	exportSpan(t, provider, exportSpanName, exportTraceID)

	requireTLSClientHello(t, awaitDial(t, got))
}

func TestTracesEndpointHostPortWithoutInsecureUsesTLS(t *testing.T) {
	isolateOTLPEnv(t)
	endpoint, got := captureDial(t)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", endpoint)

	provider := startInternalTracer(t)
	exportSpan(t, provider, exportSpanName, exportTraceID)

	requireTLSClientHello(t, awaitDial(t, got))
}

func TestTracesInsecureFalseUsesTLS(t *testing.T) {
	isolateOTLPEnv(t)
	endpoint, got := captureDial(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_INSECURE", "false")

	provider := startInternalTracer(t)
	exportSpan(t, provider, exportSpanName, exportTraceID)

	requireTLSClientHello(t, awaitDial(t, got))
}

func TestExplicitInsecureDeliversSpanInCleartext(t *testing.T) {
	for _, value := range []string{"true", "TRUE"} {
		t.Run(value, func(t *testing.T) {
			isolateOTLPEnv(t)
			endpoint, spans := startPlaintextCollector(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
			t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", value)

			provider := startInternalTracer(t)
			exportSpan(t, provider, exportSpanName, exportTraceID)

			requireCleartextSpan(t, spans)
		})
	}
}

func TestTracesInsecureTrueDeliversSpanInCleartext(t *testing.T) {
	isolateOTLPEnv(t)
	endpoint, spans := startPlaintextCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", endpoint)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_INSECURE", "true")

	provider := startInternalTracer(t)
	exportSpan(t, provider, exportSpanName, exportTraceID)

	requireCleartextSpan(t, spans)
}

func TestHostPortEndpointDoesNotDeliverSpansToPlaintextCollector(t *testing.T) {
	isolateOTLPEnv(t)
	endpoint, spans := startPlaintextCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)

	provider := startInternalTracer(t)
	exportSpan(t, provider, exportSpanName, exportTraceID)

	require.Empty(t, spans.snapshot(), "plaintext collector read internal spans, including queried.trace_id=%s", exportTraceID)
}

func isolateOTLPEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_INSECURE",
		"OTEL_EXPORTER_OTLP_TRACES_INSECURE",
		"OTEL_EXPORTER_OTLP_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_KEY",
		"OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY",
		"OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		"OTEL_EXPORTER_OTLP_HEADERS",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
		"OTEL_SDK_DISABLED",
	} {
		t.Setenv(key, "")
	}
}

func startInternalTracer(t *testing.T) *sdktrace.TracerProvider {
	t.Helper()
	provider, closer, err := NewProvider(context.Background(), "jaeger")
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
	})
	sdkProvider, ok := provider.(*sdktrace.TracerProvider)
	require.True(t, ok)
	return sdkProvider
}

func exportSpan(t *testing.T, provider *sdktrace.TracerProvider, name, traceID string) {
	t.Helper()
	_, span := provider.Tracer("internal-export-test").Start(context.Background(), name)
	span.SetAttributes(attribute.String("queried.trace_id", traceID))
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = provider.ForceFlush(ctx)
}

// captureDial listens on 127.0.0.1 and returns a host:port endpoint whose host
// is the name "localhost". The OTLP exporter keeps that form as the gRPC target;
// an IPv4 literal in the same variable is rejected by the exporter's URL parser
// and is not a dial this test can observe.
func captureDial(t *testing.T) (string, <-chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	endpoint := net.JoinHostPort("localhost", port)

	got := make(chan []byte, 1)
	go func() {
		tcpLn, ok := ln.(*net.TCPListener)
		if ok {
			_ = tcpLn.SetDeadline(time.Now().Add(6 * time.Second))
		}
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 4096)
		n, _ := conn.Read(buf)
		if n > 0 {
			got <- bytes.Clone(buf[:n])
		}
	}()
	return endpoint, got
}

func awaitDial(t *testing.T, got <-chan []byte) []byte {
	t.Helper()
	select {
	case b, ok := <-got:
		require.True(t, ok && len(b) > 0, "exporter connected without sending bytes")
		return b
	case <-time.After(6 * time.Second):
		t.Fatal("exporter did not connect to the configured endpoint")
		return nil
	}
}

func requireTLSClientHello(t *testing.T, opening []byte) {
	t.Helper()
	require.GreaterOrEqual(t, len(opening), 3)
	require.Falsef(t, bytes.HasPrefix(opening, []byte("PRI * HTTP/2.0")),
		"export used plaintext HTTP/2; opening %q", printablePrefix(opening))
	require.Equalf(t, byte(0x16), opening[0],
		"first byte %#x is not a TLS handshake record; opening %q", opening[0], printablePrefix(opening))
	require.Equalf(t, byte(0x03), opening[1],
		"second byte %#x is not a TLS record version; opening %q", opening[1], printablePrefix(opening))
	require.NotContains(t, opening, []byte(exportSpanName))
	require.NotContains(t, opening, []byte(exportTraceID))
}

func printablePrefix(b []byte) string {
	if len(b) > 48 {
		b = b[:48]
	}
	return string(b)
}

type spanCapture struct {
	coltracepb.UnimplementedTraceServiceServer
	mu    sync.Mutex
	spans []capturedSpan
}

type capturedSpan struct {
	name  string
	attrs map[string]string
}

func (s *spanCapture) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				attrs := make(map[string]string, len(sp.GetAttributes()))
				for _, kv := range sp.GetAttributes() {
					if v := kv.GetValue(); v != nil {
						attrs[kv.GetKey()] = v.GetStringValue()
					}
				}
				s.spans = append(s.spans, capturedSpan{name: sp.GetName(), attrs: attrs})
			}
		}
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func (s *spanCapture) snapshot() []capturedSpan {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]capturedSpan, len(s.spans))
	copy(out, s.spans)
	return out
}

func startPlaintextCollector(t *testing.T) (string, *spanCapture) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	capture := &spanCapture{}
	coltracepb.RegisterTraceServiceServer(srv, capture)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = ln.Close()
	})

	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	return net.JoinHostPort("localhost", port), capture
}

func requireCleartextSpan(t *testing.T, spans *spanCapture) {
	t.Helper()
	require.Eventually(t, func() bool {
		return len(spans.snapshot()) > 0
	}, 4*time.Second, 20*time.Millisecond, "plaintext collector did not receive the exported span")
	got := spans.snapshot()
	require.Equal(t, exportSpanName, got[0].name)
	require.Equal(t, exportTraceID, got[0].attrs["queried.trace_id"])
}
