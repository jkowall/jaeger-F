// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package queryinterceptorexample_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/confmap/provider/envprovider"
	"go.opentelemetry.io/collector/confmap/provider/fileprovider"
	"go.opentelemetry.io/collector/otelcol"
)

// sensitiveLiteral is a placeholder standing in for a span attribute a query
// interceptor is configured to redact (a prompt, an email, or similar). It is
// not a credential.
const sensitiveLiteral = "redact-me-placeholder"

const (
	selfTraceID = "00000000000000000000000000abc124"
	selfSpanID  = "0000000000abc124"
)

// TestSelfTraceDoesNotRevealRedactedToolData boots the query server with the
// reference per-key redaction interceptor and the MCP endpoint, then drives
// the privileged MCP tools that return span attributes or take a search
// statement. A non-privileged caller must be able to read the resulting
// self-traces (the calls are still traced) and must not see sensitiveLiteral
// in them.
//
// The tools that share the middleware but do not carry a redacted attribute
// or a search literal — get_services, get_span_names, get_trace_topology,
// get_critical_path, get_service_dependencies, and read_skill — are not called.
// The chat gateway is not called either: it records the agent name and the
// session id, not the tool payload. The assistant path leaks through the same
// tools/call middleware once the assistant invokes a tool.
func TestSelfTraceDoesNotRevealRedactedToolData(t *testing.T) {
	otlpGRPC := freePort(t)
	otlpHTTP := freePort(t)
	queryHTTP := freePort(t)

	// jtracer exports to the OTLP endpoint from the environment, insecure unless
	// the endpoint is https. Point it at this process's receiver and sample
	// every span. The batch processor's default delay is 5s; shorten it so the
	// self-trace is stored within the poll below.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", fmt.Sprintf("http://127.0.0.1:%d", otlpGRPC))
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("OTEL_TRACES_SAMPLER", "parentbased_always_on")
	t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "100")
	t.Setenv("OTLP_GRPC_ENDPOINT", fmt.Sprintf("127.0.0.1:%d", otlpGRPC))
	t.Setenv("OTLP_HTTP_ENDPOINT", fmt.Sprintf("127.0.0.1:%d", otlpHTTP))
	t.Setenv("JAEGER_QUERY_HTTP_ENDPOINT", fmt.Sprintf("127.0.0.1:%d", queryHTTP))
	t.Setenv("JAEGER_QUERY_GRPC_ENDPOINT", fmt.Sprintf("127.0.0.1:%d", freePort(t)))

	col, err := otelcol.NewCollector(otelcol.CollectorSettings{
		BuildInfo: component.NewDefaultBuildInfo(),
		Factories: func() (otelcol.Factories, error) { return testFactories(t), nil },
		ConfigProviderSettings: otelcol.ConfigProviderSettings{
			ResolverSettings: confmap.ResolverSettings{
				URIs: []string{"file:config-mcp-selftrace.yaml"},
				ProviderFactories: []confmap.ProviderFactory{
					fileprovider.NewFactory(),
					envprovider.NewFactory(),
				},
			},
		},
	})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- col.Run(t.Context()) }()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("collector did not shut down in time")
		}
	})
	require.Eventually(t, func() bool { return col.GetState() == otelcol.StateRunning },
		20*time.Second, 50*time.Millisecond, "collector did not reach running state")

	pushSensitiveSpan(t, otlpHTTP)
	queryBase := fmt.Sprintf("http://127.0.0.1:%d", queryHTTP)

	// The interceptor is in force: the same trace is intact for admin and redacted
	// for a viewer, and the redacted view does not still contain the literal.
	require.Eventually(t, func() bool {
		code, body := httpGet(t, queryBase+"/api/v3/traces/"+selfTraceID, "admin")
		return code == http.StatusOK && strings.Contains(body, sensitiveLiteral)
	}, 10*time.Second, 100*time.Millisecond, "privileged caller cannot read the stored span")

	_, viewerBody := httpGet(t, queryBase+"/api/v3/traces/"+selfTraceID, "viewer")
	require.Contains(t, viewerBody, "REDACTED", "non-privileged caller must see the configured attributes redacted")
	require.NotContains(t, viewerBody, sensitiveLiteral, "per-key redaction must hide the literal on the original span")

	session := connectMCP(t, queryBase+"/api/ai/mcp/", "admin")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// get_span_details and get_trace_errors return the span attributes, so a
	// privileged caller sees the literal in the tool result. find_traces and
	// find_spans return summaries without attributes; the literal is in the
	// SQL statement, which is the tool argument.
	calls := []struct {
		name      string
		args      map[string]any
		resultHas bool
	}{
		{
			name: "get_span_details",
			args: map[string]any{
				"trace_id": selfTraceID,
				"span_ids": []string{selfSpanID},
			},
			resultHas: true,
		},
		{
			name:      "get_trace_errors",
			args:      map[string]any{"trace_id": selfTraceID},
			resultHas: true,
		},
		{
			name: "find_traces",
			args: map[string]any{"sql": findTracesSQL(sensitiveLiteral)},
		},
		{
			name: "find_spans",
			args: map[string]any{"sql": findSpansSQL(sensitiveLiteral)},
		},
	}
	for _, call := range calls {
		result, callErr := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      call.name,
			Arguments: call.args,
		})
		require.NoError(t, callErr, call.name)
		require.False(t, result.IsError, "%s returned an error result: %s", call.name, toolResultText(t, result))
		if call.resultHas {
			require.Contains(t, toolResultText(t, result), sensitiveLiteral,
				"%s must have shown the privileged caller the literal, or the self-trace has nothing to leak", call.name)
		}
	}

	// Self-traces are stored under service jaeger and are readable by the viewer.
	// The tools/call spans have to be there (the calls stay traced). The literal
	// must not be, in any attribute, including one that embeds another span's
	// attributes inside a string.
	var leaked string
	require.Eventually(t, func() bool {
		body := searchService(t, queryBase, "jaeger", "viewer")
		if !strings.Contains(body, "tools/call get_span_details") ||
			!strings.Contains(body, "tools/call get_trace_errors") ||
			!strings.Contains(body, "tools/call find_traces") ||
			!strings.Contains(body, "tools/call find_spans") {
			return false
		}
		leaked = body
		return true
	}, 20*time.Second, 200*time.Millisecond, "self-traces of the MCP calls were not stored under service jaeger")

	idx := strings.Index(leaked, sensitiveLiteral)
	if idx >= 0 {
		start := max(0, idx-80)
		end := min(len(leaked), idx+len(sensitiveLiteral)+80)
		t.Fatalf("non-privileged read of service jaeger contains %q near %q", sensitiveLiteral, leaked[start:end])
	}
}

