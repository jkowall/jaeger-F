// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"io"
	"iter"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/confignet"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/pdata/ptrace"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap/zaptest"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/internal/jaegerai"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/querysvc"
	"github.com/jaegertracing/jaeger/internal/proto-gen/api_v2/metrics"
	metricstore "github.com/jaegertracing/jaeger/internal/storage/v1/api/metricstore"
	metricsmocks "github.com/jaegertracing/jaeger/internal/storage/v1/api/metricstore/mocks"
	depsmocks "github.com/jaegertracing/jaeger/internal/storage/v2/api/depstore/mocks"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
	tracestoremocks "github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore/mocks"
	"github.com/jaegertracing/jaeger/internal/tenancy"
)

// rebindingName is the Host a browser sends after the name it loaded has been
// pointed at the query port. It is not a loopback literal. The test installs
// it in the process resolver as 127.0.0.1, which is what the attacker's DNS
// server answers, so a check that looks the name up and then allows that
// address still has to fail.
const rebindingName = "rebinding.invalid"

const (
	storedServiceMarker    = "stored-service-marker"
	storedOperationMarker  = "stored-operation-marker"
	storedTraceMarker      = "stored-trace-marker"
	storedSummaryMarker    = "stored-summary-marker"
	storedSpanMarker       = "stored-span-marker"
	storedDependencyMarker = "stored-dependency-marker"
	storedMetricMarker     = "stored-metric-marker"
	storedDimensionMarker  = "stored-dimension-marker"
)

const traceSearch = "?query.serviceName=frontend&query.startTimeMin=1970-01-01T00:00:00Z&query.startTimeMax=1970-01-02T00:00:00Z"

// storedRoutes are the query HTTP operations that read or write stored
// telemetry. Each one is registered on the same mux as GET /api/v3/services
// and GET /api/v3/traces, with no Host check of its own.
//
// Not in this list, and not a stored-trace read: GET /api/deep-dependencies
// and GET /api/quality-metrics return built-in sample documents. The gRPC
// query service is a separate listener and is not covered: a browser page
// cannot speak gRPC to it, and the HTTP server does not translate gRPC.
var storedRoutes = []struct {
	name   string
	method string
	path   string
	marker string
}{
	{name: "services", method: http.MethodGet, path: "/api/v3/services", marker: storedServiceMarker},
	{name: "operations", method: http.MethodGet, path: "/api/v3/operations?service=frontend", marker: storedOperationMarker},
	{name: "find traces", method: http.MethodGet, path: "/api/v3/traces" + traceSearch, marker: storedTraceMarker},
	{name: "get trace", method: http.MethodGet, path: "/api/v3/traces/123456aBC", marker: storedTraceMarker},
	{name: "trace summaries", method: http.MethodGet, path: "/api/v3/trace-summaries" + traceSearch, marker: storedSummaryMarker},
	{name: "spans", method: http.MethodGet, path: "/api/v3/spans" + traceSearch, marker: storedSpanMarker},
	{name: "capabilities", method: http.MethodGet, path: "/api/v3/capabilities"},
	{name: "dependencies", method: http.MethodGet, path: "/api/dependencies?endTs=1476374248550", marker: storedDependencyMarker},
	{name: "archive", method: http.MethodPost, path: "/api/archive/123456aBC"},
	{name: "metric calls", method: http.MethodGet, path: "/api/metrics/calls?service=frontend", marker: storedMetricMarker},
	{name: "metric errors", method: http.MethodGet, path: "/api/metrics/errors?service=frontend", marker: storedMetricMarker},
	{name: "metric latencies", method: http.MethodGet, path: "/api/metrics/latencies?service=frontend&quantile=0.95", marker: storedMetricMarker},
	{name: "metric dimensions", method: http.MethodGet, path: "/api/metrics/dimensions", marker: storedDimensionMarker},
}

