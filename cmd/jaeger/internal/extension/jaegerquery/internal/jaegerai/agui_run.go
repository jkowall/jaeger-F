// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package jaegerai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	aguievents "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	aguisse "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/encoding/sse"
	"github.com/coder/acp-go-sdk"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/internal/mcptools"
)

// aguiRunIDSeq is the process-wide monotonic counter that newIDStem
// appends to its time-derived stem.
var aguiRunIDSeq atomic.Uint64

// aguiRun is the server side of one AG-UI run for one chat turn. It writes the
// run's events as AG-UI SSE frames into the HTTP response; acpClient translates
// the agent's output onto it and the turn-scoped MCP endpoint feeds it UI tool calls.
//
// Events are constructed via the typed AG-UI SDK constructors so that
// schema-required fields (toolCallName, messageId, etc.) are positional —
// forgetting one is a compile error rather than a runtime ZodError on the
// frontend. Framing is delegated to the SDK's SSEWriter.
//
// All mutable fields (closed, threadID, runID, messageID, textOpen) are
// guarded by mu. acpClient.SessionUpdate runs on a goroutine other than the
// one driving Prompt, and lifecycle calls (startRun, finishRun, failRun) come
// from the chat handler, so every entry point that touches
// state or writes a frame must acquire the lock first.
type aguiRun struct {
	requestCtx context.Context
	w          http.ResponseWriter
	sse        *aguisse.SSEWriter
	mu         sync.Mutex
	closed     bool
	threadID   string
	runID      string
	messageID  string
	textOpen   bool
}

func newAGUIRun(ctx context.Context, w http.ResponseWriter, threadID, runID string) *aguiRun {
	return &aguiRun{
		requestCtx: ctx,
		w:          w,
		sse:        aguisse.NewSSEWriter(),
		threadID:   threadID,
		runID:      runID,
	}
}

// emit writes a typed AG-UI event as a single SSE frame. The SDK's
// SSEWriter handles JSON encoding, newline escaping, and flushing.
// Must be called with c.mu held; on context cancellation or write error
// the run is marked closed so subsequent emissions are dropped.
func (c *aguiRun) emit(event aguievents.Event) {
	if c.closed {
		return
	}
	if c.requestCtx != nil {
		select {
		case <-c.requestCtx.Done():
			c.closed = true
			return
		default:
		}
	}
	if err := c.sse.WriteEvent(c.requestCtx, c.w, event); err != nil {
		c.closed = true
	}
}

// startRun emits RUN_STARTED and allocates a message id for the assistant
// text stream. ThreadID and RunID are preserved if the caller provided them
// via the AG-UI RunAgentInput payload; otherwise time-derived defaults fill
// in so the frontend always sees stable identifiers per run.
func (c *aguiRun) startRun() {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Allocate one stem per startRun and derive all three IDs from it.
	// Sharing the stem makes the trio visibly correlated in logs/traces — a
	// thread-N-K, run-N-K, msg-N-K triplet tells a debugger at a glance
	// they came from the same run startup. Only the first text message of
	// the run carries the shared stem; closeText clears messageID and
	// ensureTextStart mints a fresh stem for each later message.
	stem := newIDStem()
	if c.threadID == "" {
		c.threadID = "thread-" + stem
	}
	if c.runID == "" {
		c.runID = "run-" + stem
	}
	if c.messageID == "" {
		c.messageID = "msg-" + stem
	}
	c.emit(aguievents.NewRunStartedEvent(c.threadID, c.runID))
}

// newIDStem returns "<nanos>-<seq>". The nanosecond keeps IDs time-sortable
// across processes, and the counter keeps two stems minted in the same
// nanosecond distinct on systems with coarse clock resolution.
func newIDStem() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), aguiRunIDSeq.Add(1))
}

