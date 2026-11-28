// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package mcptools

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
	"github.com/jaegertracing/jaeger/internal/telemetry/otelsemconv"
)

const (
	mcpMethodToolsCall = "tools/call"
	errorTypeTool      = "tool_error"

	traceContextMetaTraceParent = "traceparent"
	traceContextMetaTraceState  = "tracestate"
	traceContextMetaBaggage     = "baggage"

	metricStatusSuccess = "success"
	metricStatusError   = "error"

	spanStatusMethodError = "method returned an error"
	spanStatusToolError   = "tool returned an error result"
)

var requestMetaPropagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{},
	propagation.Baggage{},
)

// createTracingMiddleware creates an MCP middleware that emits tool-level spans.
//
// The spans carry no text taken from the request or the result: not the tool
// arguments, not the tool result, and not error messages, which echo both (an
// invalid SQL statement is quoted back, for one). Jaeger usually exports its
// own spans to its own receiver, where they are stored under service jaeger and
// readable by any caller, while query interceptors only redact the trace data
// they return. Tool results hold trace data a caller was allowed to see and
// arguments hold that caller's search literals, so copying either onto a span
// would hand them to every other caller.
func createTracingMiddleware(tracerProvider trace.TracerProvider) mcp.Middleware {
	tracer := tracerProvider.Tracer("jaeger.mcp")

	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			ctx = contextWithRequestMetaTraceContext(ctx, req)

			toolName := toolNameFromRequest(method, req)
			spanName := method
			attrs := []attribute.KeyValue{
				otelsemconv.McpMethodName(method),
			}
			if toolName != "" {
				spanName = method + " " + toolName
				attrs = append(
					attrs,
					otelsemconv.GenAIOperationNameExecuteTool,
					otelsemconv.GenAIToolName(toolName),
				)
			}
			ctx, span := tracer.Start(
				ctx,
				spanName,
				trace.WithSpanKind(trace.SpanKindInternal),
				trace.WithAttributes(attrs...),
			)
			defer span.End()

			result, err := next(ctx, method, req)
			if err != nil {
				span.SetStatus(codes.Error, spanStatusMethodError)
				return result, err
			}
			if callResult, ok := result.(*mcp.CallToolResult); ok && callResult.IsError {
				span.SetAttributes(otelsemconv.ErrorType(errorTypeTool))
				span.SetStatus(codes.Error, spanStatusToolError)
			}

			return result, err
		}
	}
}

// createMetricsMiddleware creates an MCP middleware that records per-method and per-tool invocation metrics.
func createMetricsMiddleware(meterProvider metric.MeterProvider) (mcp.Middleware, error) {
	meter := meterProvider.Meter("jaeger.mcp")
	callCounter, err := meter.Int64Counter(
		"jaeger.mcp.tool.calls",
		metric.WithDescription("Number of MCP method/tool invocations"),
		metric.WithUnit("{call}"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create tool calls counter: %w", err)
	}
	durationHistogram, err := meter.Float64Histogram(
		"jaeger.mcp.tool.duration",
		metric.WithDescription("Duration of MCP method/tool invocations"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create tool duration histogram: %w", err)
	}

	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			start := time.Now()
			toolName := toolNameFromRequest(method, req)

			result, err := next(ctx, method, req)

			status := metricStatusSuccess
			if err != nil {
				status = metricStatusError
			} else if callResult, ok := result.(*mcp.CallToolResult); ok && callResult.IsError {
				status = metricStatusError
			}
			attrs := buildMetricAttributes(method, toolName, status)

			callCounter.Add(ctx, 1, metric.WithAttributes(attrs...))
			durationHistogram.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))

			return result, err
		}
	}, nil
}

// errToolPanicked is what the client sees for a tool call that panicked; the
// panic value and stack go to the server log only.
var errToolPanicked = errors.New("internal error while running the tool")

// createRecoveryMiddleware turns a panic in a method handler into an error for
// that request: an error result for a tool call, or an internal JSON-RPC error
// for any other method. The SDK runs handlers on goroutines of its own with no
// recover, out of reach of the HTTP recovery handler, so without this a single
// panic terminates the process.
func createRecoveryMiddleware(logger *zap.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
			defer func() {
				r := recover()
				if r == nil {
					return
				}
				logger.Error("Recovered from panic in MCP handler",
					zap.String("method", method),
					zap.String("tool", toolNameFromRequest(method, req)),
					zap.Any("panic", r),
					zap.Stack("stack"),
				)
				if method == mcpMethodToolsCall {
					callResult := &mcp.CallToolResult{}
					callResult.SetError(errToolPanicked)
					result, err = callResult, nil
					return
				}
				result, err = nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal error"}
			}()
			return next(ctx, method, req)
		}
	}
}

type searchCapabilitiesQueryService interface {
	SearchCapabilities(ctx context.Context) (tracestore.SearchCapabilities, error)
}

// createFindSpansFilterMiddleware removes find_spans from a tools/list result when the storage
// backend does not declare span search, so that an agent is not offered a tool whose every
// call the query service refuses. A backend that cannot report its capabilities is read as
// one without span search.
func createFindSpansFilterMiddleware(queryAPI searchCapabilitiesQueryService) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, req)
			toolList, ok := result.(*mcp.ListToolsResult)
			if err != nil || !ok {
				return result, err
			}
			if caps, capsErr := queryAPI.SearchCapabilities(ctx); capsErr == nil && caps.SpanSearch {
				return toolList, nil
			}
			toolList.Tools = slices.DeleteFunc(toolList.Tools, func(tool *mcp.Tool) bool {
				return tool.Name == findSpansToolName
			})
			return toolList, nil
		}
	}
}

func buildMetricAttributes(method, toolName, status string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 3)
	attrs = append(attrs, otelsemconv.McpMethodName(method))
	if toolName != "" {
		attrs = append(attrs, otelsemconv.GenAIToolName(toolName))
	}
	attrs = append(attrs, attribute.String("status", status))
	return attrs
}

func toolNameFromRequest(method string, req mcp.Request) string {
	if method != mcpMethodToolsCall || req == nil {
		return ""
	}
	params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	if !ok || params == nil {
		return ""
	}
	return params.Name
}

func contextWithRequestMetaTraceContext(ctx context.Context, req mcp.Request) context.Context {
	if req == nil {
		return ctx
	}

	params := req.GetParams()
	if isNil(params) {
		return ctx
	}

	return requestMetaPropagator.Extract(ctx, &requestMetaCarrier{meta: params.GetMeta()})
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflectValue := reflect.ValueOf(value)
	switch reflectValue.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return reflectValue.IsNil()
	default:
		return false
	}
}

type requestMetaCarrier struct {
	meta mcp.Meta
}

func (carrier *requestMetaCarrier) Get(key string) string {
	value, _ := carrier.meta[key].(string)
	return value
}

func (carrier *requestMetaCarrier) Set(key, value string) {
	if carrier.meta == nil {
		carrier.meta = mcp.Meta{}
	}
	carrier.meta[key] = value
}

func (carrier *requestMetaCarrier) Keys() []string {
	keys := make([]string, 0, len(carrier.meta))
	for key := range carrier.meta {
		keys = append(keys, key)
	}
	return keys
}
