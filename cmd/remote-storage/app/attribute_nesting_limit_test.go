// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/confignet"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"

	"github.com/jaegertracing/jaeger/internal/proto-gen/storage/v2"
	"github.com/jaegertracing/jaeger/internal/storage/v2/memory"
	"github.com/jaegertracing/jaeger/internal/telemetry"
	"github.com/jaegertracing/jaeger/internal/tenancy"
)

// Attribute nesting is the number of ArrayValue and KeyValueList wrappers around the
// leaf. Thirty-two must still be served. Sixty-four must be rejected on every RPC that
// decodes TraceQueryParameters.attributes, whether the wrappers are all arrays, all
// key-value lists, or the two alternating. These tests fail while that nesting is unbounded.
const (
	servedAttributeNesting   = 32
	rejectedAttributeNesting = 64
	// crashAttributeNesting is large enough that one request's decoder frames do not fit
	// in crashStackLimit, and small enough that the payload stays under gRPC's 4 MiB
	// default receive limit. Each array level is an AnyValue frame (400 bytes) plus an
	// ArrayValue frame (208 bytes) in the generated gogo decoder.
	crashAttributeNesting = 20000
	crashStackLimit       = 8 << 20

	findTraceIDsMethod       = "/jaeger.storage.v2.TraceReader/FindTraceIDs"
	findTracesMethod         = "/jaeger.storage.v2.TraceReader/FindTraces"
	findTraceSummariesMethod = "/jaeger.storage.v2.TraceReader/FindTraceSummaries"
)

func TestRemoteStorageServesBoundedAttributeNesting(t *testing.T) {
	addr := startNestingServer(t, false)
	conn := dialNesting(t, addr)
	defer conn.Close()

	payload := traceQueryRequest(nestedArrayValue(2))
	var decoded storage.FindTraceIDsRequest
	require.NoError(t, proto.Unmarshal(payload, &decoded))
	require.Equal(t, int32(1), decoded.GetQuery().GetSearchDepth())
	require.Equal(t, 2, attributeWireDepth(decoded.GetQuery().GetAttributes()[0].GetValue()))

	for _, shape := range []string{"array", "kvlist", "mixed"} {
		t.Run(shape, func(t *testing.T) {
			for _, method := range nestingMethods {
				t.Run(methodName(method), func(t *testing.T) {
					err := invokeNesting(conn, method, traceQueryRequest(nestedValue(shape, servedAttributeNesting)))
					require.Truef(t, readerReached(method, err), "nesting of %d must still reach the reader, got %v", servedAttributeNesting, err)
				})
			}
		})
	}
}

func TestRemoteStorageRejectsDeepAttributeNesting(t *testing.T) {
	addr := startNestingServer(t, false)
	conn := dialNesting(t, addr)
	defer conn.Close()

	for _, shape := range []string{"array", "kvlist", "mixed"} {
		t.Run(shape, func(t *testing.T) {
			for _, method := range nestingMethods {
				t.Run(methodName(method), func(t *testing.T) {
					err := invokeNesting(conn, method, traceQueryRequest(nestedValue(shape, rejectedAttributeNesting)))
					require.Falsef(t, readerReached(method, err), "nesting of %d reached the reader (%v); it must be rejected first", rejectedAttributeNesting, err)
					require.Error(t, err)

					// A rejected request must leave the server able to serve a shallow one.
					err = invokeNesting(conn, method, traceQueryRequest(nestedValue(shape, 1)))
					require.Truef(t, readerReached(method, err), "server did not serve a shallow query after rejecting a deep one: %v", err)
				})
			}
		})
	}
}