// TestQueryHTTPRejectsRebindingHostBeforeReadingStorage is the DNS-rebinding
// case the MCP handler already rejects. The query server is listening on
// loopback (or a wildcard listener, with the TCP connection arriving on
// loopback, which is what ":16686" does). The Host header is rebindingName,
// including the shapes that only look like a loopback name. The response must
// be 403 and storage must not have been read or written: a check that runs
// after the query, or that allows the name because it resolves to 127.0.0.1,
// fails this test.
func TestQueryHTTPRejectsRebindingHostBeforeReadingStorage(t *testing.T) {
	pinRebindingHost(t)

	t.Run("loopback listener", func(t *testing.T) {
		fx := startQueryHTTP(t, "127.0.0.1:0", "", "", false)
		fx.requireMCPRejects(t)
		rejectStoredRoutes(t, fx, fx.rebindingHost())
		for _, host := range []string{
			rebindingName,
			"localhost." + rebindingName,
			"127.0.0.1." + rebindingName,
		} {
			t.Run("host "+host, func(t *testing.T) {
				fx.reject(t, http.MethodGet, "/api/v3/services", host, storedServiceMarker)
				fx.reject(t, http.MethodGet, "/api/v3/traces/123456aBC", host, storedTraceMarker)
			})
		}
	})

	t.Run("wildcard listener dialed on ipv4 loopback", func(t *testing.T) {
		fx := startQueryHTTP(t, ":0", "127.0.0.1", "", false)
		fx.requireMCPRejects(t)
		rejectStoredRoutes(t, fx, fx.rebindingHost())
	})

	t.Run("ipv6 loopback listener", func(t *testing.T) {
		fx := startQueryHTTP(t, "[::1]:0", "", "", false)
		fx.requireMCPRejects(t)
		rejectStoredRoutes(t, fx, fx.rebindingHost())
	})

	t.Run("base path", func(t *testing.T) {
		fx := startQueryHTTP(t, "127.0.0.1:0", "", "/jaeger", false)
		fx.reject(t, http.MethodGet, "/api/v3/services", fx.rebindingHost(), storedServiceMarker)
		fx.reject(t, http.MethodGet, "/api/v3/traces"+traceSearch, fx.rebindingHost(), storedTraceMarker)
	})

	t.Run("tenancy header does not authorize the host", func(t *testing.T) {
		fx := startQueryHTTP(t, "127.0.0.1:0", "", "", true)
		fx.reject(t, http.MethodGet, "/api/v3/services", fx.rebindingHost(), storedServiceMarker)
		fx.reject(t, http.MethodGet, "/api/v3/traces/123456aBC", fx.rebindingHost(), storedTraceMarker)
	})
}

// TestQueryHTTPServesLoopbackHost keeps the ordinary UI request working: the
// browser's Host is a loopback name when the page was opened on one.
func TestQueryHTTPServesLoopbackHost(t *testing.T) {
	fx := startQueryHTTP(t, "127.0.0.1:0", "", "", false)
	_, port, err := net.SplitHostPort(fx.dialAddr)
	require.NoError(t, err)
	for _, host := range []string{
		net.JoinHostPort("127.0.0.1", port),
		net.JoinHostPort("localhost", port),
		"localhost",
		net.JoinHostPort("::1", port),
	} {
		t.Run("host "+host, func(t *testing.T) {
			fx.allowStoredRoutes(t, host)
		})
	}
}

// TestQueryHTTPAllowsExternalHostOnNonLoopbackConnection is the request a
// reverse proxy makes when it dials a non-loopback address and preserves the
// caller's Host. The loopback guard must not reject it. rebindingName still
// resolves to 127.0.0.1, so a check that decides from that lookup instead of
// from the accepted connection fails this test.
func TestQueryHTTPAllowsExternalHostOnNonLoopbackConnection(t *testing.T) {
	pinRebindingHost(t)
	ip := firstNonLoopbackIPv4(t)
	fx := startQueryHTTP(t, net.JoinHostPort(ip, "0"), "", "", false)
	code, body := fx.do(http.MethodGet, "/api/v3/services", fx.rebindingHost())
	assert.Equalf(t, http.StatusOK, code, "body: %s", body)
	assert.Contains(t, body, storedServiceMarker)

	code, body = fx.do(http.MethodGet, "/api/v3/traces/123456aBC", fx.rebindingHost())
	assert.Equalf(t, http.StatusOK, code, "body: %s", body)
	assert.Contains(t, body, storedTraceMarker)
}

type queryFixture struct {
	dialAddr string
	basePath string
	tenancy  bool
	reader   *tracestoremocks.Reader
	writer   *tracestoremocks.Writer
	deps     *depsmocks.Reader
	metrics  *metricsmocks.Reader
	client   *http.Client
}

