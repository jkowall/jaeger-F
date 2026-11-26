// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package jaegerai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configauth"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/jaegertracing/jaeger/internal/headerforwarding"
)

func TestNewHandlerBuildsEndpoints(t *testing.T) {
	h := NewHandler(HandlerParams{Logger: zap.NewNop(), AgentURL: "ws://example", BasePath: "/jaeger", MaxRequestBodySize: 1 << 20})
	require.NotNil(t, h.chat, "NewHandler must build the chat endpoint")
	assert.Equal(t, "/jaeger", h.basePath)
	assert.Nil(t, h.mcp, "the MCP endpoint must be nil when no MCP handler is supplied")
}

func TestRegisterRoutesMountsChatEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		basePath string
		wantChat string
	}{
		{
			name:     "no base path",
			basePath: "",
			wantChat: "/api/ai/chat",
		},
		{
			name:     "single-slash base path is treated as no prefix",
			basePath: "/",
			wantChat: "/api/ai/chat",
		},
		{
			name:     "with base path",
			basePath: "/jaeger",
			wantChat: "/jaeger/api/ai/chat",
		},
		{
			// Operator-supplied trailing slash must be normalized away so we
			// don't register a "/jaeger//api/..." pattern.
			name:     "trailing slash in base path is normalized",
			basePath: "/jaeger/",
			wantChat: "/jaeger/api/ai/chat",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(HandlerParams{Logger: zap.NewNop(), AgentURL: "ws://127.0.0.1:1", BasePath: tc.basePath, MaxRequestBodySize: 1 << 20})
			mux := http.NewServeMux()
			h.RegisterRoutes(mux)

			// Chat endpoint: GET (wrong method) is enough to confirm the
			// route is mounted — the handler returns 405 instead of the
			// mux returning 404.
			req := httptest.NewRequest(http.MethodGet, tc.wantChat, http.NoBody)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusMethodNotAllowed, rr.Code,
				"chat endpoint should be mounted at %s", tc.wantChat)
		})
	}
}

func TestNewHandlerNormalizesTrailingSlash(t *testing.T) {
	h := NewHandler(HandlerParams{Logger: zap.NewNop(), AgentURL: "ws://127.0.0.1:1", BasePath: "/jaeger/", MaxRequestBodySize: 1 << 20})
	assert.Equal(t, "/jaeger", h.basePath, "NewHandler must trim the trailing slash")
}

// TestNewHandlerNormalizesMCPBaseURL pins the trailing-slash handling: config only
// requires an absolute URL, so an operator may legally write trailing slashes, and
// an announced "…//api/ai/mcp/<id>/" is a path the mux never matches. NewHandler
// must trim them so the announced URL has exactly one slash before the route.
func TestNewHandlerNormalizesMCPBaseURL(t *testing.T) {
	for _, base := range []string{
		"http://127.0.0.1:16686",
		"http://127.0.0.1:16686/",
		"http://127.0.0.1:16686//",
	} {
		h := NewHandler(HandlerParams{
			Logger: zap.NewNop(), AgentURL: "ws://x", MaxRequestBodySize: 1,
			MCP: sharedMCPHandler(t), MCPBaseURL: base,
		})
		got := h.chat.announceMCP(t.Context(), httpCaps(true), turnGrant{routeID: "SID", token: "tok"})
		require.Len(t, got, 1)
		assert.Equal(t, "http://127.0.0.1:16686/api/ai/mcp/SID/", got[0].Http.Url,
			"base URL %q must normalize to a single slash", base)
	}
}

func mcpEnabledHandler(t *testing.T, basePath string) *Handler {
	t.Helper()
	return NewHandler(HandlerParams{
		Logger:             zap.NewNop(),
		AgentURL:           "ws://127.0.0.1:1",
		BasePath:           basePath,
		MaxRequestBodySize: 1 << 20,
		MCP:                sharedMCPHandler(t),
	})
}