func findTracesSQL(literal string) string {
	return "SELECT DISTINCT trace_id FROM spans s " +
		"WHERE between(s.start_time, now() - '1h', now()) " +
		"AND s.attributes['prompt'] = '" + literal + "'"
}

func findSpansSQL(literal string) string {
	return "SELECT * FROM spans s " +
		"WHERE between(s.start_time, now() - '1h', now()) " +
		"AND s.attributes['prompt'] = '" + literal + "'"
}

func pushSensitiveSpan(t *testing.T, otlpHTTPPort int) {
	t.Helper()
	nowNano := time.Now().UnixNano()
	payload := fmt.Sprintf(`{"resourceSpans":[{"resource":{"attributes":[`+
		`{"key":"service.name","value":{"stringValue":"checkout"}}]},"scopeSpans":[{"spans":[{`+
		`"traceId":"%s","spanId":"%s","name":"llm-call","kind":2,`+
		`"startTimeUnixNano":"%d","endTimeUnixNano":"%d",`+
		`"status":{"code":2,"message":"failed"},"attributes":[`+
		`{"key":"prompt","value":{"stringValue":"%s"}},`+
		`{"key":"llm.response","value":{"stringValue":"%s"}},`+
		`{"key":"keep","value":{"stringValue":"visible"}}]}]}]}]}`,
		selfTraceID, selfSpanID, nowNano, nowNano+1000, sensitiveLiteral, sensitiveLiteral)

	endpoint := fmt.Sprintf("http://127.0.0.1:%d/v1/traces", otlpHTTPPort)
	var code int
	require.Eventually(t, func() bool {
		resp, err := testHTTPClient.Post(endpoint, "application/json", strings.NewReader(payload))
		if err != nil {
			return false
		}
		resp.Body.Close()
		code = resp.StatusCode
		return true
	}, 10*time.Second, 100*time.Millisecond, "OTLP endpoint not reachable")
	require.Equal(t, http.StatusOK, code, "OTLP push should succeed")
}

func searchService(t *testing.T, queryBase, service, role string) string {
	t.Helper()
	now := time.Now()
	searchURL := fmt.Sprintf("%s/api/v3/traces?query.serviceName=%s&query.startTimeMin=%s&query.startTimeMax=%s",
		queryBase,
		url.QueryEscape(service),
		url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339Nano)),
		url.QueryEscape(now.Add(time.Hour).Format(time.RFC3339Nano)),
	)
	code, body := httpGet(t, searchURL, role)
	if code != http.StatusOK {
		return ""
	}
	return body
}

func connectMCP(t *testing.T, endpoint, role string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	client := mcp.NewClient(&mcp.Implementation{Name: "redaction-test", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		DisableStandaloneSSE: true,
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
			Transport: roleRoundTripper{
				base: http.DefaultTransport,
				role: role,
			},
		},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { session.Close() })
	return session
}

type roleRoundTripper struct {
	base http.RoundTripper
	role string
}

func (r roleRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header.Set("X-Jaeger-Caller-Role", r.role)
	return r.base.RoundTrip(cloned)
}

func toolResultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	return string(encoded)
}
