// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package jaegerai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/gorilla/websocket"
	basicauthextension "github.com/open-telemetry/opentelemetry-collector-contrib/extension/basicauthextension"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/config/configauth"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/extension"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/querysvc"
	depsmocks "github.com/jaegertracing/jaeger/internal/storage/v2/api/depstore/mocks"
	tracestoremocks "github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore/mocks"
	"github.com/jaegertracing/jaeger/internal/telemetry"
	"github.com/jaegertracing/jaeger/internal/tenancy"
)

// attackerPrompt is the user-message text a cross-site page would place in the
// chat body. The assertions compare it to the ACP prompt the sidecar received.
const attackerPrompt = "attacker-controlled prompt"

// placeholderUser and placeholderSecret are obvious stand-ins for the htpasswd
// pair a basicauth server authenticator would be configured with. They are not
// a real credential.
const (
	placeholderUser   = "placeholder-user"
	placeholderSecret = "placeholder-secret"
)

// chatBody is the AG-UI JSON a no-cors fetch sends as a text/plain body.
func chatBody(prompt string) string {
	return `{"threadId":"t","runId":"r","messages":[{"id":"1","role":"user","content":"` + prompt + `"}]}`
}

// countingAgent is the sidecar the gateway dials. dials increments on every
// WebSocket upgrade, before any ACP message is read, so a handler that connects
// and then refuses still fails the test.
type countingAgent struct {
	mockACPAgent
	dials atomic.Int32
}

func startCountingAgent(t *testing.T) (*countingAgent, string) {
	t.Helper()
	agent := &countingAgent{}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent.dials.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		adapter := NewWsAdapter(conn, zap.NewNop())
		asc := acp.NewAgentSideConnection(agent, adapter, adapter)
		agent.mu.Lock()
		agent.asc = asc
		agent.mu.Unlock()
		<-asc.Done()
	}))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Config.Shutdown(ctx)
		server.Close()
	})
	return agent, "ws" + strings.TrimPrefix(server.URL, "http")
}

// allInOneAIConfig is the ai value DefaultQueryOptions seeds, after a partial
// "mcp: {}" block is unmarshaled onto it the way the collector unmarshals the
// embedded all-in-one config. AgentURL stays at its default; the test then
// points that URL at the in-process sidecar.
func allInOneAIConfig(t *testing.T) Config {
	t.Helper()
	seeded := configoptional.Default(validConfig())
	block := confmap.NewFromStringMap(map[string]any{
		"mcp": map[string]any{},
	})
	require.NoError(t, block.Unmarshal(&seeded))
	got := seeded.Get()
	require.NotNil(t, got)
	require.Equal(t, DefaultAgentURL, got.AgentURL)
	require.True(t, got.MCP.HasValue())
	require.Equal(t, DefaultMaxRequestBodySize, got.MaxRequestBodySize)
	return *got
}

// chatHarness is the query HTTP stack in front of the chat route: Register
// mounts it, and confighttp.ServerConfig.ToServer wraps it with the same
// decompressor, body limit, server authenticator, and otelhttp handler that
// createHTTPServer applies. httpAuth, when set, is that server authenticator.
type chatHarness struct {
	agent  *countingAgent
	server *httptest.Server
	client *http.Client
}

func newChatHarness(t *testing.T, mutate func(*Config), httpAuth *component.ID, extensions map[component.ID]component.Component) *chatHarness {
	t.Helper()
	return newChatHarnessAt(t, "", mutate, httpAuth, extensions)
}

func newChatHarnessAt(t *testing.T, basePath string, mutate func(*Config), httpAuth *component.ID, extensions map[component.ID]component.Component) *chatHarness {
	t.Helper()
	agent, wsURL := startCountingAgent(t)
	cfg := allInOneAIConfig(t)
	cfg.AgentURL = wsURL
	if mutate != nil {
		mutate(&cfg)
	}

	telset := telemetry.NoopSettings()
	telset.Logger = zaptest.NewLogger(t)
	if extensions != nil {
		telset.Host = &extensionsHost{extensions: extensions}
	}
	traceReader := &tracestoremocks.Reader{}
	querySvc := querysvc.NewQueryService(traceReader, &depsmocks.Reader{}, querysvc.QueryServiceOptions{})

	mux := http.NewServeMux()
	routes, err := Register(context.Background(), mux, RegisterParams{
		Config:       &cfg,
		BasePath:     basePath,
		QueryService: querySvc,
		Tenancy:      tenancy.NewManager(&tenancy.Options{}),
		Telemetry:    telset,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, routes.Close()) })

	httpCfg := confighttp.ServerConfig{}
	if httpAuth != nil {
		httpCfg.Auth = configoptional.Some(confighttp.AuthConfig{
			Config: configauth.Config{AuthenticatorID: *httpAuth},
		})
	}
	hs, err := httpCfg.ToServer(context.Background(), extensions, telset.ToOtelComponent(), mux)
	require.NoError(t, err)
	ts := httptest.NewServer(hs.Handler)
	t.Cleanup(ts.Close)
	return &chatHarness{agent: agent, server: ts, client: &http.Client{Timeout: 5 * time.Second}}
}