func TestRegisterRoutesMountsSessionScopedMCPWhenEnabled(t *testing.T) {
	h := mcpEnabledHandler(t, "")
	require.NotNil(t, h.mcp, "MCP endpoint must be built when an MCP handler is supplied")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	grant := registerTurn(h.mcp.turns, testAGUIRun(), nil) // active turn

	turnRequest := func(path, token string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
		if token != "" {
			req.Header.Set(turnTokenHeader, token)
		}
		return req
	}

	t.Run("active turn is served", func(t *testing.T) {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, turnRequest("/api/ai/mcp/"+grant.routeID+"/mcp", grant.token))
		assert.NotEqual(t, http.StatusNotFound, rr.Code, "registered turn must reach the MCP handler")
		assert.NotEqual(t, http.StatusUnauthorized, rr.Code, "the turn's token must be accepted")
	})

	t.Run("active turn is served without a trailing slash", func(t *testing.T) {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, turnRequest("/api/ai/mcp/"+grant.routeID, grant.token))
		assert.NotEqual(t, http.StatusNotFound, rr.Code, "no-slash form must also reach the MCP handler")
		assert.NotEqual(t, http.StatusUnauthorized, rr.Code, "the turn's token must be accepted")
	})

	t.Run("active turn without its token is refused", func(t *testing.T) {
		for _, token := range []string{"", "not-the-token"} {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, turnRequest("/api/ai/mcp/"+grant.routeID+"/mcp", token))
			assert.Equal(t, http.StatusUnauthorized, rr.Code, "token %q", token)
		}
	})

	t.Run("one turn's token does not open another turn", func(t *testing.T) {
		other := registerTurn(h.mcp.turns, testAGUIRun(), nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, turnRequest("/api/ai/mcp/"+grant.routeID+"/mcp", other.token))
		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})

	t.Run("unknown route id is rejected", func(t *testing.T) {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/ai/mcp/ghost/mcp", http.NoBody))
		assert.Equal(t, http.StatusNotFound, rr.Code)
	})
}

// TestRegisterRoutesOmitsMCPEndpointWhenDisabled is the chat-only shape: with no
// ai.mcp block the query server passes no MCP handler, so there are no telemetry
// tools to attach UI tools to and the turn-scoped endpoint is not built at all.
func TestRegisterRoutesOmitsMCPEndpointWhenDisabled(t *testing.T) {
	h := NewHandler(HandlerParams{Logger: zap.NewNop(), AgentURL: "ws://127.0.0.1:1", BasePath: "", MaxRequestBodySize: 1 << 20})
	require.Nil(t, h.mcp, "no MCP handler supplied means no turn-scoped endpoint")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// With MCP disabled the route is never mounted, so any turn URL is a 404
	// regardless of whether a turn is active.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/ai/mcp/any-id/mcp", http.NoBody))
	assert.Equal(t, http.StatusNotFound, rr.Code, "turn-scoped MCP endpoint must not be mounted when disabled")
}