func startQueryHTTP(t *testing.T, bind, dialHost, basePath string, withTenancy bool) *queryFixture {
	t.Helper()
	reader := &tracestoremocks.Reader{}
	writer := &tracestoremocks.Writer{}
	deps := &depsmocks.Reader{}
	metricsReader := &metricsmocks.Reader{}
	installStoredTelemetry(reader, writer, deps, metricsReader)

	opts := &QueryOptions{
		BasePath: basePath,
		HTTP: confighttp.ServerConfig{NetAddr: confignet.AddrConfig{
			Endpoint:  bind,
			Transport: confignet.TransportTypeTCP,
		}},
		GRPC: configgrpc.ServerConfig{NetAddr: confignet.AddrConfig{
			Endpoint:  "127.0.0.1:0",
			Transport: confignet.TransportTypeTCP,
		}},
		AI: configoptional.Some(jaegerai.Config{
			MCP:                configoptional.Some(jaegerai.MCPConfig{}),
			MaxRequestBodySize: 1 << 20,
		}),
	}
	var tenancyMgr *tenancy.Manager
	if withTenancy {
		opts.Tenancy = tenancy.Options{Enabled: true}
		tenancyMgr = tenancy.NewManager(&opts.Tenancy)
	} else {
		tenancyMgr = noopTenancyMgr()
	}
	qs := querysvc.NewQueryService(reader, deps, querysvc.QueryServiceOptions{ArchiveTraceWriter: writer})
	telset := initTelSet(zaptest.NewLogger(t), nooptrace.NewTracerProvider())
	server, err := NewServer(context.Background(), qs, metricsReader, opts, noBackendCaps, tenancyMgr, telset)
	require.NoError(t, err)
	require.NoError(t, server.Start(context.Background()))
	t.Cleanup(func() {
		require.NoError(t, server.Close())
	})

	dialAddr := server.HTTPAddr()
	if dialHost != "" {
		_, port, err := net.SplitHostPort(dialAddr)
		require.NoError(t, err)
		dialAddr = net.JoinHostPort(dialHost, port)
	}
	return &queryFixture{
		dialAddr: dialAddr,
		basePath: basePath,
		tenancy:  withTenancy,
		reader:   reader,
		writer:   writer,
		deps:     deps,
		metrics:  metricsReader,
		client: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		},
	}
}

func installStoredTelemetry(reader *tracestoremocks.Reader, writer *tracestoremocks.Writer, deps *depsmocks.Reader, metricsReader *metricsmocks.Reader) {
	trace := storedTrace(storedTraceMarker)
	spans := storedTrace(storedSpanMarker)
	reader.On("GetServices", mock.Anything).Return([]string{storedServiceMarker}, nil).Maybe()
	reader.On("GetOperations", mock.Anything, mock.Anything).
		Return([]tracestore.Operation{{Name: storedOperationMarker, SpanKind: "server"}}, nil).Maybe()
	reader.On("GetTraces", mock.Anything, mock.Anything).
		Return(iter.Seq2[[]ptrace.Traces, error](func(yield func([]ptrace.Traces, error) bool) {
			yield([]ptrace.Traces{trace}, nil)
		})).Maybe()
	reader.On("FindTraces", mock.Anything, mock.Anything).
		Return(iter.Seq2[[]ptrace.Traces, error](func(yield func([]ptrace.Traces, error) bool) {
			yield([]ptrace.Traces{trace}, nil)
		})).Maybe()
	reader.On("FindTraceSummaries", mock.Anything, mock.Anything).
		Return(iter.Seq2[tracestore.PageChunk[[]tracestore.TraceSummary], error](func(yield func(tracestore.PageChunk[[]tracestore.TraceSummary], error) bool) {
			yield(tracestore.PageChunk[[]tracestore.TraceSummary]{
				Results: []tracestore.TraceSummary{{RootServiceName: storedSummaryMarker, RootOperationName: storedSummaryMarker}},
			}, nil)
		})).Maybe()
	reader.On("FindSpans", mock.Anything, mock.Anything).
		Return(iter.Seq2[tracestore.PageChunk[ptrace.Traces], error](func(yield func(tracestore.PageChunk[ptrace.Traces], error) bool) {
			yield(tracestore.PageChunk[ptrace.Traces]{Results: spans}, nil)
		})).Maybe()
	reader.On("SearchCapabilities", mock.Anything).
		Return(tracestore.SearchCapabilities{SpanSearch: true}, nil).Maybe()
	writer.On("WriteTraces", mock.Anything, mock.Anything).Return(nil).Maybe()
	deps.On("GetDependencies", mock.Anything, mock.Anything).
		Return([]model.DependencyLink{{Parent: storedDependencyMarker, Child: "downstream", CallCount: 1}}, nil).Maybe()
	family := &metrics.MetricFamily{Name: storedMetricMarker}
	metricsReader.On("GetCallRates", mock.Anything, mock.Anything).Return(family, nil).Maybe()
	metricsReader.On("GetErrorRates", mock.Anything, mock.Anything).Return(family, nil).Maybe()
	metricsReader.On("GetLatencies", mock.Anything, mock.Anything).Return(family, nil).Maybe()
	metricsReader.On("GetDimensions", mock.Anything).
		Return([]metricstore.Dimension{{Name: storedDimensionMarker}}, nil).Maybe()
}

