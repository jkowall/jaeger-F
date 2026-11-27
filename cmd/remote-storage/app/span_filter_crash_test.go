// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/confignet"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	expression "github.com/jaegertracing/jaeger-idl/query/expression/v1"
	"github.com/jaegertracing/jaeger/internal/proto-gen/storage/v2"
	expressionproto "github.com/jaegertracing/jaeger/internal/proto/expression/v1"
	"github.com/jaegertracing/jaeger/internal/storage/v2/memory"
	"github.com/jaegertracing/jaeger/internal/telemetry"
	"github.com/jaegertracing/jaeger/internal/tenancy"
)

// TestRemoteStorageFindSpans_RejectsRegexNestedInAComparison drives the
// default remote-storage server (in-memory traces, tenancy off, no credentials)
// with the span filters FindSpans currently forwards without FinalizeFilter.
// Each filter nests a regex call that has no pattern under an operator whose
// arity check does not look inside its operands. The call must come back
// InvalidArgument, the process must still be serving, and a trace written
// before the call must still be readable. The same payload on FindTraces,
// FindTraceIDs and FindTraceSummaries is rejected the same way, because those
// methods already finalize the filter.
func TestRemoteStorageFindSpans_RejectsRegexNestedInAComparison(t *testing.T) {
	telset := telemetry.NoopSettings()
	factory, err := memory.NewFactory(memory.Configuration{MaxTraces: 10}, telset)
	require.NoError(t, err)
	writer, err := factory.CreateTraceWriter()
	require.NoError(t, err)

	traceID := pcommon.TraceID{1}
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	traces := ptrace.NewTraces()
	rs := traces.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "frontend")
	span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.SetTraceID(traceID)
	span.SetSpanID(pcommon.SpanID{1})
	span.SetName("GET /")
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(base))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(base.Add(time.Millisecond)))
	require.NoError(t, writer.WriteTraces(context.Background(), traces))

	server, err := NewServer(
		context.Background(),
		configgrpc.ServerConfig{
			NetAddr: confignet.AddrConfig{Endpoint: ":0"},
		},
		factory,
		factory,
		tenancy.NewManager(&tenancy.Options{}),
		telset,
	)
	require.NoError(t, err)
	require.NoError(t, server.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, server.Close()) })

	conn, err := grpc.NewClient(server.GRPCAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	client := storage.NewTraceReaderClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	requireTraceStillStored(t, ctx, client, traceID)

	// A well-formed span search reaches the memory reader and returns the stored span.
	valid := wireCall(expression.OpEq,
		exprField(expression.LevelSpan, expression.SpanFieldName),
		exprScalar("GET /"),
	)
	validStream, err := client.FindSpans(ctx, spanRequest(base, valid))
	require.NoError(t, err)
	validResp, err := validStream.Recv()
	require.NoError(t, err)
	require.Positive(t, validResp.GetSpans().ToTraces().SpanCount())
	_, err = validStream.Recv()
	require.ErrorIs(t, err, io.EOF)

	// Trace searches already finalize, so this payload is refused before the reader.
	bad := reportedFilter()
	for _, call := range []struct {
		name string
		recv func() error
	}{
		{
			name: "FindTraces",
			recv: func() error {
				stream, err := client.FindTraces(ctx, &storage.FindTracesRequest{
					Query: &storage.TraceQueryParameters{
						SearchDepth: 1,
						Filter:      bad,
					},
				})
				if err != nil {
					return err
				}
				_, err = stream.Recv()
				return err
			},
		},
		{
			name: "FindTraceIDs",
			recv: func() error {
				resp, err := client.FindTraceIDs(ctx, &storage.FindTraceIDsRequest{
					Query: &storage.TraceQueryParameters{
						SearchDepth: 1,
						Filter:      bad,
					},
				})
				if err != nil {
					return err
				}
				require.Empty(t, resp.GetTraceIds())
				return nil
			},
		},
		{
			name: "FindTraceSummaries",
			recv: func() error {
				stream, err := client.FindTraceSummaries(ctx, &storage.FindTraceSummariesRequest{
					Query: &storage.TraceQueryParameters{
						SearchDepth: 1,
						Filter:      bad,
					},
				})
				if err != nil {
					return err
				}
				_, err = stream.Recv()
				return err
			},
		},
	} {
		t.Run(call.name, func(t *testing.T) {
			requireInvalidArgument(t, call.recv())
			requireTraceStillStored(t, ctx, client, traceID)
		})
	}

	scalar := exprScalar("x")
	emptyRegex := exprCall(expression.OpRegex)
	oneArgRegex := exprCall(expression.OpRegex, exprField(expression.LevelSpan, expression.SpanFieldName))
	comparison := func(op expression.Operator, left, right *expressionproto.Expression) *expressionproto.Call {
		return wireCall(op, left, right)
	}
	// Shapes the memory reader's arity check accepts and then indexes past the
	// regex call's argument list while compiling patterns.
	cases := []struct {
		name   string
		filter *expressionproto.Call
	}{
		{"eq/regex with no arguments", comparison(expression.OpEq, emptyRegex, scalar)},
		{"eq/regex with no arguments on the right", comparison(expression.OpEq, scalar, emptyRegex)},
		{"eq/regex with one argument", comparison(expression.OpEq, oneArgRegex, scalar)},
		{"ne", comparison(expression.OpNe, emptyRegex, scalar)},
		{"gt", comparison(expression.OpGt, emptyRegex, scalar)},
		{"lt", comparison(expression.OpLt, emptyRegex, scalar)},
		{"gte", comparison(expression.OpGte, emptyRegex, scalar)},
		{"lte", comparison(expression.OpLte, emptyRegex, scalar)},
		{"in", comparison(expression.OpIn, emptyRegex, scalar)},
		{"not_in", comparison(expression.OpNotIn, emptyRegex, scalar)},
		{"phrase", comparison(expression.OpPhrase, emptyRegex, scalar)},
		{"fulltext", comparison(expression.OpFulltext, emptyRegex, scalar)},
		{"exists", wireCall(expression.OpExists, emptyRegex)},
		{"and", wireCall(expression.OpAnd, exprCall(expression.OpEq, emptyRegex, scalar))},
		{"or", wireCall(expression.OpOr, exprCall(expression.OpEq, emptyRegex, scalar))},
		{"not", wireCall(expression.OpNot, exprCall(expression.OpEq, emptyRegex, scalar))},
		{"some", wireCall(expression.OpSome,
			exprNested(expression.LevelEvent),
			exprCall(expression.OpEq, emptyRegex, scalar),
		)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream, err := client.FindSpans(ctx, spanRequest(base, tc.filter))
			require.NoError(t, err)
			_, err = stream.Recv()
			requireInvalidArgument(t, err)
			requireTraceStillStored(t, ctx, client, traceID)
		})
	}
}

