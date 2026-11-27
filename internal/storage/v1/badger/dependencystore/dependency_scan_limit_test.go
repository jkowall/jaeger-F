// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package dependencystore_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger/internal/metrics"
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/dependencystore"
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/spanstore"
	"github.com/jaegertracing/jaeger/internal/storage/v1/badger"
)

// maxDependencyTraces is the most traces one Badger dependencies query may
// fold into the graph. It is the trace reader's own default search limit
// (defaultNumTraces in spanstore/reader.go). GetDependencies currently
// replaces that limit with math.MaxUint32 and then retains every span body.
const maxDependencyTraces = 100

// dependencySpanPayload is stored on the parent span, in a field the writer
// does not copy into an index key, so the value log holds the bytes and the
// dependency scan is what brings them back into the process.
const dependencySpanPayload = 16 * 1024

func openDependencyFixture(t *testing.T) (spanstore.Writer, dependencystore.Reader) {
	t.Helper()
	f := badger.NewFactory()
	cfg := *badger.DefaultConfig()
	cfg.Ephemeral = true
	cfg.SyncWrites = false
	f.Config = &cfg
	require.NoError(t, f.Initialize(metrics.NullFactory, zap.NewNop()))
	t.Cleanup(func() {
		require.NoError(t, f.Close())
	})
	writer, err := f.CreateSpanWriter()
	require.NoError(t, err)
	reader, err := f.CreateDependencyReader()
	require.NoError(t, err)
	return writer, reader
}

func writeDependencyTraces(t *testing.T, writer spanstore.Writer, n int, start time.Time) {
	t.Helper()
	payload := make([]byte, dependencySpanPayload)
	for i := range payload {
		payload[i] = 'x'
	}
	body := string(payload)
	for i := range n {
		traceID := model.TraceID{Low: uint64(i + 1), High: 9}
		parent := &model.Span{
			TraceID:       traceID,
			SpanID:        1,
			OperationName: "request",
			Process:       &model.Process{ServiceName: "frontend"},
			StartTime:     start.Add(time.Duration(i) * time.Millisecond),
			Duration:      time.Millisecond,
			Warnings:      []string{body},
		}
		child := &model.Span{
			TraceID:       traceID,
			SpanID:        2,
			OperationName: "request",
			Process:       &model.Process{ServiceName: "backend"},
			StartTime:     parent.StartTime,
			Duration:      time.Millisecond,
			References:    []model.SpanRef{model.NewChildOfRef(traceID, parent.SpanID)},
		}
		// Distinct children make each trace its own edge, so the number of
		// links is the number of traces the reader materialized.
		child.Process.ServiceName = "backend-" + itoa(i)
		require.NoError(t, writer.WriteSpan(context.Background(), parent))
		require.NoError(t, writer.WriteSpan(context.Background(), child))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

func dependencyAlloc(t *testing.T, n int) (uint64, int) {
	t.Helper()
	writer, reader := openDependencyFixture(t)
	end := time.Now().Add(time.Minute)
	writeDependencyTraces(t, writer, n, end.Add(-time.Hour))

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	links, err := reader.GetDependencies(context.Background(), end, 24*time.Hour)
	runtime.ReadMemStats(&after)
	edges := 0
	if err == nil {
		edges = len(links)
	}
	return after.TotalAlloc - before.TotalAlloc, edges
}

// TestGetDependenciesDoesNotMaterializeEveryTrace fails while one
// dependencies read allocates every stored span body. A fix passes it by
// capping how many spans are visited (and therefore how many edges come
// back) instead of setting the trace limit to math.MaxUint32.
func TestGetDependenciesDoesNotMaterializeEveryTrace(t *testing.T) {
	allocSmall, linksSmall := dependencyAlloc(t, maxDependencyTraces*2)
	allocLarge, linksLarge := dependencyAlloc(t, maxDependencyTraces*8)

	require.LessOrEqualf(t, linksSmall, maxDependencyTraces,
		"a %d-trace window returned %d dependency edges; the reader materialized every trace",
		maxDependencyTraces*2, linksSmall)
	require.LessOrEqualf(t, linksLarge, maxDependencyTraces,
		"a %d-trace window returned %d dependency edges; the reader materialized every trace",
		maxDependencyTraces*8, linksLarge)

	// Decoding every span body makes the eight-times-larger window allocate
	// far more than the same capped walk. Measured on the vulnerable reader
	// with a 16KiB warning per trace: 200 traces allocate ~27MB and 800
	// traces allocate ~93MB (about 3.5x). A walk that stops at the cap stays
	// near 1x.
	require.Lessf(t, float64(allocLarge), float64(allocSmall)*1.5,
		"dependency read allocations scaled with the stored traces (%d bytes for %d traces, %d bytes for %d traces)",
		allocSmall, maxDependencyTraces*2, allocLarge, maxDependencyTraces*8)
}