// finishRun closes any open text message and emits RUN_FINISHED. The
// AG-UI schema has no top-level stopReason field, so the sidecar's stop
// reason is forwarded via the schema-supported `result` payload as
// `{"stopReason": "<reason>"}`. Empty stop reasons are omitted so the
// emitted event stays compact.
func (c *aguiRun) finishRun(stopReason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeText()
	if stopReason != "" {
		c.emit(aguievents.NewRunFinishedEventWithOptions(
			c.threadID, c.runID,
			aguievents.WithResult(map[string]any{"stopReason": stopReason}),
		))
		return
	}
	c.emit(aguievents.NewRunFinishedEvent(c.threadID, c.runID))
}

// failRun emits RUN_ERROR with the supplied message. It is safe to call from
// any handler error path; an open text message is closed first so frontends
// can finalize rendering before reporting the failure.
func (c *aguiRun) failRun(message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeText()
	c.emit(aguievents.NewRunErrorEvent(message, aguievents.WithRunID(c.runID)))
}

// ensureTextStart emits TEXT_MESSAGE_START when streamed assistant text is
// observed and no message is open. Subsequent chunks are emitted as
// TEXT_MESSAGE_CONTENT. A messageID is allocated lazily here as well as in
// startRun so an out-of-order SessionUpdate (no preceding startRun) still
// produces a schema-valid event — the SDK rejects events with empty required
// fields at encode time. Must be called with c.mu held.
func (c *aguiRun) ensureTextStart() {
	if c.textOpen {
		return
	}
	if c.messageID == "" {
		c.messageID = "msg-" + newIDStem()
	}
	c.emit(aguievents.NewTextMessageStartEvent(c.messageID))
	c.textOpen = true
}

// closeText emits TEXT_MESSAGE_END for the open text message, if any, and
// clears messageID so the next text chunk starts a new message. The
// @assistant-ui/react-ag-ui aggregator keeps one text part per messageId, so
// text that arrives after a tool call must carry a new id to render after
// the tool call rather than be appended to the text before it. Must be
// called with c.mu held.
func (c *aguiRun) closeText() {
	if !c.textOpen {
		return
	}
	c.emit(aguievents.NewTextMessageEndEvent(c.messageID))
	c.textOpen = false
	c.messageID = ""
}

// EmitUIToolCall fires the TOOL_CALL_START / TOOL_CALL_ARGS /
// TOOL_CALL_END lifecycle for a UI tool the agent invoked via the
// turn-scoped MCP endpoint, writing the events onto the browser SSE stream so
// the frontend executes the tool. It acquires c.mu like the other entry points,
// so it is safe to call concurrently with the SessionUpdate path.
func (c *aguiRun) EmitUIToolCall(toolCallID, toolName string, rawArgs any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeText()
	c.emit(aguievents.NewToolCallStartEvent(toolCallID, toolName))
	if rawArgs != nil {
		c.emit(aguievents.NewToolCallArgsEvent(toolCallID, marshalToolArgsDelta(rawArgs)))
	}
	c.emit(aguievents.NewToolCallEndEvent(toolCallID))
}

var _ acp.Client = (*acpClient)(nil)

// acpClient is the gateway's side of the ACP connection to the sidecar agent
// for one chat turn. The ACP SDK calls it with the agent's session updates and
// permission requests; it translates each update into events on the turn's
// aguiRun and declines the file and terminal operations the gateway does not
// offer.
type acpClient struct {
	run    *aguiRun
	logger *zap.Logger
}

func newACPClient(run *aguiRun, logger *zap.Logger) *acpClient {
	return &acpClient{run: run, logger: logger}
}