// The ai.auth check arrives as AuthMiddleware and guards the chat endpoint and
// the turn-scoped MCP endpoint alike, with the authenticator the query server
// builds. The agent sends the headers the gateway announced for the turn on every
// tool call, so the check admits a tool call that carries them and refuses one
// that does not, turn token or no turn token, on both forms of the route.
func TestAuthMiddlewareGuardsChatAndTurnScopedEndpoints(t *testing.T) {
	authID := component.MustNewID("testauth")
	host := &extensionsHost{extensions: map[component.ID]component.Component{
		authID: &headerAuthenticator{header: "X-User"},
	}}
	auth, err := newAuthenticator(t.Context(), &confighttp.AuthConfig{Config: configauth.Config{AuthenticatorID: authID}}, host)
	require.NoError(t, err)
	forwarded := []headerforwarding.ForwardedHeader{{HTTPName: "X-User"}}

	h := NewHandler(HandlerParams{
		Logger:             zap.NewNop(),
		AgentURL:           "ws://127.0.0.1:1",
		MaxRequestBodySize: 1 << 20,
		MCP:                sharedMCPHandler(t),
		AuthMiddleware:     auth.wrap,
	})
	grant := registerTurn(h.mcp.turns, testAGUIRun(), nil)
	announced := announcedHeaders(t, forwarded, grant, "", "alice")

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	post := func(path string, headers ...acp.HttpHeader) int {
		req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(`{}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		for _, hdr := range headers {
			req.Header.Set(hdr.Name, hdr.Value)
		}
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}
	token := acp.HttpHeader{Name: turnTokenHeader, Value: grant.token}
	user := acp.HttpHeader{Name: "X-User", Value: "alice"}
	withSlash := routeMCPPrefix + grant.routeID + "/"
	noSlash := routeMCPPrefix + grant.routeID

	assert.Equal(t, http.StatusUnauthorized, post(routeChat), "the chat endpoint is behind the authenticator")
	assert.Equal(t, http.StatusUnauthorized, post(withSlash, token), "the turn token alone does not pass the authenticator")
	assert.Equal(t, http.StatusUnauthorized, post(noSlash, token), "nor on the no-slash form of the route")
	assert.Equal(t, http.StatusUnauthorized, post(withSlash, user, acp.HttpHeader{Name: turnTokenHeader, Value: "wrong"}),
		"an admitted caller still needs the turn's token")

	session := connectTurnMCPWithHeaders(t, ts, withSlash, announced)
	listed, err := session.ListTools(t.Context(), &mcp.ListToolsParams{})
	require.NoError(t, err)
	assert.Contains(t, toolNames(listed.Tools), "get_services",
		"the announced headers pass the authenticator and the turn check")
}

// chatRouteHarness mounts a Handler whose agent records the prompt it was sent,
// so a test can tell a refused chat request from one that started a turn.
func chatRouteHarness(t *testing.T, trustedOrigins []string) (*http.ServeMux, *mockACPAgent) {
	t.Helper()
	agent := &mockACPAgent{}
	wsURL, cleanup := startMockACPWebSocketServer(t, agent)
	t.Cleanup(cleanup)
	h := NewHandler(HandlerParams{
		Logger:             zap.NewNop(),
		AgentURL:           wsURL,
		MaxRequestBodySize: 1 << 20,
		TrustedOrigins:     trustedOrigins,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux, agent
}

func chatRouteRequest(method, contentType string, header http.Header) *http.Request {
	req := httptest.NewRequest(method, routeChat,
		strings.NewReader(`{"messages":[{"id":"1","role":"user","content":"hello"}]}`))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for name, values := range header {
		req.Header[name] = values
	}
	return req
}

func TestChatRouteRefusesCrossSiteRequests(t *testing.T) {
	// httptest.NewRequest addresses example.com, so this is the route's own origin.
	const ownOrigin = "http://example.com"
	crossSite := func(origin string) http.Header {
		return http.Header{"Origin": {origin}, "Sec-Fetch-Site": {"cross-site"}}
	}
	tests := []struct {
		name           string
		method         string
		contentType    string
		header         http.Header
		trustedOrigins []string
		wantStatus     int
	}{
		{
			name:        "cross-site text/plain",
			method:      http.MethodPost,
			contentType: "text/plain;charset=UTF-8",
			header:      crossSite("https://attacker.example"),
			wantStatus:  http.StatusForbidden,
		},
		{
			name:        "cross-site application/json",
			method:      http.MethodPost,
			contentType: "application/json",
			header:      crossSite("https://attacker.example"),
			wantStatus:  http.StatusForbidden,
		},
		{
			name:        "same-site is still another origin",
			method:      http.MethodPost,
			contentType: "application/json",
			header:      http.Header{"Origin": {"http://example.com:9999"}, "Sec-Fetch-Site": {"same-site"}},
			wantStatus:  http.StatusForbidden,
		},
		{
			name:        "origin that does not match the host, without fetch metadata",
			method:      http.MethodPost,
			contentType: "application/json",
			header:      http.Header{"Origin": {"https://attacker.example"}},
			wantStatus:  http.StatusForbidden,
		},
		{
			name:        "form post from the same origin",
			method:      http.MethodPost,
			contentType: "application/x-www-form-urlencoded",
			header:      http.Header{"Origin": {ownOrigin}, "Sec-Fetch-Site": {"same-origin"}},
			wantStatus:  http.StatusUnsupportedMediaType,
		},
		{
			name:       "no content type",
			method:     http.MethodPost,
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name:        "malformed content type",
			method:      http.MethodPost,
			contentType: "application/json; =",
			wantStatus:  http.StatusUnsupportedMediaType,
		},
		{
			name:           "wildcard CORS origin is not trusted",
			method:         http.MethodPost,
			contentType:    "application/json",
			header:         crossSite("https://ui.example"),
			trustedOrigins: []string{"https://*.example", "*"},
			wantStatus:     http.StatusForbidden,
		},
		{
			name:       "GET still gets the chat endpoint's own answer",
			method:     http.MethodGet,
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:        "same-origin application/json",
			method:      http.MethodPost,
			contentType: "application/json; charset=utf-8",
			header:      http.Header{"Origin": {ownOrigin}, "Sec-Fetch-Site": {"same-origin"}},
			wantStatus:  http.StatusOK,
		},
		{
			name:        "non-browser application/json",
			method:      http.MethodPost,
			contentType: "application/json",
			wantStatus:  http.StatusOK,
		},
		{
			name:           "trusted CORS origin",
			method:         http.MethodPost,
			contentType:    "application/json",
			header:         crossSite("https://ui.example"),
			trustedOrigins: []string{"https://ui.example", "not an origin"},
			wantStatus:     http.StatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, agent := chatRouteHarness(t, tt.trustedOrigins)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, chatRouteRequest(tt.method, tt.contentType, tt.header))
			require.Equal(t, tt.wantStatus, rr.Code, "body=%q", rr.Body.String())
			_, _, prompt := agent.snapshot()
			if tt.wantStatus == http.StatusOK {
				require.NotNil(t, prompt, "an admitted request starts a turn")
				return
			}
			assert.Nil(t, prompt, "a refused request must not reach the agent")
			assert.NotEqual(t, "text/event-stream", rr.Header().Get("Content-Type"))
		})
	}
}

func TestNewCrossOriginProtectionWarnsAboutSkippedOrigins(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	c := newCrossOriginProtection(zap.New(core), []string{"https://ui.example", "https://*.example", "not an origin"})
	require.Equal(t, 2, logs.Len())
	assert.Equal(t, "https://*.example", logs.All()[0].ContextMap()["origin"])

	req := httptest.NewRequest(http.MethodPost, "/", http.NoBody)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://ui.example")
	require.NoError(t, c.Check(req))
	req.Header.Set("Origin", "https://a.example")
	require.Error(t, c.Check(req))
}

// The agent dials the turn-scoped endpoint without browser headers, so it is
// unaffected; a browser request from another origin is refused there too.
func TestTurnScopedEndpointRefusesCrossSiteRequests(t *testing.T) {
	h := mcpEnabledHandler(t, "")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	grant := registerTurn(h.mcp.turns, testAGUIRun(), nil)

	post := func(header http.Header) int {
		req := httptest.NewRequest(http.MethodPost, routeMCPPrefix+grant.routeID+"/", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(turnTokenHeader, grant.token)
		for name, values := range header {
			req.Header[name] = values
		}
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr.Code
	}
	assert.Equal(t, http.StatusForbidden, post(http.Header{
		"Origin":         {"https://attacker.example"},
		"Sec-Fetch-Site": {"cross-site"},
	}))
	assert.NotEqual(t, http.StatusForbidden, post(nil))
}
