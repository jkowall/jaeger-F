// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/jaegertracing/jaeger/cmd/internal/storageconfig"
	"github.com/jaegertracing/jaeger/internal/metrics"
	"github.com/jaegertracing/jaeger/internal/proto-gen/storage/v2"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/depstore"
	"github.com/jaegertracing/jaeger/internal/telemetry"
	conventions "github.com/jaegertracing/jaeger/internal/telemetry/otelsemconv"
	"github.com/jaegertracing/jaeger/internal/tenancy"
	"github.com/jaegertracing/jaeger/ports"
)

// Profiling routes mounted together by AdminServer.registerPprofHandlers and
// Service.Start. A later fix has to close every one of them to a remote
// unauthenticated client; removing only /debug/pprof/profile leaves the rest.
var remoteProfilingPaths = []string{
	"/debug/pprof/",
	"/debug/pprof/cmdline",
	"/debug/pprof/symbol",
	"/debug/pprof/goroutine?debug=2",
	"/debug/pprof/heap?debug=1",
	"/debug/pprof/allocs?debug=1",
	"/debug/pprof/threadcreate?debug=1",
	"/debug/pprof/block?debug=1",
	"/debug/pprof/mutex?debug=1",
	"/debug/pprof/goroutineleak?debug=2",
	"/debug/vars",
}

// TestDefaultRemoteStorageDoesNotServeProfilingToRemoteClients is the spec for
// the unauthenticated admin profiling listener. It fails while a client that
// is not on loopback can call those handlers without credentials, and it
// passes once that client cannot.
func TestDefaultRemoteStorageDoesNotServeProfilingToRemoteClients(t *testing.T) {
	t.Run("no configuration file", func(t *testing.T) {
		// main.loadConfig returns DefaultConfig when --config-file is empty.
		cfg := DefaultConfig()
		require.Equal(t, ports.PortToHostPort(ports.RemoteStorageAdminHTTP), cfg.Service.Admin.Endpoint)
		require.False(t, cfg.Service.Admin.TLS.HasValue())
		require.Equal(t, ports.PortToHostPort(ports.RemoteStorageGRPC), cfg.GRPC.NetAddr.Endpoint)
		assertRemoteClientCannotProfile(t, cfg)
	})

	t.Run("config file leaves admin and tls unset", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "remote-storage.yaml")
		// No admin section and no tls key: both stay on the defaults.
		body := []byte("storage:\n  backends:\n    memory:\n      memory:\n        max_traces: 1000\n")
		require.NoError(t, os.WriteFile(path, body, 0o600))
		cfg, err := LoadConfigFile(context.Background(), path)
		require.NoError(t, err)
		require.Equal(t, ports.PortToHostPort(ports.RemoteStorageAdminHTTP), cfg.Service.Admin.Endpoint)
		require.False(t, cfg.Service.Admin.TLS.HasValue())
		// The no-file case already registered the default Prometheus collectors.
		// Metrics are not part of this listener; skip a second registration.
		cfg.Service.Metrics.Backend = "none"
		assertRemoteClientCannotProfile(t, cfg)
	})
}