// RequestPermission approves, once, a tool call to the tools the gateway
// serves over its MCP endpoint: the Jaeger telemetry tools and the turn's UI
// tools. Every other call is cancelled and logged. The chat caller writes the
// prompt that drives the agent, and anyone who can send spans writes text the
// agent reads back, so an approval for a tool of the agent's own (a shell,
// file edits, fetches, subagents) would run whatever that text asks for on
// the agent's host. AG-UI has no event to put a per-call question to the
// browser, so nobody else can approve such a call either.
//
// Agents like the Claude Agent SDK route every tool call, MCP calls included,
// through session/request_permission and abort a cancelled call as "Tool use
// aborted", which is why the gateway's own tools are approved rather than
// cancelled with the rest.
//
// Only allow_once is selected. An allow_always approval would let later calls
// skip this check, and an agent that offers no allow_once gets a cancel.
func (c *acpClient) RequestPermission(_ context.Context, req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	reason := refuseToolCall(req.ToolCall)
	if reason == "" {
		for _, opt := range req.Options {
			if opt.Kind == acp.PermissionOptionKindAllowOnce {
				return acp.RequestPermissionResponse{
					Outcome: acp.RequestPermissionOutcome{
						Selected: &acp.RequestPermissionOutcomeSelected{OptionId: opt.OptionId},
					},
				}, nil
			}
		}
		reason = "agent offered no allow_once option"
	}
	c.logger.Warn("Refused agent tool permission request",
		zap.String("tool_call_id", string(req.ToolCall.ToolCallId)),
		zap.String("title", valueOrZero(req.ToolCall.Title)),
		zap.String("kind", string(valueOrZero(req.ToolCall.Kind))),
		zap.String("reason", reason),
	)
	return acp.RequestPermissionResponse{
		Outcome: acp.RequestPermissionOutcome{
			Cancelled: &acp.RequestPermissionOutcomeCancelled{},
		},
	}, nil
}

func valueOrZero[T any](v *T) T {
	if v == nil {
		var zero T
		return zero
	}
	return *v
}

// gatewayToolNames are the telemetry tools the gateway's MCP endpoint serves.
var gatewayToolNames = mcptools.ToolNames()

// mcpToolNamePattern is the character set and length MCP allows in a tool name.
var mcpToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// gatewayToolKinds are the ACP tool kinds a call to a gateway tool may carry.
// The gateway's tools only read telemetry or hand a call to the browser, so a
// call of any other kind (execute, edit, delete, move, fetch, think, ...) is a
// tool of the agent's own, whatever its title says.
var gatewayToolKinds = []acp.ToolKind{acp.ToolKindOther, acp.ToolKindRead, acp.ToolKindSearch}

// commandInputKeys are input fields that carry a command for the agent's host
// to run or a task for a subagent to carry out. No gateway tool takes one.
// Agents title a shell call with its command and a subagent call with its
// description, and some omit the kind, so the title alone does not tell such a
// call from a gateway tool.
var commandInputKeys = []string{"command", "cmd", "argv", "script", "shell", "executable", "prompt"}

// refuseToolCall returns why a permission request is not for a gateway tool,
// or "" when it is. The agent puts the name of the tool it was offered in the
// title, optionally namespaced by the MCP server name it was given.
func refuseToolCall(tc acp.ToolCallUpdate) string {
	if tc.Title == nil {
		return "tool call has no title"
	}
	if !isGatewayToolName(*tc.Title) {
		return "not a tool served by the gateway"
	}
	if tc.Kind != nil && !slices.Contains(gatewayToolKinds, *tc.Kind) {
		return "tool kind is not read-only"
	}
	if tc.RawInput == nil {
		return ""
	}
	input, ok := tc.RawInput.(map[string]any)
	if !ok {
		return "tool input is not an object"
	}
	for key := range input {
		if slices.Contains(commandInputKeys, strings.ToLower(key)) {
			return "tool input carries a command or task"
		}
	}
	return ""
}

// isGatewayToolName reports whether title names a telemetry tool or a UI tool,
// bare or under one of the forms agents use to namespace an MCP server's tools.
func isGatewayToolName(title string) bool {
	name := title
	for _, prefix := range []string{
		"mcp__" + mcpServerName + "__",
		mcpServerName + "__",
		mcpServerName + "/",
		mcpServerName + ": ",
		mcpServerName + ".",
	} {
		if rest, ok := strings.CutPrefix(title, prefix); ok {
			name = rest
			break
		}
	}
	if uiName, ok := strings.CutPrefix(name, UIToolPrefix); ok {
		return mcpToolNamePattern.MatchString(uiName)
	}
	return slices.Contains(gatewayToolNames, name)
}

