// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package internal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/extension"
	"go.uber.org/zap/zaptest"

	"github.com/jaegertracing/jaeger/cmd/jaeger/internal"
	expvarext "github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/expvar"
	"github.com/jaegertracing/jaeger/internal/testutils"
)

// These tests are the spec for the default all-in-one debug listeners.
// They fail while expvar and zpages are reachable without credentials on a
// non-loopback address, and pass once that disclosure is closed.

const (
	expvarPort = 27777
	zpagesPort = 27778
	queryPort  = 16686
	otlpPort   = 4318

	// Distinctive span attribute posted to the default OTLP receiver.
	// It is a fixed canary, not a credential.
	customerAttributeCanary = "customer-trace-attribute-canary"
	customerSpanName        = "customer-canary-span"
)

var zpagesPaths = []string{
	"/debug/tracez",
	"/debug/pipelinez",
	"/debug/servicez",
	"/debug/extensionz",
	"/debug/featurez",
}

var spanNamePattern = regexp.MustCompile(`zspanname=([^&"<>\s]+)`)

func TestDefaultAllInOneDebugPortsAreNotPublic(t *testing.T) {
	cancel, done := startAllInOne(t, nil)
	defer stopAllInOne(t, cancel, done)

	postCustomerSpan(t)

	for _, ip := range nonLoopbackIPv4s(t) {
		body := assertUnauthenticatedDebugClosed(t, fmt.Sprintf("http://%s:%d/", ip, expvarPort))
		if strings.Contains(body, `"cmdline"`) && strings.Contains(body, `"memstats"`) {
			t.Errorf("expvar on %s disclosed process cmdline and memory statistics", ip)
		}
		for _, path := range zpagesPaths {
			page := assertUnauthenticatedDebugClosed(t, fmt.Sprintf("http://%s:%d%s", ip, zpagesPort, path))
			if path == "/debug/tracez" {
				assertTracezHidesCustomerSpan(t, ip, page)
			}
		}
	}
}

func TestQueryAuthenticatorDoesNotCoverDebugPorts(t *testing.T) {
	overlay := writeQueryAuthOverlay(t)
	cancel, done := startAllInOne(t, []string{
		"--config=file:all-in-one.yaml",
		"--config=file:" + overlay,
	})
	defer stopAllInOne(t, cancel, done)

	queryStatus, queryBody := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/api/services", queryPort))
	require.Equalf(t, http.StatusUnauthorized, queryStatus,
		"query HTTP authenticator was not enforced, body prefix: %s", truncate(queryBody))

	for _, ip := range nonLoopbackIPv4s(t) {
		assertUnauthenticatedDebugClosed(t, fmt.Sprintf("http://%s:%d/", ip, expvarPort))
		for _, path := range zpagesPaths {
			assertUnauthenticatedDebugClosed(t, fmt.Sprintf("http://%s:%d%s", ip, zpagesPort, path))
		}
	}
}

func TestExpvarFactoryDefaultIsNotReachableOffLoopback(t *testing.T) {
	factory := expvarext.NewFactory()
	cfg := factory.CreateDefaultConfig()
	ext, err := factory.Create(context.Background(), extension.Settings{
		ID: expvarext.ID,
		TelemetrySettings: component.TelemetrySettings{
			Logger: zaptest.NewLogger(t),
		},
	}, cfg)
	require.NoError(t, err)

	require.NoError(t, ext.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() {
		require.NoError(t, ext.Shutdown(context.Background()))
	})

	for _, ip := range nonLoopbackIPv4s(t) {
		assertUnauthenticatedDebugClosed(t, fmt.Sprintf("http://%s:%d/", ip, expvarPort))
	}
}

// startAllInOne runs the jaeger command the way the image entrypoint does
// when args is nil: no --config flag, so the embedded all-in-one file is
// loaded, and JAEGER_LISTEN_HOST is 0.0.0.0 as in cmd/jaeger/Dockerfile.
func startAllInOne(t *testing.T, args []string) (cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	t.Setenv("JAEGER_LISTEN_HOST", "0.0.0.0")
	t.Setenv("OTEL_TRACES_SAMPLER", "always_off")

	factories, err := internal.Components()
	require.NoError(t, err)
	command := internal.NewCommand(factories)
	ctx, cancel := context.WithCancel(context.Background())
	command.SetContext(ctx)
	if args == nil {
		args = []string{}
	}
	command.SetArgs(args)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)

	errCh := make(chan error, 1)
	go func() {
		errCh <- command.Execute()
	}()

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			t.Fatalf("all-in-one exited before the query port was open: %v", err)
		default:
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", queryPort), 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return cancel, errCh
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	t.Fatalf("all-in-one did not open the query port")
	return cancel, errCh
}

func stopAllInOne(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("all-in-one exit: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Errorf("all-in-one did not exit after cancel")
	}
	testutils.VerifyGoLeaksOnce(t)
}