// assertRemoteClientCannotProfile starts remote-storage the way main does and
// requests every profiling route from a non-loopback address with no
// credentials. The port is ephemeral; the host is whatever the configuration
// under test chose, so a loopback default refuses the dial and a wildcard
// default accepts it.
func assertRemoteClientCannotProfile(t *testing.T, cfg *Config) {
	t.Helper()

	adminPort := freeTCPPort(t)
	grpcPort := freeTCPPort(t)
	cfg.Service.Admin.Endpoint = replacePort(t, cfg.Service.Admin.Endpoint, adminPort)
	cfg.GRPC.NetAddr.Endpoint = replacePort(t, cfg.GRPC.NetAddr.Endpoint, grpcPort)

	svc, server := startLikeMain(t, cfg)
	t.Cleanup(func() {
		require.NoError(t, server.Close())
		require.NoError(t, svc.Admin.Close())
		signalStop(svc)
	})

	remoteIP := nonLoopbackIPv4(t)
	base := "http://" + net.JoinHostPort(remoteIP.String(), adminPort)
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
	t.Cleanup(client.CloseIdleConnections)

	for _, path := range remoteProfilingPaths {
		status, hdr, body, err := getUnauthenticated(client, base+path)
		if isListenerRefused(err) {
			continue
		}
		require.NoError(t, err)
		if profilingHandlerServed(status, hdr) {
			t.Errorf("GET %s from %s was served without credentials: status=%d content-type=%q x-go-pprof=%q body=%q",
				path, remoteIP, status, hdr.Get("Content-Type"), hdr.Get("X-Go-Pprof"), snippet(body))
		}
	}

	// Profile with no seconds parameter uses the handler default of 30s and
	// holds the process-wide CPU profiler until the client goes away. A second
	// profile overlaps it. Storage reads and writes run while the first is in progress.
	profileCtx, cancelProfile := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancelProfile()
	firstProfile := make(chan error, 1)
	go func() {
		_, _, _, err := getUnauthenticatedContext(profileCtx, client, base+"/debug/pprof/profile")
		firstProfile <- err
	}()

	// Give the first handler time to reach StartCPUProfile before the second request.
	time.Sleep(200 * time.Millisecond)

	secondStatus, secondHdr, secondBody, secondErr := getUnauthenticated(client, base+"/debug/pprof/profile?seconds=1")
	storageCtx, storageCancel := context.WithTimeout(context.Background(), time.Second)
	storageStarted := time.Now()
	storageErr := storageWriteAndRead(storageCtx, net.JoinHostPort("127.0.0.1", grpcPort))
	storageElapsed := time.Since(storageStarted)
	storageCancel()

	firstErr := <-firstProfile
	cancelProfile()

	if !isListenerRefused(firstErr) && profilingStillRunning(firstErr) {
		t.Errorf("GET /debug/pprof/profile from %s started a CPU profile without credentials and was still running after 1.5s (handler default is 30s); overlapping profile status=%d x-go-pprof=%q body=%q; storage write+read while it ran: err=%v elapsed=%s",
			remoteIP, secondStatus, secondHdr.Get("X-Go-Pprof"), snippet(secondBody), storageErr, storageElapsed)
	} else if !isListenerRefused(firstErr) && !isListenerRefused(secondErr) && profilingHandlerServed(secondStatus, secondHdr) {
		t.Errorf("GET /debug/pprof/profile?seconds=1 from %s was served without credentials: status=%d content-type=%q x-go-pprof=%q body=%q; storage write+read: err=%v elapsed=%s",
			remoteIP, secondStatus, secondHdr.Get("Content-Type"), secondHdr.Get("X-Go-Pprof"), snippet(secondBody), storageErr, storageElapsed)
	} else if isListenerRefused(firstErr) || isListenerRefused(secondErr) {
		// Loopback-only listener: the remote client never reached a handler.
	} else if firstErr != nil && !errors.Is(firstErr, context.DeadlineExceeded) {
		t.Errorf("GET /debug/pprof/profile from %s failed for a reason other than refusal: %v", remoteIP, firstErr)
	}

	traceStatus, traceHdr, traceBody, traceErr := getUnauthenticated(client, base+"/debug/pprof/trace?seconds=1")
	if isListenerRefused(traceErr) {
		return
	}
	require.NoError(t, traceErr)
	if profilingHandlerServed(traceStatus, traceHdr) {
		t.Errorf("GET /debug/pprof/trace?seconds=1 from %s was served without credentials: status=%d content-type=%q x-go-pprof=%q body=%q",
			remoteIP, traceStatus, traceHdr.Get("Content-Type"), traceHdr.Get("X-Go-Pprof"), snippet(traceBody))
	}
}

