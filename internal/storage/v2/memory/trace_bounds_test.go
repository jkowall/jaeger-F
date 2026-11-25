// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"go.opentelemetry.io/collector/receiver/otlpreceiver"
	"go.opentelemetry.io/collector/receiver/receivertest"

	"github.com/jaegertracing/jaeger/internal/jptrace/sanitizer"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
	"github.com/jaegertracing/jaeger/internal/telemetry/otelsemconv"
)

// maxSpansPerTrace and maxBytesPerTrace are the bounds one trace id must stay
// inside, including the first time that id is stored. Repeated exports that
// reuse an id, and one export that is already over the bound, have to be
// rejected or trimmed so the stored trace never exceeds either number. The
// protobuf size is what GetTraces returns, measured with ptrace.ProtoMarshaler.
//
// 1024 spans and 1 MiB are large enough that the small traces the other tests
// in this package write still fit, and small enough that reusing one id cannot
// retain a request body on every call. The default OTLP HTTP server accepts
// 20 MiB per request and the default gRPC server accepts 4 MiB, and neither
// limit resets between requests.
const (
	maxSpansPerTrace = 1024
	maxBytesPerTrace = 1 << 20
)

const traceBoundsService = "frontend"

func TestTraceBounds_UnderCapRetained(t *testing.T) {
	store := newTraceBoundsStore(t, 4)
	id := traceBoundsID(1)
	require.NoError(t, writeSanitized(store, tracesFor(id, traceBoundsService, 1, 4, 32)))

	got := readStored(t, store, id)
	require.True(t, got.found)
	require.Equal(t, 4, got.spans)
	require.LessOrEqual(t, got.bytes, maxBytesPerTrace)
	require.Equal(t, []string{traceBoundsService}, serviceNames(t, store))
}

func TestTraceBounds_RepeatedAppendsCapSpanCount(t *testing.T) {
	store := newTraceBoundsStore(t, 4)
	id := traceBoundsID(1)
	const batch = 100
	written := 0
	for written <= maxSpansPerTrace {
		err := writeSanitized(store, tracesFor(id, traceBoundsService, uint64(written+1), batch, 0))
		// A later batch may be rejected once the cap is reached. It must not be stored.
		_ = err
		written += batch
	}

	got := readStored(t, store, id)
	requireWithinCap(t, got)
	require.Greater(t, written, maxSpansPerTrace)
}

func TestTraceBounds_RepeatedAppendsCapBytes(t *testing.T) {
	store := newTraceBoundsStore(t, 4)
	id := traceBoundsID(1)
	// One accepted span, then appends whose payloads together exceed the byte cap
	// while the span count stays under maxSpansPerTrace. One service name is used
	// for every span, so a cap on distinct service names does not stop the growth.
	require.NoError(t, writeSanitized(store, tracesFor(id, traceBoundsService, 1, 1, 16)))
	require.Equal(t, []string{traceBoundsService}, serviceNames(t, store))

	const (
		batchSpans = 1
		attrBytes  = 64 << 10
		batches    = (maxBytesPerTrace / attrBytes) + 4
	)
	for i := range batches {
		_ = writeSanitized(store, tracesFor(id, traceBoundsService, uint64(i+2), batchSpans, attrBytes))
	}

	got := readStored(t, store, id)
	requireWithinCap(t, got)
	// Evicting the trace may drop its service name. Keeping the name is also fine.
	// Either way a single service was enough for the appends above.
	require.LessOrEqual(t, len(serviceNames(t, store)), 1)
	require.Less(t, batches+1, maxSpansPerTrace)
}

func TestTraceBounds_FirstInsertCapsSpanCount(t *testing.T) {
	store := newTraceBoundsStore(t, 4)
	id := traceBoundsID(1)
	_ = writeSanitized(store, tracesFor(id, traceBoundsService, 1, maxSpansPerTrace+1, 0))

	requireWithinCap(t, readStored(t, store, id))
}

