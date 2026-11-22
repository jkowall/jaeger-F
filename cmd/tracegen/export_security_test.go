// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// tracegen's OTLP exporters always pass WithInsecure. A host:port or https
// endpoint must use TLS unless the insecure variable is true.

func TestTracegenGRPCHostPortWithoutInsecureUsesTLS(t *testing.T) {
	assertTracegenUsesTLS(t, "otlp-grpc", func(endpoint string) {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	})
}

func TestTracegenGRPCHTTPSEndpointUsesTLS(t *testing.T) {
	assertTracegenUsesTLS(t, "otlp-grpc", func(endpoint string) {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+endpoint)
	})
}

func TestTracegenGRPCInsecureFalseUsesTLS(t *testing.T) {
	assertTracegenUsesTLS(t, "otlp-grpc", func(endpoint string) {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
		t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "false")
	})
}

func TestTracegenHTTPHTTPSEndpointUsesTLS(t *testing.T) {
	assertTracegenUsesTLS(t, "otlp-http", func(endpoint string) {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+endpoint)
	})
}

func TestTracegenGRPCExplicitInsecureStaysPlaintext(t *testing.T) {
	isolateTracegenEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	got := make(chan []byte, 1)
	go func() {
		if tcpLn, ok := ln.(*net.TCPListener); ok {
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
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", net.JoinHostPort("localhost", port))
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")

	exp, err := createOtelExporter("otlp-grpc")
	require.NoError(t, err)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tp.Shutdown(ctx)
	})
	_, span := tp.Tracer("tracegen").Start(context.Background(), "tracegen-export")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tp.ForceFlush(ctx)

	select {
	case opening := <-got:
		require.Truef(t, bytes.HasPrefix(opening, []byte("PRI * HTTP/2.0")),
			"explicit insecure export was not plaintext HTTP/2; opening %q", string(opening[:min(48, len(opening))]))
	case <-time.After(6 * time.Second):
		t.Fatal("exporter did not connect to the configured endpoint")
	}
}

func assertTracegenUsesTLS(t *testing.T, exporterType string, setEndpoint func(endpoint string)) {
	t.Helper()
	isolateTracegenEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	got := make(chan []byte, 1)
	go func() {
		if tcpLn, ok := ln.(*net.TCPListener); ok {
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
	setEndpoint(net.JoinHostPort("localhost", port))

	exp, err := createOtelExporter(exporterType)
	require.NoError(t, err)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tp.Shutdown(ctx)
	})
	_, span := tp.Tracer("tracegen").Start(context.Background(), "tracegen-export")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tp.ForceFlush(ctx)

	select {
	case opening := <-got:
		require.GreaterOrEqual(t, len(opening), 3)
		require.Falsef(t, bytes.HasPrefix(opening, []byte("PRI * HTTP/2.0")) || bytes.HasPrefix(opening, []byte("POST ")),
			"%s export was plaintext; opening %q", exporterType, string(opening[:min(48, len(opening))]))
		require.Equalf(t, byte(0x16), opening[0], "%s first byte %#x is not a TLS handshake", exporterType, opening[0])
		require.Equalf(t, byte(0x03), opening[1], "%s second byte %#x is not a TLS record version", exporterType, opening[1])
	case <-time.After(6 * time.Second):
		t.Fatal("exporter did not connect to the configured endpoint")
	}
}

func isolateTracegenEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_INSECURE",
		"OTEL_EXPORTER_OTLP_TRACES_INSECURE",
		"OTEL_EXPORTER_OTLP_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE",
		"OTEL_SDK_DISABLED",
	} {
		t.Setenv(key, "")
	}
	// Bound each export so a refused TLS handshake does not wait out the default 10s.
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "1000")
}