func (h *chatHarness) post(t *testing.T, path, contentType, prompt string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(chatBody(prompt)))
	require.NoError(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for name, values := range header {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := h.client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	})
	return resp
}

func (h *chatHarness) promptText() string {
	_, _, prompt := h.agent.snapshot()
	if prompt == nil || len(prompt.Prompt) == 0 || prompt.Prompt[0].Text == nil {
		return ""
	}
	return prompt.Prompt[0].Text.Text
}

// requireRefused asserts the sidecar was not dialed and the response is not a
// chat stream. A check that runs after DialWsAdapter fails this, because dials
// increments on the upgrade itself.
func (h *chatHarness) requireRefused(t *testing.T, resp *http.Response) {
	t.Helper()
	// Drain before asserting. The handler commits the SSE status inside the
	// turn, so Client.Do returns while Prompt may still be running.
	_, _ = io.Copy(io.Discard, resp.Body)
	require.Zero(t, h.agent.dials.Load(), "sidecar was dialed before the request was refused; prompt=%q status=%d", h.promptText(), resp.StatusCode)
	require.Empty(t, h.promptText(), "agent received a prompt")
	require.NotEqual(t, "text/event-stream", resp.Header.Get("Content-Type"))
	require.GreaterOrEqual(t, resp.StatusCode, 400)
	require.Less(t, resp.StatusCode, 500)
}

func (h *chatHarness) requireDispatched(t *testing.T, resp *http.Response, prompt string) {
	t.Helper()
	require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", readBody(t, resp))
	require.Equal(t, int32(1), h.agent.dials.Load())
	require.Equal(t, prompt, h.promptText())
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

func crossSiteHeaders() http.Header {
	return http.Header{
		"Origin":         []string{"https://attacker.example"},
		"Sec-Fetch-Site": []string{"cross-site"},
		"Sec-Fetch-Mode": []string{"no-cors"},
		"Sec-Fetch-Dest": []string{"empty"},
	}
}

func startBasicAuth(t *testing.T) (component.ID, map[component.ID]component.Component) {
	t.Helper()
	factory := basicauthextension.NewFactory()
	cfg := factory.CreateDefaultConfig().(*basicauthextension.Config)
	cfg.Htpasswd = &basicauthextension.HtpasswdSettings{
		Inline: placeholderUser + ":" + placeholderSecret + "\n",
	}
	authID := component.MustNewID("basicauth")
	ext, err := factory.Create(context.Background(), extension.Settings{
		ID:                authID,
		TelemetrySettings: componenttest.NewNopTelemetrySettings(),
	}, cfg)
	require.NoError(t, err)
	require.NoError(t, ext.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, ext.Shutdown(context.Background())) })
	_, ok := ext.(interface {
		Authenticate(context.Context, map[string][]string) (context.Context, error)
	})
	require.True(t, ok, "basicauth extension must be a server authenticator")
	return authID, map[component.ID]component.Component{authID: ext}
}