func TestTraceBounds_FirstInsertCapsBytes(t *testing.T) {
	store := newTraceBoundsStore(t, 4)
	id := traceBoundsID(1)
	_ = writeSanitized(store, tracesFor(id, traceBoundsService, 1, 1, maxBytesPerTrace+64<<10))

	got := readStored(t, store, id)
	requireWithinCap(t, got)
	require.LessOrEqual(t, got.spans, 1)
}

func TestTraceBounds_ReshuffledResourceSpansShareTheCap(t *testing.T) {
	store := newTraceBoundsStore(t, 4)
	id := traceBoundsID(1)
	// Two resource spans in one export carry the same trace id. WriteTraces
	// groups them before storeTraces, so the cap applies to the merged trace
	// and not to each resource span on its own.
	td := ptrace.NewTraces()
	appendResourceSpans(td, id, traceBoundsService, 1, 600, 0)
	appendResourceSpans(td, id, traceBoundsService, 601, 600, 0)
	_ = writeSanitized(store, td)

	requireWithinCap(t, readStored(t, store, id))
}

func TestTraceBounds_SameIDDoesNotEvictNeighbor(t *testing.T) {
	store := newTraceBoundsStore(t, 2)
	attack := traceBoundsID(1)
	neighbor := traceBoundsID(2)
	require.NoError(t, writeSanitized(store, tracesFor(attack, traceBoundsService, 1, 1, 8)))
	require.NoError(t, writeSanitized(store, tracesFor(neighbor, "payments", 1, 1, 8)))

	for i := range maxSpansPerTrace + 50 {
		_ = writeSanitized(store, tracesFor(attack, traceBoundsService, uint64(i+2), 1, 0))
	}

	kept := readStored(t, store, neighbor)
	require.True(t, kept.found, "appending to an existing trace id occupies the same ring slot")
	require.Equal(t, 1, kept.spans)
	requireWithinCap(t, readStored(t, store, attack))
}

func TestTraceBounds_ConcurrentAppendsStayWithinCap(t *testing.T) {
	store := newTraceBoundsStore(t, 4)
	id := traceBoundsID(1)
	const (
		workers = 16
		each    = 80 // 1280 spans, over the cap, each write is a single span
	)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := range workers {
		go func(worker int) {
			defer wg.Done()
			<-start
			base := uint64(worker*each + 1)
			for n := range each {
				_ = writeSanitized(store, tracesFor(id, traceBoundsService, base+uint64(n), 1, 0))
			}
		}(w)
	}
	close(start)
	wg.Wait()

	requireWithinCap(t, readStored(t, store, id))
}

func TestTraceBounds_OTLPHTTPRepeatedExportsStayWithinCap(t *testing.T) {
	store := newTraceBoundsStore(t, 4)
	url := startOTLPHTTP(t, sanitizingWriter{store: store})
	id := traceBoundsID(7)

	first := postOTLP(t, url, tracesFor(id, traceBoundsService, 1, 1, 32))
	require.Equal(t, http.StatusOK, first, "an under-cap export with no credentials is stored")
	require.Equal(t, 1, readStored(t, store, id).spans)

	// Each body is far under the receiver's per-request limit. Reusing the id
	// is what pushes the stored trace over the cap.
	const attrBytes = 64 << 10
	posts := (maxBytesPerTrace / attrBytes) + 4
	for i := range posts {
		_ = postOTLP(t, url, tracesFor(id, traceBoundsService, uint64(i+2), 1, attrBytes))
	}

	requireWithinCap(t, readStored(t, store, id))
	require.LessOrEqual(t, len(serviceNames(t, store)), 1)
}

func TestTraceBounds_OTLPHTTPFirstInsertCapsSpanCount(t *testing.T) {
	store := newTraceBoundsStore(t, 4)
	url := startOTLPHTTP(t, sanitizingWriter{store: store})
	id := traceBoundsID(8)

	_ = postOTLP(t, url, tracesFor(id, traceBoundsService, 1, maxSpansPerTrace+50, 0))
	requireWithinCap(t, readStored(t, store, id))
}

type storedTrace struct {
	found bool
	spans int
	bytes int
}

type sanitizingWriter struct {
	store *Store
}

func (w sanitizingWriter) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: true}
}

