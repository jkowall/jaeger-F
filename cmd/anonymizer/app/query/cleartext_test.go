// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package query

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger-idl/proto-gen/api_v2"
	"github.com/jaegertracing/jaeger/cmd/anonymizer/app/writer"
)

// Markers are equal length so a TCP proxy can rewrite one into the other
// inside a protobuf message without shifting the following bytes.
const (
	realOperation = "real-operation-name"
	evilOperation = "evil-operation-name"
	realTagValue  = "cleartext-span-tag-value"
	evilTagValue  = "rewritten-span-tag-value"
)

func init() {
	if len(realOperation) != len(evilOperation) || len(realTagValue) != len(evilTagValue) {
		panic("cleartext markers must stay equal length so the proxy rewrite stays valid protobuf")
	}
}

// TestNonLoopbackGetTraceWireIsCleartext is the passive observation of a
// GetTrace response. query.New is given a non-loopback address and no TLS
// configuration. The bytes delivered to that client must not contain the
// operation name or tag value the query service returned.
func TestNonLoopbackGetTraceWireIsCleartext(t *testing.T) {
	upstream := startPlainQueryServer(t, secretTrace())
	tap := startPathTap(t, upstream, nil)

	observed := fetchThroughInsecureClient(t, tap.addr)
	tap.waitIdle()
	requireWireContains(t, tap.bytes(), realOperation, "plaintext control client did not observe the operation name; the tap is not on the path")
	requireWireContains(t, tap.bytes(), realTagValue, "plaintext control client did not observe the tag; the tap is not on the path")
	require.Equal(t, realOperation, observed[0].OperationName)
	require.Equal(t, realTagValue, tagValue(t, observed[0], "user.email"))
	tap.reset()

	spans, err := queryTraceFromProduct(t, tap.addr)
	tap.waitIdle()
	productWire := tap.bytes()
	requireWireOmitsAll(t, productWire, []string{realOperation, realTagValue}, "GetTrace response was readable on the non-loopback path")
	require.False(t, spansContain(spans, realOperation, realTagValue), "product returned the cleartext trace over a non-loopback connection (err=%v)", err)
}

// TestNonLoopbackGetTraceRejectsSubstitutedSpans rewrites the plaintext
// response on the path and requires the product not to accept the replacement.
func TestNonLoopbackGetTraceRejectsSubstitutedSpans(t *testing.T) {
	upstream := startPlainQueryServer(t, secretTrace())
	tap := startPathTap(t, upstream, []replacement{
		{from: realOperation, to: evilOperation},
		{from: realTagValue, to: evilTagValue},
	})

	substituted := fetchThroughInsecureClient(t, tap.addr)
	require.Equal(t, evilOperation, substituted[0].OperationName, "rewrite did not reach a plaintext gRPC client")
	require.Equal(t, evilTagValue, tagValue(t, substituted[0], "user.email"))

	spans, err := queryTraceFromProduct(t, tap.addr)
	require.False(t, spansContain(spans, evilOperation, evilTagValue), "product accepted spans substituted on the non-loopback path: %+v", spans)
	captured := writeCaptured(t, spans)
	require.NotContains(t, captured, evilOperation)
	require.NotContains(t, captured, evilTagValue)
	require.NotContains(t, captured, realOperation)
	require.NotContains(t, captured, realTagValue)
	if err == nil {
		require.Empty(t, spans)
	}
}

// TestAnonymizerCommandDoesNotStoreCleartextRemoteTrace runs the anonymizer
// command the way an operator does: --query-host-port set to a query service
// that is not a loopback address. The captured file is the pre-anonymization
// output main writes after GetTrace.
func TestAnonymizerCommandDoesNotStoreCleartextRemoteTrace(t *testing.T) {
	upstream := startPlainQueryServer(t, secretTrace())
	tap := startPathTap(t, upstream, []replacement{
		{from: realOperation, to: evilOperation},
		{from: realTagValue, to: evilTagValue},
	})

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", "./cmd/anonymizer",
		"--query-host-port="+tap.addr,
		"--trace-id="+mockTraceID.String(),
		"--output-dir="+dir,
	)
	cmd.Dir = moduleRoot(t)
	out, runErr := cmd.CombinedOutput()
	capturedPath := filepath.Join(dir, mockTraceID.String()+".original.json")
	body, readErr := os.ReadFile(capturedPath)
	require.NoError(t, readErr, "anonymizer output:\n%s", out)
	require.NotContains(t, string(body), realOperation, "captured file stored the remote operation name; command output:\n%s\nrun err: %v", out, runErr)
	require.NotContains(t, string(body), realTagValue, "captured file stored the remote tag; command output:\n%s\nrun err: %v", out, runErr)
	require.NotContains(t, string(body), evilOperation, "captured file stored the substituted operation name; command output:\n%s\nrun err: %v", out, runErr)
	require.NotContains(t, string(body), evilTagValue, "captured file stored the substituted tag; command output:\n%s\nrun err: %v", out, runErr)
}

