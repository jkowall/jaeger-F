// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package jaegerai

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"

	"github.com/jaegertracing/jaeger/internal/telemetry"
)

// These tests are the authorization contract for session/request_permission.
// They fail while acpClient.RequestPermission selects the first allow option
// for every tool call. A chat caller who is not otherwise able to run
// commands or write files on the agent host must not receive an allow
// outcome for any tool the gateway did not itself announce.

const (
	permAllowOnce   acp.PermissionOptionId = "allow-once"
	permAllowAlways acp.PermissionOptionId = "allow-always"
	permRejectOnce  acp.PermissionOptionId = "reject-once"
)

// permissionOptions lists allow_always before allow_once. The gateway must
// not treat that order as a reason to grant a persistent approval, and must
// not treat either allow option as approval of a tool it did not curate.
func permissionOptions() []acp.PermissionOption {
	return []acp.PermissionOption{
		{OptionId: permAllowAlways, Name: "Allow always", Kind: acp.PermissionOptionKindAllowAlways},
		{OptionId: permAllowOnce, Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: permRejectOnce, Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
	}
}

// permissionAuthorizesTool is the decision the ACP example agent applies to a
// permission response before it performs the tool call: a selected allow_once
// or allow_always option authorizes that call, and a cancelled outcome does
// not (github.com/coder/acp-go-sdk v0.13.5 example/agent/main.go).
func permissionAuthorizesTool(options []acp.PermissionOption, resp acp.RequestPermissionResponse) bool {
	if resp.Outcome.Selected == nil {
		return false
	}
	for _, opt := range options {
		if opt.OptionId != resp.Outcome.Selected.OptionId {
			continue
		}
		return opt.Kind == acp.PermissionOptionKindAllowOnce || opt.Kind == acp.PermissionOptionKindAllowAlways
	}
	return false
}

func requirePermissionCancelled(t *testing.T, options []acp.PermissionOption, resp acp.RequestPermissionResponse) {
	t.Helper()
	selected := ""
	if resp.Outcome.Selected != nil {
		selected = string(resp.Outcome.Selected.OptionId)
	}
	require.Falsef(t, permissionAuthorizesTool(options, resp),
		"gateway authorized this tool call by selecting %q", selected)
	require.NotNil(t, resp.Outcome.Cancelled, "unauthorized tool call must be cancelled, selected %q", selected)
	require.Nil(t, resp.Outcome.Selected, "unauthorized tool call must not select an option")
}

func requirePermissionAllowOnce(t *testing.T, resp acp.RequestPermissionResponse) {
	t.Helper()
	require.Nil(t, resp.Outcome.Cancelled, "a curated Jaeger tool must not be cancelled")
	require.NotNil(t, resp.Outcome.Selected)
	require.Equal(t, permAllowOnce, resp.Outcome.Selected.OptionId,
		"a curated tool must be approved once, not remembered for later calls")
}

// mountUnauthenticatedChat registers the chat route the way an ai block
// without ai.auth does: the route is mounted and no authenticator runs.
func mountUnauthenticatedChat(t *testing.T, agentURL string) *http.ServeMux {
	t.Helper()
	cfg := validConfig()
	cfg.AgentURL = agentURL
	mux := http.NewServeMux()
	routes, err := Register(context.Background(), mux, RegisterParams{
		Config:    &cfg,
		Telemetry: telemetry.NoopSettings(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, routes.Close()) })
	return mux
}

// postChatPermission posts body to the unauthenticated chat route and, during
// that turn, has the agent ask permission for each tool in order. The agent
// uses the real ACP connection, so the gateway's RequestPermission runs on
// the session/request_permission request the SDK delivers.
func postChatPermission(t *testing.T, body ChatRequest, options []acp.PermissionOption, tools []acp.ToolCallUpdate) (prompt []string, responses []acp.RequestPermissionResponse) {
	t.Helper()

	agent := &mockACPAgent{}
	var hookErr error
	agent.promptHook = func(ctx context.Context, conn *acp.AgentSideConnection, req acp.PromptRequest) {
		for _, block := range req.Prompt {
			if block.Text != nil {
				prompt = append(prompt, block.Text.Text)
			}
		}
		for _, tool := range tools {
			resp, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
				SessionId: req.SessionId,
				ToolCall:  tool,
				Options:   options,
			})
			if err != nil {
				hookErr = err
				return
			}
			responses = append(responses, resp)
		}
	}

	wsURL, cleanup := startMockACPWebSocketServer(t, agent)
	t.Cleanup(cleanup)

	payload, err := json.Marshal(body)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/ai/chat", bytes.NewReader(payload)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mountUnauthenticatedChat(t, wsURL).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "unauthenticated chat POST must reach the agent, body=%s", rr.Body.String())
	require.NoError(t, hookErr)
	require.Len(t, responses, len(tools), "agent did not receive a permission response for every tool call")
	return prompt, responses
}