func (w sanitizingWriter) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	return w.store.WriteTraces(ctx, sanitizer.Sanitize(td))
}

func newTraceBoundsStore(t *testing.T, maxTraces uint32) *Store {
	t.Helper()
	store, err := NewStore(Configuration{MaxTraces: maxTraces})
	require.NoError(t, err)
	return store
}

func writeSanitized(store *Store, td ptrace.Traces) error {
	return store.WriteTraces(context.Background(), sanitizer.Sanitize(td))
}

func requireWithinCap(t *testing.T, got storedTrace) {
	t.Helper()
	if !got.found {
		return
	}
	require.LessOrEqual(t, got.spans, maxSpansPerTrace, "spans stored for one trace id")
	require.LessOrEqual(t, got.bytes, maxBytesPerTrace, "protobuf bytes stored for one trace id")
}

func readStored(t *testing.T, store *Store, id pcommon.TraceID) storedTrace {
	t.Helper()
	var got storedTrace
	n := 0
	for traces, err := range store.GetTraces(context.Background(), tracestore.GetTraceParams{TraceID: id}) {
		require.NoError(t, err)
		require.Len(t, traces, 1)
		marshaled, merr := new(ptrace.ProtoMarshaler).MarshalTraces(traces[0])
		require.NoError(t, merr)
		got = storedTrace{found: true, spans: traces[0].SpanCount(), bytes: len(marshaled)}
		n++
	}
	require.LessOrEqual(t, n, 1)
	return got
}

func serviceNames(t *testing.T, store *Store) []string {
	t.Helper()
	names, err := store.GetServices(context.Background())
	require.NoError(t, err)
	return names
}

func tracesFor(id pcommon.TraceID, service string, firstSpan uint64, n int, attrBytes int) ptrace.Traces {
	td := ptrace.NewTraces()
	appendResourceSpans(td, id, service, firstSpan, n, attrBytes)
	return td
}

func appendResourceSpans(td ptrace.Traces, id pcommon.TraceID, service string, firstSpan uint64, n int, attrBytes int) {
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr(otelsemconv.ServiceNameKey, service)
	ss := rs.ScopeSpans().AppendEmpty()
	ss.Scope().SetName("memory-bounds")
	payload := ""
	if attrBytes > 0 {
		payload = strings.Repeat("a", attrBytes)
	}
	start := time.Unix(1_700_000_000, 0).UTC()
	for i := range n {
		span := ss.Spans().AppendEmpty()
		span.SetTraceID(id)
		span.SetSpanID(traceBoundsSpanID(firstSpan + uint64(i)))
		span.SetName("op")
		span.SetKind(ptrace.SpanKindServer)
		span.SetStartTimestamp(pcommon.NewTimestampFromTime(start))
		span.SetEndTimestamp(pcommon.NewTimestampFromTime(start.Add(time.Millisecond)))
		if attrBytes > 0 {
			span.Attributes().PutStr("payload", payload)
		}
	}
}

func traceBoundsID(last byte) pcommon.TraceID {
	var id pcommon.TraceID
	id[0] = 0x0a
	id[15] = last
	return id
}

func traceBoundsSpanID(n uint64) pcommon.SpanID {
	var id pcommon.SpanID
	binary.BigEndian.PutUint64(id[:], n)
	return id
}

func startOTLPHTTP(t *testing.T, next consumer.Traces) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	factory := otlpreceiver.NewFactory()
	cfg := factory.CreateDefaultConfig().(*otlpreceiver.Config)
	cfg.Protocols.HTTP.GetOrInsertDefault().ServerConfig.NetAddr.Endpoint = addr
	recv, err := factory.CreateTraces(context.Background(), receivertest.NewNopSettings(factory.Type()), cfg, next)
	require.NoError(t, err)
	require.NoError(t, recv.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() {
		require.NoError(t, recv.Shutdown(context.Background()))
	})
	return "http://" + addr + "/v1/traces"
}

func postOTLP(t *testing.T, url string, td ptrace.Traces) int {
	t.Helper()
	body, err := ptraceotlp.NewExportRequestFromTraces(td).MarshalProto()
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	return resp.StatusCode
}
