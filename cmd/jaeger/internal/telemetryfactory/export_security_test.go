// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package telemetryfactory

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/service/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerstorage"
)

// The jaeger binary installs createTracerProvider as its telemetry tracer
// provider. Query and storage components must export over TLS when the OTLP
// endpoint is a host:port and the insecure variable is unset.

func TestCreateTracerProviderHostPortUsesTLS(t *testing.T) {
	for _, id := range []string{jaegerquery.ID.String(), jaegerstorage.ID.String()} {
		t.Run(id, func(t *testing.T) {
			isolateFactoryOTLPEnv(t)
			endpoint, got := captureFactoryDial(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)

			provider, err := createTracerProvider(context.Background(), telemetry.TracerSettings{}, nil)
			require.NoError(t, err)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = provider.Shutdown(ctx)
			})

			tr := provider.Tracer("component", trace.WithInstrumentationAttributes(
				attribute.String("otelcol.component.id", id),
			))
			_, span := tr.Start(context.Background(), "jaeger-component-export")
			span.SetAttributes(attribute.String("queried.trace_id", "00112233445566778899aabbccddeeff"))
			span.End()

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = provider.Shutdown(ctx)

			opening := awaitFactoryDial(t, got)
			require.GreaterOrEqual(t, len(opening), 3)
			require.Falsef(t, bytes.HasPrefix(opening, []byte("PRI * HTTP/2.0")),
				"component %s exported over plaintext HTTP/2; opening %q", id, string(opening[:min(48, len(opening))]))
			require.Equalf(t, byte(0x16), opening[0],
				"component %s first byte %#x is not a TLS handshake; opening %q", id, opening[0], string(opening[:min(48, len(opening))]))
			require.Equalf(t, byte(0x03), opening[1],
				"component %s second byte %#x is not a TLS record version", id, opening[1])
			require.NotContains(t, opening, []byte("00112233445566778899aabbccddeeff"))
		})
	}
}

func isolateFactoryOTLPEnv(t *testing.T) {
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
}

func captureFactoryDial(t *testing.T) (string, <-chan []byte) {
	t.Helper()
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
	return net.JoinHostPort("localhost", port), got
}

func awaitFactoryDial(t *testing.T, got <-chan []byte) []byte {
	t.Helper()
	select {
	case b := <-got:
		require.NotEmpty(t, b)
		return b
	case <-time.After(6 * time.Second):
		t.Fatal("exporter did not connect to the configured endpoint")
		return nil
	}
}