func reportedFilter() *expressionproto.Call {
	return wireCall(expression.OpEq, exprCall(expression.OpRegex), exprScalar("x"))
}

func spanRequest(base time.Time, filter *expressionproto.Call) *storage.FindSpansRequest {
	return &storage.FindSpansRequest{
		Query: &storage.SpanQueryParameters{
			StartTimeMin: base.Add(-time.Hour),
			StartTimeMax: base.Add(time.Hour),
			Filter:       filter,
		},
	}
}

func requireInvalidArgument(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "got a non-status error %v", err)
	require.Equal(t, codes.InvalidArgument, st.Code(), st.Message())
}

func requireTraceStillStored(t *testing.T, ctx context.Context, client storage.TraceReaderClient, traceID pcommon.TraceID) {
	t.Helper()
	stream, err := client.GetTraces(ctx, &storage.GetTracesRequest{
		Query: []*storage.GetTraceParams{{TraceId: traceID[:]}},
	})
	require.NoError(t, err)
	got, err := stream.Recv()
	require.NoError(t, err)
	require.Positive(t, got.ToTraces().SpanCount())
	_, err = stream.Recv()
	require.ErrorIs(t, err, io.EOF)
}

func wireCall(op expression.Operator, args ...*expressionproto.Expression) *expressionproto.Call {
	return &expressionproto.Call{Op: string(op), Args: args}
}

func exprCall(op expression.Operator, args ...*expressionproto.Expression) *expressionproto.Expression {
	return &expressionproto.Expression{Term: &expressionproto.Expression_Call{Call: wireCall(op, args...)}}
}

func exprScalar(value string) *expressionproto.Expression {
	return &expressionproto.Expression{Term: &expressionproto.Expression_Scalar{Scalar: &expressionproto.Scalar{Value: value}}}
}

func exprField(level expression.Level, name string) *expressionproto.Expression {
	return &expressionproto.Expression{Term: &expressionproto.Expression_Field{Field: &expressionproto.FieldReference{
		Level: string(level),
		Name:  name,
	}}}
}

func exprNested(level expression.Level) *expressionproto.Expression {
	return &expressionproto.Expression{Term: &expressionproto.Expression_Nested{Nested: &expressionproto.NestedReference{
		Level: string(level),
	}}}
}