// TestDeepAttributeNestingDoesNotCrashRemoteStorage sends a payload that stays within the
// default 4 MiB receive limit but forces about twenty thousand nested decoder frames. The
// child lowers the goroutine stack limit so the unbounded decoder crashes the process.
// Tenancy is enabled for the unary call: FindTraceIDs decodes before that interceptor, so a
// missing tenant must not be what keeps the process alive.
func TestDeepAttributeNestingDoesNotCrashRemoteStorage(t *testing.T) {
	if os.Getenv("JAEGER_NESTING_CHILD") == "1" {
		debug.SetMaxStack(crashStackLimit)
		requireDeepNestingRejected(t, false)
		requireDeepNestingRejected(t, true)
		t.Log("nesting-rejected")
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestDeepAttributeNestingDoesNotCrashRemoteStorage$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "JAEGER_NESTING_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("nesting-rejected")) {
		snippet := out
		if len(snippet) > 2500 {
			snippet = snippet[:2500]
		}
		t.Fatalf("deep attribute nesting was not rejected before it exhausted the goroutine stack: %v\n%s", err, snippet)
	}
}

func requireDeepNestingRejected(t *testing.T, withTenancy bool) {
	t.Helper()
	addr := startNestingServer(t, withTenancy)
	conn := dialNesting(t, addr)
	defer conn.Close()

	methods := nestingMethods
	if withTenancy {
		// The stream interceptor runs before RecvMsg, so tenancy can refuse a stream
		// without decoding it. The unary method decodes first.
		methods = []string{findTraceIDsMethod}
	}
	for _, shape := range []string{"array", "kvlist", "mixed"} {
		for _, method := range methods {
			err := invokeNesting(conn, method, traceQueryRequest(nestedValue(shape, crashAttributeNesting)))
			if readerReached(method, err) {
				t.Fatalf("tenancy=%v %s %s nesting of %d reached the reader", withTenancy, shape, methodName(method), crashAttributeNesting)
			}
			if err == nil {
				t.Fatalf("tenancy=%v %s %s accepted nesting of %d", withTenancy, shape, methodName(method), crashAttributeNesting)
			}
		}
	}
}

var nestingMethods = []string{findTraceIDsMethod, findTracesMethod, findTraceSummariesMethod}

func startNestingServer(t *testing.T, withTenancy bool) string {
	t.Helper()
	factory, err := memory.NewFactory(memory.Configuration{MaxTraces: 10}, telemetry.NoopSettings())
	require.NoError(t, err)
	opts := &tenancy.Options{}
	if withTenancy {
		opts.Enabled = true
		opts.Tenants = []string{"acme"}
	}
	server, err := NewServer(
		context.Background(),
		configgrpc.ServerConfig{NetAddr: confignet.AddrConfig{Endpoint: "127.0.0.1:0"}},
		factory,
		factory,
		tenancy.NewManager(opts),
		telemetry.NoopSettings(),
	)
	require.NoError(t, err)
	require.NoError(t, server.Start(context.Background()))
	t.Cleanup(func() { _ = server.Close() })
	return server.GRPCAddr()
}

func dialNesting(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	return conn
}

// readerReached reports whether the storage reader ran. FindTraceIDs and FindTraces then
// succeed (an empty memory store, search depth 1). FindTraceSummaries returns the memory
// store's unsupported-operation status. A nesting rejection is any other error.
func readerReached(method string, err error) bool {
	if method == findTraceSummariesMethod {
		st, ok := status.FromError(err)
		return ok && st.Code() == codes.Unimplemented && strings.Contains(st.Message(), "trace summaries")
	}
	return err == nil || errors.Is(err, io.EOF)
}

func methodName(method string) string {
	if i := strings.LastIndex(method, "/"); i >= 0 {
		return method[i+1:]
	}
	return method
}

type rawNestingMessage struct{ bytes []byte }

func (*rawNestingMessage) Reset()         {}
func (*rawNestingMessage) String() string { return "raw" }
func (*rawNestingMessage) ProtoMessage()  {}

// rawNestingCodec sends pre-built wire bytes. Naming it proto makes the server decode them
// with the process's registered proto codec, which for these messages is gogo.
type rawNestingCodec struct{}

func (rawNestingCodec) Name() string { return "proto" }