func writeQueryAuthOverlay(t *testing.T) string {
	t.Helper()
	// placeholder-user / placeholder-password are obvious placeholders, not a real credential.
	const overlay = `
extensions:
  basicauth/query:
    htpasswd:
      inline: "placeholder-user:placeholder-password"
  jaeger_query:
    http:
      auth:
        authenticator: basicauth/query
service:
  extensions:
    - basicauth/query
    - jaeger_storage
    - jaeger_query
    - remote_sampling
    - healthcheckv2
    - expvar
    - zpages
`
	path := filepath.Join(t.TempDir(), "query-auth.yaml")
	require.NoError(t, os.WriteFile(path, []byte(overlay), 0o600))
	return path
}

func postCustomerSpan(t *testing.T) {
	t.Helper()
	now := time.Now()
	payload := fmt.Sprintf(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"canary-service"}}]},"scopeSpans":[{"spans":[{"traceId":"0123456789abcdef0123456789abcdef","spanId":"0123456789abcdef","name":%q,"startTimeUnixNano":"%d","endTimeUnixNano":"%d","attributes":[{"key":"customer.attribute","value":{"stringValue":%q}}]}]}]}]}`,
		customerSpanName, now.UnixNano(), now.Add(time.Millisecond).UnixNano(), customerAttributeCanary)
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/traces", otlpPort), strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := debugHTTP.Do(req)
	if err != nil {
		t.Logf("OTLP export failed: %v", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Logf("OTLP export status %d: %s", resp.StatusCode, truncate(string(body)))
	}
}

func assertTracezHidesCustomerSpan(t *testing.T, ip, summary string) {
	t.Helper()
	if summary == "" {
		return
	}
	if strings.Contains(summary, customerAttributeCanary) || strings.Contains(summary, customerSpanName) {
		t.Errorf("tracez summary on %s disclosed the ingested customer span", ip)
	}
	seen := map[string]struct{}{}
	for _, raw := range spanNamePattern.FindAllStringSubmatch(summary, -1) {
		name, err := url.QueryUnescape(raw[1])
		require.NoError(t, err)
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		if len(seen) > 40 {
			break
		}
		for _, detail := range tracezDetailURLs(ip, name) {
			page := assertUnauthenticatedDebugClosed(t, detail)
			if strings.Contains(page, customerAttributeCanary) || strings.Contains(page, customerSpanName) {
				t.Errorf("tracez detail %s disclosed the ingested customer span", detail)
			}
		}
	}
}

func tracezDetailURLs(ip, name string) []string {
	q := url.Values{}
	q.Set("zspanname", name)
	base := fmt.Sprintf("http://%s:%d/debug/tracez", ip, zpagesPort)
	var urls []string
	q.Set("ztype", "0")
	urls = append(urls, base+"?"+q.Encode())
	q.Set("ztype", "2")
	urls = append(urls, base+"?"+q.Encode())
	q.Set("ztype", "1")
	for bucket := 0; bucket <= 8; bucket++ {
		q.Set("zlatencybucket", fmt.Sprintf("%d", bucket))
		urls = append(urls, base+"?"+q.Encode())
	}
	return urls
}

func assertUnauthenticatedDebugClosed(t *testing.T, rawURL string) string {
	t.Helper()
	status, body := httpGet(t, rawURL)
	if status == 0 {
		return ""
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		if disclosed(body) {
			t.Errorf("%s returned %d but the body still disclosed debug data: %s", rawURL, status, truncate(body))
		}
		return body
	}
	t.Errorf("unauthenticated GET %s returned %d cmdline=%t memstats=%t pipeline=%t service=%t tracez=%t customerSpan=%t",
		rawURL, status,
		strings.Contains(body, `"cmdline"`),
		strings.Contains(body, `"memstats"`),
		strings.Contains(body, "builtPipelines"),
		strings.Contains(body, "Runtime Info"),
		strings.Contains(body, "Trace Spans"),
		strings.Contains(body, customerAttributeCanary) || strings.Contains(body, customerSpanName),
	)
	return body
}

func disclosed(body string) bool {
	return strings.Contains(body, `"cmdline"`) ||
		strings.Contains(body, `"memstats"`) ||
		strings.Contains(body, "builtPipelines") ||
		strings.Contains(body, "Runtime Info") ||
		strings.Contains(body, "Trace Spans") ||
		strings.Contains(body, customerAttributeCanary) ||
		strings.Contains(body, customerSpanName)
}

func httpGet(t *testing.T, rawURL string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	require.NoError(t, err)
	resp, err := debugHTTP.Do(req)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	buf := bytes.NewBuffer(nil)
	_, err = io.Copy(buf, io.LimitReader(resp.Body, 1<<20))
	require.NoError(t, err)
	return resp.StatusCode, buf.String()
}

func nonLoopbackIPv4s(t *testing.T) []string {
	t.Helper()
	ifaces, err := net.Interfaces()
	require.NoError(t, err)
	var ips []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		require.NoError(t, err)
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ipv4 := ipnet.IP.To4()
			if ipv4 == nil || ipv4.IsLoopback() {
				continue
			}
			ips = append(ips, ipv4.String())
		}
	}
	require.NotEmpty(t, ips, "this environment has no non-loopback IPv4 address to probe")
	return ips
}

func truncate(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 240 {
		return s[:240]
	}
	return s
}

var debugHTTP = &http.Client{
	Timeout: 3 * time.Second,
	Transport: &http.Transport{
		Proxy: nil,
	},
}