func chatWithUserText(text string) ChatRequest {
	return ChatRequest{
		Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: text}},
	}
}

func toolCall(id, title string, kind *acp.ToolKind, rawInput any) acp.ToolCallUpdate {
	var titlePtr *string
	if title != "" {
		titlePtr = acp.Ptr(title)
	}
	return acp.ToolCallUpdate{
		ToolCallId: acp.ToolCallId(id),
		Title:      titlePtr,
		Kind:       kind,
		Status:     acp.Ptr(acp.ToolCallStatusPending),
		RawInput:   rawInput,
	}
}

func TestChatPermissionDeniesAgentBuiltinTools(t *testing.T) {
	const userText = "placeholder-user-instruction"
	options := permissionOptions()
	cases := []struct {
		name string
		tool acp.ToolCallUpdate
	}{
		{
			name: "shell execute",
			tool: toolCall("call-bash", "Bash", acp.Ptr(acp.ToolKindExecute), map[string]any{"command": userText}),
		},
		{
			name: "shell with only allow_once offered",
			tool: toolCall("call-bash-once", "Bash", acp.Ptr(acp.ToolKindExecute), map[string]any{"command": userText}),
		},
		{
			name: "file write",
			tool: toolCall("call-write", "Write", acp.Ptr(acp.ToolKindEdit), map[string]any{
				"path":    "/tmp/placeholder-not-created",
				"content": "placeholder-file-body",
			}),
		},
		{
			name: "file edit by title with kind omitted",
			tool: toolCall("call-edit", "Edit", nil, map[string]any{
				"file_path": "/tmp/placeholder-not-created",
				"content":   "placeholder-file-body",
			}),
		},
		{
			name: "file read",
			tool: toolCall("call-read", "Read", acp.Ptr(acp.ToolKindRead), map[string]any{
				"path": "/tmp/placeholder-not-created",
			}),
		},
		{
			name: "fetch",
			tool: toolCall("call-fetch", "Fetch", acp.Ptr(acp.ToolKindFetch), map[string]any{
				"url": "http://example.invalid/placeholder",
			}),
		},
		{
			name: "delete",
			tool: toolCall("call-delete", "Delete", acp.Ptr(acp.ToolKindDelete), map[string]any{
				"path": "/tmp/placeholder-not-created",
			}),
		},
		{
			name: "move",
			tool: toolCall("call-move", "Move", acp.Ptr(acp.ToolKindMove), map[string]any{
				"source": "/tmp/placeholder-not-created",
				"dest":   "/tmp/placeholder-not-created-dest",
			}),
		},
		{
			name: "command raw input with kind omitted",
			tool: toolCall("call-cmd", "Bash", nil, map[string]any{"cmd": userText}),
		},
		{
			name: "bare command string",
			tool: toolCall("call-raw", "run a command", acp.Ptr(acp.ToolKindOther), userText),
		},
		{
			name: "unknown tool",
			tool: toolCall("call-unknown", "developer", acp.Ptr(acp.ToolKindOther), map[string]any{"task": userText}),
		},
		{
			name: "no tool identity",
			tool: acp.ToolCallUpdate{ToolCallId: "call-empty"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			offered := options
			if tc.name == "shell with only allow_once offered" {
				offered = []acp.PermissionOption{
					{OptionId: permAllowOnce, Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
					{OptionId: permRejectOnce, Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
				}
			}
			prompt, responses := postChatPermission(t, chatWithUserText(userText), offered, []acp.ToolCallUpdate{tc.tool})
			require.Equal(t, []string{userText}, prompt, "the posted user text must be the prompt the agent sees")
			requirePermissionCancelled(t, offered, responses[0])
		})
	}
}

func TestChatPermissionDeniesSpoofedJaegerToolIdentity(t *testing.T) {
	const userText = "placeholder-user-instruction"
	options := permissionOptions()
	cases := []struct {
		name string
		tool acp.ToolCallUpdate
	}{
		{
			name: "telemetry title with execute command",
			tool: toolCall("call-spoof-exec", "get_services", acp.Ptr(acp.ToolKindExecute), map[string]any{"command": userText}),
		},
		{
			name: "telemetry title with command and benign kind",
			tool: toolCall("call-spoof-kind", "get_services", acp.Ptr(acp.ToolKindOther), map[string]any{"command": userText}),
		},
		{
			name: "ui title with file write",
			tool: toolCall("call-spoof-ui", UIToolPrefix+"show_chart", acp.Ptr(acp.ToolKindEdit), map[string]any{
				"path":    "/tmp/placeholder-not-created",
				"content": "placeholder-file-body",
			}),
		},
		{
			name: "telemetry title with argv",
			tool: toolCall("call-spoof-argv", "find_traces", acp.Ptr(acp.ToolKindSearch), map[string]any{
				"argv": []any{userText},
			}),
		},
		{
			name: "ui prefix only",
			tool: toolCall("call-spoof-prefix", UIToolPrefix, acp.Ptr(acp.ToolKindOther), map[string]any{}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prompt, responses := postChatPermission(t, chatWithUserText(userText), options, []acp.ToolCallUpdate{tc.tool})
			require.Equal(t, []string{userText}, prompt)
			requirePermissionCancelled(t, options, responses[0])
		})
	}
}

func TestChatPermissionApprovesCuratedToolOnce(t *testing.T) {
	const userText = "which services are slow"
	options := permissionOptions()
	cases := []struct {
		name string
		tool acp.ToolCallUpdate
	}{
		{
			name: "get_services",
			tool: toolCall("call-services", "get_services", acp.Ptr(acp.ToolKindOther), map[string]any{"pattern": "checkout"}),
		},
		{
			name: "find_traces",
			tool: toolCall("call-traces", "find_traces", acp.Ptr(acp.ToolKindSearch), map[string]any{
				"sql": "SELECT DISTINCT trace_id FROM spans",
			}),
		},
		{
			name: "read_skill",
			tool: toolCall("call-skill", "read_skill", acp.Ptr(acp.ToolKindRead), map[string]any{"name": "SKILL.md"}),
		},
		{
			name: "ui tool",
			tool: toolCall("call-ui", UIToolPrefix+"show_chart", acp.Ptr(acp.ToolKindOther), map[string]any{"chart": "latency"}),
		},
		{
			name: "namespaced telemetry tool",
			tool: toolCall("call-ns", "jaeger/get_services", acp.Ptr(acp.ToolKindOther), map[string]any{"pattern": "checkout"}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, responses := postChatPermission(t, chatWithUserText(userText), options, []acp.ToolCallUpdate{tc.tool})
			requirePermissionAllowOnce(t, responses[0])
		})
	}
}

func TestChatPermissionDoesNotReuseApprovalForNextTool(t *testing.T) {
	const userText = "placeholder-user-instruction"
	options := permissionOptions()
	curated := toolCall("call-services", "get_services", acp.Ptr(acp.ToolKindOther), map[string]any{"pattern": "checkout"})
	shell := toolCall("call-bash", "Bash", acp.Ptr(acp.ToolKindExecute), map[string]any{"command": userText})
	spoof := toolCall("call-spoof", "get_services", acp.Ptr(acp.ToolKindOther), map[string]any{"command": userText})

	t.Run("shell then curated tool", func(t *testing.T) {
		_, responses := postChatPermission(t, chatWithUserText(userText), options, []acp.ToolCallUpdate{shell, curated})
		requirePermissionCancelled(t, options, responses[0])
		requirePermissionAllowOnce(t, responses[1])
	})
	t.Run("curated tool then shell", func(t *testing.T) {
		_, responses := postChatPermission(t, chatWithUserText(userText), options, []acp.ToolCallUpdate{curated, shell})
		requirePermissionAllowOnce(t, responses[0])
		requirePermissionCancelled(t, options, responses[1])
	})
	t.Run("curated tool then same title with a command", func(t *testing.T) {
		_, responses := postChatPermission(t, chatWithUserText(userText), options, []acp.ToolCallUpdate{curated, spoof})
		requirePermissionAllowOnce(t, responses[0])
		requirePermissionCancelled(t, options, responses[1])
	})
}

func TestChatPermissionContextDoesNotAuthorizeShell(t *testing.T) {
	const (
		userText    = "summarize the trace"
		contextText = "placeholder-context-instruction"
	)
	options := permissionOptions()
	body := ChatRequest{
		Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: userText}},
		Context:  []aguitypes.Context{{Description: "span note", Value: contextText}},
	}
	shell := toolCall("call-bash", "Bash", acp.Ptr(acp.ToolKindExecute), map[string]any{"command": contextText})
	prompt, responses := postChatPermission(t, body, options, []acp.ToolCallUpdate{shell})
	require.Equal(t, []string{userText, "span note:\n" + contextText}, prompt,
		"context text posted with the chat must be delivered to the agent with the user message")
	requirePermissionCancelled(t, options, responses[0])
}