// TestAIChatCrossSiteRequestDoesNotDispatchPrompt is the spec for the missing
// origin and content-type checks on POST /api/ai/chat. It fails while a
// browser-shaped request is decoded and forwarded to the ACP sidecar.
func TestAIChatCrossSiteRequestDoesNotDispatchPrompt(t *testing.T) {
	const plain = "text/plain;charset=UTF-8"
	const jsonType = "application/json"

	t.Run("no-cors text/plain from another origin", func(t *testing.T) {
		h := newChatHarness(t, nil, nil, nil)
		resp := h.post(t, routeChat, plain, attackerPrompt, crossSiteHeaders())
		h.requireRefused(t, resp)
	})

	t.Run("application/json from another origin", func(t *testing.T) {
		h := newChatHarness(t, nil, nil, nil)
		resp := h.post(t, routeChat, jsonType, attackerPrompt, crossSiteHeaders())
		h.requireRefused(t, resp)
	})

	t.Run("origin mismatch without sec-fetch-site", func(t *testing.T) {
		h := newChatHarness(t, nil, nil, nil)
		resp := h.post(t, routeChat, jsonType, attackerPrompt, http.Header{
			"Origin": []string{"https://attacker.example"},
		})
		h.requireRefused(t, resp)
	})

	t.Run("same-site fetch metadata", func(t *testing.T) {
		h := newChatHarness(t, nil, nil, nil)
		resp := h.post(t, routeChat, jsonType, attackerPrompt, http.Header{
			"Origin":         []string{"http://localhost:9999"},
			"Sec-Fetch-Site": []string{"same-site"},
		})
		h.requireRefused(t, resp)
	})

	t.Run("text/plain without fetch metadata", func(t *testing.T) {
		h := newChatHarness(t, nil, nil, nil)
		resp := h.post(t, routeChat, plain, attackerPrompt, nil)
		h.requireRefused(t, resp)
	})

	t.Run("same-origin application/json still dispatches", func(t *testing.T) {
		h := newChatHarness(t, nil, nil, nil)
		const prompt = "operator prompt"
		resp := h.post(t, routeChat, jsonType, prompt, http.Header{
			"Origin":         []string{h.server.URL},
			"Sec-Fetch-Site": []string{"same-origin"},
		})
		h.requireDispatched(t, resp, prompt)
	})

	t.Run("non-browser application/json still dispatches", func(t *testing.T) {
		h := newChatHarness(t, nil, nil, nil)
		const prompt = "operator prompt"
		resp := h.post(t, routeChat, jsonType, prompt, nil)
		h.requireDispatched(t, resp, prompt)
	})

	t.Run("base path", func(t *testing.T) {
		h := newChatHarnessAt(t, "/jaeger", nil, nil, nil)
		resp := h.post(t, "/jaeger"+routeChat, plain, attackerPrompt, crossSiteHeaders())
		h.requireRefused(t, resp)
	})

	t.Run("ai.auth basicauth does not stop a cross-site prompt", func(t *testing.T) {
		authID, extensions := startBasicAuth(t)
		withAuth := func(cfg *Config) {
			cfg.Auth = configoptional.Some(confighttp.AuthConfig{
				Config: configauth.Config{AuthenticatorID: authID},
			})
		}
		admitted := newChatHarness(t, withAuth, nil, extensions)
		const prompt = "operator prompt"
		okResp := postBasic(t, admitted, routeChat, jsonType, prompt, http.Header{
			"Origin":         []string{admitted.server.URL},
			"Sec-Fetch-Site": []string{"same-origin"},
		})
		admitted.requireDispatched(t, okResp, prompt)

		anonymous := newChatHarness(t, withAuth, nil, extensions)
		rejected := anonymous.post(t, routeChat, plain, attackerPrompt, crossSiteHeaders())
		require.Equal(t, http.StatusUnauthorized, rejected.StatusCode)
		require.Zero(t, anonymous.agent.dials.Load())

		cross := newChatHarness(t, withAuth, nil, extensions)
		resp := postBasic(t, cross, routeChat, plain, attackerPrompt, crossSiteHeaders())
		cross.requireRefused(t, resp)
	})

	t.Run("http server basicauth does not stop a cross-site prompt", func(t *testing.T) {
		authID, extensions := startBasicAuth(t)
		admitted := newChatHarness(t, nil, &authID, extensions)
		const prompt = "operator prompt"
		okResp := postBasic(t, admitted, routeChat, jsonType, prompt, http.Header{
			"Origin":         []string{admitted.server.URL},
			"Sec-Fetch-Site": []string{"same-origin"},
		})
		admitted.requireDispatched(t, okResp, prompt)

		cross := newChatHarness(t, nil, &authID, extensions)
		resp := postBasic(t, cross, routeChat, plain, attackerPrompt, crossSiteHeaders())
		cross.requireRefused(t, resp)
	})
}

func postBasic(t *testing.T, h *chatHarness, path, contentType, prompt string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(chatBody(prompt)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", contentType)
	req.SetBasicAuth(placeholderUser, placeholderSecret)
	for name, values := range header {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := h.client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	})
	return resp
}