func secretTrace() *model.Trace {
	return &model.Trace{
		Spans: []*model.Span{
			{
				TraceID:       mockTraceID,
				SpanID:        model.NewSpanID(1),
				OperationName: realOperation,
				Process: &model.Process{
					ServiceName: "billing",
				},
				Tags: []model.KeyValue{
					model.String("user.email", realTagValue),
				},
			},
		},
	}
}

func startPlainQueryServer(t *testing.T, trace *model.Trace) string {
	t.Helper()
	handler := &testGRPCHandler{returnTrace: trace}
	server := grpc.NewServer()
	api_v2.RegisterQueryServiceServer(server, handler)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var wg sync.WaitGroup
	wg.Go(func() {
		_ = server.Serve(lis)
	})
	t.Cleanup(func() {
		server.Stop()
		wg.Wait()
	})
	return lis.Addr().String()
}

type replacement struct {
	from string
	to   string
}

type pathTap struct {
	addr string
	ln   net.Listener
	wg   sync.WaitGroup

	mu  sync.Mutex
	buf bytes.Buffer
}

func startPathTap(t *testing.T, upstream string, repl []replacement) *pathTap {
	t.Helper()
	ln := listenNonLoopback(t)
	tap := &pathTap{addr: ln.Addr().String(), ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			tap.wg.Add(1)
			go func() {
				defer tap.wg.Done()
				forward(conn, upstream, tap, repl)
			}()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		tap.wg.Wait()
	})
	ip, _, err := net.SplitHostPort(tap.addr)
	require.NoError(t, err)
	parsed := net.ParseIP(ip)
	require.NotNil(t, parsed)
	require.False(t, parsed.IsLoopback(), "tap address %s is loopback", tap.addr)
	return tap
}

func (p *pathTap) bytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.buf.Bytes()...)
}

func (p *pathTap) reset() {
	p.mu.Lock()
	p.buf.Reset()
	p.mu.Unlock()
}

func (p *pathTap) waitIdle() {
	p.wg.Wait()
}

func (p *pathTap) record(b []byte) {
	if len(b) == 0 {
		return
	}
	p.mu.Lock()
	p.buf.Write(b)
	p.mu.Unlock()
}

func forward(client net.Conn, upstream string, tap *pathTap, repl []replacement) {
	defer client.Close()
	server, err := net.Dial("tcp", upstream)
	if err != nil {
		return
	}
	defer server.Close()

	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = io.Copy(server, client)
		closeWrite(server)
	})
	wg.Go(func() {
		relay(client, server, tap, repl)
		closeWrite(client)
	})
	wg.Wait()
}

func closeWrite(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

func relay(dst, src net.Conn, tap *pathTap, repl []replacement) {
	buf := make([]byte, 4096)
	var pending []byte
	write := func(b []byte) bool {
		tap.record(b)
		if len(b) == 0 {
			return true
		}
		_, err := dst.Write(b)
		return err == nil
	}
	for {
		// A short read deadline flushes a rewrite hold-back once the server
		// has stopped sending. Without that flush the client would wait for
		// bytes the tap is still holding, and the tap would wait for the
		// client to close.
		_ = src.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, err := src.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			var emit []byte
			emit, pending = drain(pending, repl, false)
			if !write(emit) {
				return
			}
		}
		if err == nil {
			continue
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			emit, _ := drain(pending, repl, true)
			pending = nil
			if !write(emit) {
				return
			}
			continue
		}
		emit, _ := drain(pending, repl, true)
		write(emit)
		return
	}
}