func (rawNestingCodec) Marshal(v any) (mem.BufferSlice, error) {
	m, ok := v.(*rawNestingMessage)
	if !ok {
		return nil, fmt.Errorf("raw nesting codec marshal %T", v)
	}
	dup := append([]byte(nil), m.bytes...)
	return mem.BufferSlice{mem.SliceBuffer(dup)}, nil
}

func (rawNestingCodec) Unmarshal(data mem.BufferSlice, v any) error {
	m, ok := v.(*rawNestingMessage)
	if !ok {
		return fmt.Errorf("raw nesting codec unmarshal %T", v)
	}
	m.bytes = append([]byte(nil), data.Materialize()...)
	return nil
}

func invokeNesting(conn *grpc.ClientConn, method string, payload []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opts := []grpc.CallOption{grpc.ForceCodecV2(rawNestingCodec{})}
	if method == findTraceIDsMethod {
		return conn.Invoke(ctx, method, &rawNestingMessage{bytes: payload}, &rawNestingMessage{}, opts...)
	}
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, method, opts...)
	if err != nil {
		return err
	}
	if err := stream.SendMsg(&rawNestingMessage{bytes: payload}); err != nil {
		return err
	}
	if err := stream.CloseSend(); err != nil {
		return err
	}
	return stream.RecvMsg(&rawNestingMessage{})
}

func appendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func appendLenDelim(dst []byte, fieldNum int, body []byte) []byte {
	dst = appendVarint(dst, uint64(fieldNum)<<3|2)
	dst = appendVarint(dst, uint64(len(body)))
	return append(dst, body...)
}

// nestedValue wraps a string leaf in depth ArrayValue and/or KeyValueList messages.
// "mixed" alternates, starting with an array at the leaf side.
func nestedValue(shape string, depth int) []byte {
	body := appendLenDelim(nil, 1, []byte("x")) // AnyValue.string_value
	for i := range depth {
		useArray := shape == "array" || (shape == "mixed" && i%2 == 0)
		if useArray {
			array := appendLenDelim(nil, 1, body) // ArrayValue.values
			body = appendLenDelim(nil, 5, array)  // AnyValue.array_value
			continue
		}
		kv := appendLenDelim(nil, 1, []byte("k"))
		kv = append(kv, appendLenDelim(nil, 2, body)...) // KeyValue.value
		list := appendLenDelim(nil, 1, kv)               // KeyValueList.values
		body = appendLenDelim(nil, 6, list)              // AnyValue.kvlist_value
	}
	return body
}

func nestedArrayValue(depth int) []byte { return nestedValue("array", depth) }

// traceQueryRequest is a FindTraceIDs, FindTraces, or FindTraceSummaries request: each carries
// TraceQueryParameters in field 1. Search depth is 1 so a query the reader actually runs
// succeeds on the empty memory store instead of being refused as unbounded.
func traceQueryRequest(anyValue []byte) []byte {
	kv := appendLenDelim(nil, 1, []byte("k"))
	kv = append(kv, appendLenDelim(nil, 2, anyValue)...)
	query := appendLenDelim(nil, 3, kv) // TraceQueryParameters.attributes
	query = appendVarint(query, 8<<3)   // TraceQueryParameters.search_depth
	query = appendVarint(query, 1)
	return appendLenDelim(nil, 1, query)
}

func attributeWireDepth(av *storage.AnyValue) int {
	if av == nil || av.Value == nil {
		return -1
	}
	switch v := av.Value.(type) {
	case *storage.AnyValue_StringValue:
		return 0
	case *storage.AnyValue_ArrayValue:
		if v.ArrayValue == nil || len(v.ArrayValue.Values) == 0 {
			return 1
		}
		return 1 + attributeWireDepth(v.ArrayValue.Values[0])
	case *storage.AnyValue_KvlistValue:
		if v.KvlistValue == nil || len(v.KvlistValue.Values) == 0 || v.KvlistValue.Values[0].GetValue() == nil {
			return 1
		}
		return 1 + attributeWireDepth(v.KvlistValue.Values[0].GetValue())
	default:
		return -2
	}
}