func (c *acpClient) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	run := c.run
	run.mu.Lock()
	defer run.mu.Unlock()
	u := n.Update
	if u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text != nil {
		text := u.AgentMessageChunk.Content.Text.Text
		// The AG-UI SDK rejects an empty delta and closes the stream.
		// Whitespace between tokens and paragraphs is a real delta and is kept.
		// Whitespace before the first visible text must not open a message.
		if text != "" && (run.textOpen || strings.TrimSpace(text) != "") {
			run.ensureTextStart()
			run.emit(aguievents.NewTextMessageContentEvent(run.messageID, text))
		}
	}
	if u.ToolCall != nil {
		run.closeText()
		// The agent populates ACP Title with the tool name it was offered, which
		// for a UI tool carries UIToolPrefix. Strip it so the frontend sees the
		// name it registered.
		run.emit(aguievents.NewToolCallStartEvent(
			string(u.ToolCall.ToolCallId),
			stripUIToolPrefix(u.ToolCall.Title),
		))
		if u.ToolCall.RawInput != nil {
			run.emit(aguievents.NewToolCallArgsEvent(
				string(u.ToolCall.ToolCallId),
				marshalToolArgsDelta(u.ToolCall.RawInput),
			))
		}
	}
	if u.ToolCallUpdate != nil {
		if u.ToolCallUpdate.RawInput != nil {
			run.emit(aguievents.NewToolCallArgsEvent(
				string(u.ToolCallUpdate.ToolCallId),
				marshalToolArgsDelta(u.ToolCallUpdate.RawInput),
			))
		}
		if u.ToolCallUpdate.RawOutput != nil {
			run.emit(aguievents.NewToolCallResultEvent(
				toolResultMessageID(u.ToolCallUpdate.ToolCallId),
				string(u.ToolCallUpdate.ToolCallId),
				flattenToolResultContent(u.ToolCallUpdate.RawOutput),
			))
		}
		status := valueOrUnknown(u.ToolCallUpdate.Status)
		if status == string(acp.ToolCallStatusCompleted) || status == string(acp.ToolCallStatusFailed) {
			run.emit(aguievents.NewToolCallEndEvent(string(u.ToolCallUpdate.ToolCallId)))
		}
	}
	return nil
}

// The methods below are the acp.Client operations the gateway does not offer:
// filesystem and terminal. They return errors because the chat endpoint
// advertises these capabilities as disabled during Initialize.

var errNotSupported = errors.New("operation not supported by jaeger-ai-gateway")

func (*acpClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errNotSupported
}

func (*acpClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errNotSupported
}

func (*acpClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errNotSupported
}

func (*acpClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errNotSupported
}

func (*acpClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errNotSupported
}

func (*acpClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errNotSupported
}

func (*acpClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errNotSupported
}

// closeACPSession is a best-effort cleanup hook: it tells the agent to
// release the session before the gateway tears down the WebSocket. Meant
// to be invoked via `defer` in ServeHTTP, after defer adapter.Close so it
// fires first (LIFO) while the connection is still open.
//
// The capability gate ensures we never call session/close against agents
// that don't advertise support — they would respond with MethodNotFound.
// WithoutCancel detaches the request context's cancellation so cleanup
// still runs after the client disconnects mid-stream, while preserving
// values such as tracing. The 5s deadline keeps a stuck agent from
// hanging the goroutine indefinitely. Errors are Debug-logged because the
// HTTP response has already been streamed by the time this runs.
func closeACPSession(ctx context.Context, conn *acp.ClientSideConnection, caps acp.AgentCapabilities, sessionID acp.SessionId, logger *zap.Logger) {
	if caps.SessionCapabilities.Close == nil {
		return
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := conn.CloseSession(closeCtx, acp.CloseSessionRequest{
		SessionId: sessionID,
	})
	if err != nil {
		logger.Debug("session/close failed", zap.String("session_id", string(sessionID)), zap.Error(err))
	}
}