func drain(pending []byte, repl []replacement, eof bool) (emit, rest []byte) {
	if len(pending) == 0 {
		return nil, nil
	}
	if len(repl) == 0 {
		out := append([]byte(nil), pending...)
		return out, nil
	}
	replaced := append([]byte(nil), pending...)
	for _, r := range repl {
		replaced = bytes.ReplaceAll(replaced, []byte(r.from), []byte(r.to))
	}
	keep := 0
	if !eof {
		keep = longestFrom(repl) - 1
	}
	if keep >= len(replaced) {
		return nil, replaced
	}
	emit = append([]byte(nil), replaced[:len(replaced)-keep]...)
	rest = append([]byte(nil), replaced[len(replaced)-keep:]...)
	return emit, rest
}

func longestFrom(repl []replacement) int {
	n := 1
	for _, r := range repl {
		if len(r.from) > n {
			n = len(r.from)
		}
	}
	return n
}

func listenNonLoopback(t *testing.T) net.Listener {
	t.Helper()
	ifaces, err := net.Interfaces()
	require.NoError(t, err)
	var tried []string
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
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
				continue
			}
			ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
			if err != nil {
				tried = append(tried, ip.String()+": "+err.Error())
				continue
			}
			return ln
		}
	}
	t.Fatalf("need a non-loopback IPv4 address to exercise a remote query dial, tried: %s", strings.Join(tried, "; "))
	return nil
}

func fetchThroughInsecureClient(t *testing.T, addr string) []model.Span {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	client := api_v2.NewQueryServiceClient(conn)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream, err := client.GetTrace(ctx, &api_v2.GetTraceRequest{TraceID: mockTraceID})
	require.NoError(t, err)

	var spans []model.Span
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		spans = append(spans, chunk.Spans...)
	}
	require.NotEmpty(t, spans)
	return spans
}

func queryTraceFromProduct(t *testing.T, addr string) ([]model.Span, error) {
	t.Helper()
	q, err := New(addr)
	require.NoError(t, err)
	defer q.Close()

	var (
		spans []model.Span
		qerr  error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		spans, qerr = q.QueryTrace(mockTraceID.String(), time.Time{}, time.Time{})
	}()
	select {
	case <-done:
		return spans, qerr
	case <-time.After(5 * time.Second):
		_ = q.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("QueryTrace did not return after the client connection was closed")
		}
		t.Fatal("QueryTrace did not return within 5s")
		return nil, nil
	}
}

func writeCaptured(t *testing.T, spans []model.Span) string {
	t.Helper()
	dir := t.TempDir()
	w, err := writer.New(writer.Config{
		CapturedFile:   filepath.Join(dir, "captured.json"),
		AnonymizedFile: filepath.Join(dir, "anonymized.json"),
		MappingFile:    filepath.Join(dir, "mapping.json"),
	}, zap.NewNop())
	require.NoError(t, err)
	for i := range spans {
		require.NoError(t, w.WriteSpan(&spans[i]))
	}
	w.Close()
	body, err := os.ReadFile(filepath.Join(dir, "captured.json"))
	require.NoError(t, err)
	return string(body)
}

func requireWireContains(t *testing.T, wire []byte, needle, message string) {
	t.Helper()
	require.True(t, bytes.Contains(wire, []byte(needle)), message)
}

func requireWireOmitsAll(t *testing.T, wire []byte, needles []string, message string) {
	t.Helper()
	var found []string
	for _, needle := range needles {
		if bytes.Contains(wire, []byte(needle)) {
			found = append(found, needle)
		}
	}
	require.Empty(t, found, "%s: found %v", message, found)
}

func spansContain(spans []model.Span, operation, tag string) bool {
	for i := range spans {
		if spans[i].OperationName == operation {
			return true
		}
		for j := range spans[i].Tags {
			if spans[i].Tags[j].VStr == tag {
				return true
			}
		}
	}
	return false
}

func tagValue(t *testing.T, span model.Span, key string) string {
	t.Helper()
	for _, kv := range span.Tags {
		if kv.Key == key {
			return kv.VStr
		}
	}
	t.Fatalf("span has no tag %s", key)
	return ""
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
	_, err := os.Stat(filepath.Join(root, "go.mod"))
	require.NoError(t, err)
	return root
}