func startLikeMain(t *testing.T, cfg *Config) (*Service, *Server) {
	t.Helper()

	svc := NewService()
	require.NoError(t, svc.Start(cfg.Service))

	baseFactory := svc.MetricsFactory.Namespace(metrics.NSOptions{Name: "jaeger"})
	metricsFactory := baseFactory.Namespace(metrics.NSOptions{Name: "remote-storage"})

	baseTelset := telemetry.Settings{
		Logger:        svc.Logger,
		Metrics:       baseFactory,
		Host:          svc.Admin.Host(),
		MeterProvider: noopmetric.NewMeterProvider(),
	}
	tm := tenancy.NewManager(&cfg.Tenancy)
	telset := baseTelset
	telset.Metrics = metricsFactory

	storageName := cfg.GetStorageName()
	require.NotEmpty(t, storageName)
	backend, ok := cfg.Storage.TraceBackends[storageName]
	require.True(t, ok)

	traceFactory, err := storageconfig.CreateTraceStorageFactory(
		context.Background(),
		storageName,
		backend,
		telset,
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		if closer, ok := traceFactory.(io.Closer); ok {
			require.NoError(t, closer.Close())
		}
	})
	depFactory, ok := traceFactory.(depstore.Factory)
	require.True(t, ok, "storage %s does not implement dependency store", storageName)

	server, err := NewServer(context.Background(), cfg.GRPC, traceFactory, depFactory, tm, telset)
	require.NoError(t, err)
	require.NoError(t, server.Start(context.Background()))
	svc.Admin.Host().Ready()
	return svc, server
}

func storageWriteAndRead(ctx context.Context, addr string) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	traces := ptrace.NewTraces()
	rs := traces.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr(conventions.ServiceNameKey, "pprof-exposure")
	span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.SetName("ping")
	span.SetTraceID(pcommon.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	span.SetSpanID(pcommon.SpanID{1, 2, 3, 4, 5, 6, 7, 8})
	now := pcommon.NewTimestampFromTime(time.Now())
	span.SetStartTimestamp(now)
	span.SetEndTimestamp(now)

	if _, err := ptraceotlp.NewGRPCClient(conn).Export(ctx, ptraceotlp.NewExportRequestFromTraces(traces)); err != nil {
		return errors.New("export: " + err.Error())
	}
	resp, err := storage.NewTraceReaderClient(conn).GetServices(ctx, &storage.GetServicesRequest{})
	if err != nil {
		return errors.New("get services: " + err.Error())
	}
	for _, name := range resp.GetServices() {
		if name == "pprof-exposure" {
			return nil
		}
	}
	return errors.New("written service missing from " + strings.Join(resp.GetServices(), ","))
}

func getUnauthenticated(client *http.Client, url string) (int, http.Header, []byte, error) {
	return getUnauthenticatedContext(context.Background(), client, url)
}

func getUnauthenticatedContext(ctx context.Context, client *http.Client, url string) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Close = true
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header.Clone(), body, err
}

// profilingHandlerServed reports whether the admin mux dispatched the request
// to a profiling or expvar handler. A loopback bind never gets here. A removed
// route is the stock 404, and an authenticator refuses before the handler.
func profilingHandlerServed(status int, hdr http.Header) bool {
	if hdr != nil && hdr.Get("X-Go-Pprof") != "" {
		return true
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return false
	default:
		return true
	}
}

func profilingStillRunning(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

func isListenerRefused(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && errno == syscall.ECONNREFUSED {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return errors.Is(op.Err, syscall.ECONNREFUSED)
	}
	return false
}

func nonLoopbackIPv4(t *testing.T) net.IP {
	t.Helper()
	ifaces, err := net.Interfaces()
	require.NoError(t, err)
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		require.NoError(t, err)
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
				continue
			}
			return ipNet.IP.To4()
		}
	}
	t.Fatal("no non-loopback IPv4 address; a remote client cannot be exercised")
	return nil
}

func freeTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	return port
}

func replacePort(t *testing.T, hostport, port string) string {
	t.Helper()
	host, _, err := net.SplitHostPort(hostport)
	require.NoError(t, err)
	return net.JoinHostPort(host, port)
}

func signalStop(svc *Service) {
	// NewService registers the process signal handlers. Drop them so a later
	// test in this package does not inherit an extra notification target.
	if svc != nil && svc.signalsChannel != nil {
		signal.Stop(svc.signalsChannel)
	}
}

func snippet(body []byte) string {
	body = bytes.ToValidUTF8(body, []byte("."))
	text := strings.ReplaceAll(string(body), "\n", " ")
	if len(text) > 180 {
		text = text[:180]
	}
	return text
}