func storedTrace(marker string) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", marker)
	span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.SetName(marker)
	span.Attributes().PutStr("enduser.id", marker)
	return td
}

func (f *queryFixture) rebindingHost() string {
	_, port, err := net.SplitHostPort(f.dialAddr)
	if err != nil {
		return rebindingName
	}
	return net.JoinHostPort(rebindingName, port)
}

func (f *queryFixture) do(method, path, host string) (int, string) {
	req, err := http.NewRequest(method, "http://"+f.dialAddr+f.basePath+path, http.NoBody)
	if err != nil {
		return 0, err.Error()
	}
	req.Host = host
	if f.tenancy {
		req.Header.Set("x-tenant", "acme")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err.Error()
	}
	return resp.StatusCode, string(body)
}

func (f *queryFixture) resetCalls() {
	f.reader.Calls = nil
	f.writer.Calls = nil
	f.deps.Calls = nil
	f.metrics.Calls = nil
}

func (f *queryFixture) storageCalls() []string {
	var called []string
	for _, c := range f.reader.Calls {
		called = append(called, "trace."+c.Method)
	}
	for _, c := range f.writer.Calls {
		called = append(called, "archive."+c.Method)
	}
	for _, c := range f.deps.Calls {
		called = append(called, "dependencies."+c.Method)
	}
	for _, c := range f.metrics.Calls {
		called = append(called, "metrics."+c.Method)
	}
	return called
}

func (f *queryFixture) reject(t *testing.T, method, path, host, marker string) {
	t.Helper()
	f.resetCalls()
	code, body := f.do(method, path, host)
	assert.Equalf(t, http.StatusForbidden, code, "%s %s Host %s body: %s", method, path, host, body)
	if marker != "" {
		assert.NotContains(t, body, marker)
	}
	assert.Emptyf(t, f.storageCalls(), "storage ran before the response for %s %s", method, path)
}

func rejectStoredRoutes(t *testing.T, fx *queryFixture, host string) {
	t.Helper()
	for _, route := range storedRoutes {
		t.Run(route.name, func(t *testing.T) {
			fx.reject(t, route.method, route.path, host, route.marker)
		})
	}
}

func (f *queryFixture) allowStoredRoutes(t *testing.T, host string) {
	t.Helper()
	for _, route := range storedRoutes {
		t.Run(route.name, func(t *testing.T) {
			code, body := f.do(route.method, route.path, host)
			assert.Equalf(t, http.StatusOK, code, "%s %s Host %s body: %s", route.method, route.path, host, body)
			if route.marker != "" {
				assert.Contains(t, body, route.marker)
			}
		})
	}
}

func (f *queryFixture) requireMCPRejects(t *testing.T) {
	t.Helper()
	const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`
	req, err := http.NewRequest(http.MethodPost, "http://"+f.dialAddr+f.basePath+"/api/ai/mcp/", strings.NewReader(initialize))
	require.NoError(t, err)
	req.Host = f.rebindingHost()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := f.client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equalf(t, http.StatusForbidden, resp.StatusCode,
		"MCP guard did not see a loopback connection; the query API assertions would not be testing that path. body: %s", body)
	require.Contains(t, string(body), "invalid Host header")
}

func pinRebindingHost(t *testing.T) {
	t.Helper()
	const path = "/etc/hosts"
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	if !strings.Contains(string(original), rebindingName) {
		updated := append(append([]byte{}, original...), []byte("\n127.0.0.1 "+rebindingName+"\n::1 "+rebindingName+"\n")...)
		writeHosts(t, path, updated)
		t.Cleanup(func() {
			writeHosts(t, path, original)
		})
	}
	ips, err := net.LookupHost(rebindingName)
	require.NoError(t, err)
	require.Contains(t, ips, "127.0.0.1", "the rebinding name must resolve to loopback or a lookup-based Host check is not exercised")
}

func writeHosts(t *testing.T, path string, content []byte) {
	t.Helper()
	err := os.WriteFile(path, content, 0o644)
	if err == nil {
		return
	}
	cmd := exec.Command("sudo", "-n", "tee", path)
	cmd.Stdin = bytes.NewReader(content)
	out, serr := cmd.CombinedOutput()
	require.NoErrorf(t, serr, "writing %s failed (%v); sudo -n tee: %s", path, err, out)
}

func firstNonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	require.NoError(t, err)
	for _, ifi := range ifaces {
		addrs, err := ifi.Addrs()
		require.NoError(t, err)
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || ip.IsLoopback() {
				continue
			}
			return ip.String()
		}
	}
	t.Fatal("no non-loopback IPv4 address")
	return ""
}
