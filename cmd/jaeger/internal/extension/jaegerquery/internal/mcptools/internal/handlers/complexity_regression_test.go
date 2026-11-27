// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/binary"
	"iter"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/internal/mcptools/internal/types"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/querysvc"
	"github.com/jaegertracing/jaeger/internal/jptrace/sanitizer"
	"github.com/jaegertracing/jaeger/internal/storage/v2/memory"
)

// These tests fail while get_critical_path loads an uncapped trace and walks it
// by rescanning every child on each return to the parent. The same call drops
// out-of-range children by copying the parent's child list once per drop.
// A bounded walk plus observing ctx.Err() makes them pass. The trace is written
// through the storage sanitizer into the memory store, in two batches, then read
// back with MaxTraceSize left at 0, which is the query default.

const (
	attackTraceID = "00000000000000000000000000000001"

	// sequentialChildren matches the shape in the criticalpath package tests.
	// At this commit the walk of 10_000 children takes a few seconds.
	sequentialChildren = 10_000
	sequentialBudget   = 500 * time.Millisecond

	overflowingChildren = 40_000
	overflowingBudget   = 500 * time.Millisecond
)

func TestGetCriticalPathSequentialChildrenStaysBounded(t *testing.T) {
	h := newAttackHandler(t, sequentialChildren, false)
	small := timeHandle(t, sequentialChildren/4, false)
	started := time.Now()
	_, out, err := h.handle(context.Background(), nil, sequentialTraceIDInput())
	elapsed := time.Since(started)
	require.NoError(t, err)
	requireSubquadratic(t, small, elapsed, sequentialBudget)
	require.Len(t, out.Segments, sequentialChildren)
	require.Equal(t, uint64(sequentialChildren), out.CriticalPathDurationUs)
}

func TestGetCriticalPathOverflowingChildrenStayBounded(t *testing.T) {
	h := newAttackHandler(t, overflowingChildren, true)
	small := timeHandle(t, overflowingChildren/4, true)
	started := time.Now()
	_, out, err := h.handle(context.Background(), nil, sequentialTraceIDInput())
	elapsed := time.Since(started)
	require.NoError(t, err)
	requireSubquadratic(t, small, elapsed, overflowingBudget)
	require.Len(t, out.Segments, 1)
	require.Equal(t, uint64(100), out.CriticalPathDurationUs)
}

func TestGetCriticalPathStopsWhenContextCanceled(t *testing.T) {
	h := newAttackHandler(t, sequentialChildren, false)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel once the trace has been loaded and handed to the handler, which is
	// before ComputeCriticalPathFromTraces. A check only before GetTraces does
	// not see this, and the walk today never reads ctx.
	h.queryService = cancelOnYield{next: h.queryService, cancel: cancel}

	started := time.Now()
	_, _, err := h.handle(ctx, nil, sequentialTraceIDInput())
	elapsed := time.Since(started)

	require.Less(t, elapsed, sequentialBudget, "get_critical_path kept running for %s after the caller canceled", elapsed)
	require.ErrorIs(t, err, context.Canceled)
}

func sequentialTraceIDInput() types.GetCriticalPathInput {
	return types.GetCriticalPathInput{TraceID: attackTraceID}
}

func timeHandle(t *testing.T, children int, overflow bool) time.Duration {
	t.Helper()
	small := newAttackHandler(t, children, overflow)
	started := time.Now()
	_, _, err := small.handle(context.Background(), nil, sequentialTraceIDInput())
	elapsed := time.Since(started)
	require.NoError(t, err)
	return elapsed
}

func requireSubquadratic(t *testing.T, small, large, budget time.Duration) {
	t.Helper()
	require.Less(t, large, budget, "elapsed %s, budget %s", large, budget)
	if small > 10*time.Millisecond {
		require.Lessf(t, large, small*8, "grew from %s to %s when the child count grew 4x", small, large)
	}
}

// cancelOnYield wraps a query service and cancels when the loaded trace is
// yielded, after storage and the query adjuster have already run.
type cancelOnYield struct {
	next   queryServiceGetCriticalPathInterface
	cancel context.CancelFunc
}

func (c cancelOnYield) GetTraces(ctx context.Context, params querysvc.GetTraceParams) iter.Seq2[[]ptrace.Traces, error] {
	seq := c.next.GetTraces(ctx, params)
	return func(yield func([]ptrace.Traces, error) bool) {
		seq(func(batch []ptrace.Traces, err error) bool {
			c.cancel()
			return yield(batch, err)
		})
	}
}

func newAttackHandler(t *testing.T, children int, overflow bool) *getCriticalPathHandler {
	t.Helper()
	store, err := memory.NewStore(memory.Configuration{MaxTraces: 8})
	require.NoError(t, err)
	ctx := context.Background()
	mid := children / 2
	require.NoError(t, store.WriteTraces(ctx, sanitizer.Sanitize(attackTrace(children, 0, mid, true, overflow))))
	require.NoError(t, store.WriteTraces(ctx, sanitizer.Sanitize(attackTrace(children, mid, children, false, overflow))))

	// MaxTraceSize 0 is the query default: the whole trace is returned.
	qs := querysvc.NewQueryService(store, store, querysvc.QueryServiceOptions{MaxTraceSize: 0})
	return &getCriticalPathHandler{queryService: qs}
}

func attackTrace(total, from, to int, withRoot, overflow bool) ptrace.Traces {
	traces := ptrace.NewTraces()
	rs := traces.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "attacker")
	ss := rs.ScopeSpans().AppendEmpty()

	var traceID pcommon.TraceID
	traceID[15] = 1
	rootID := attackSpanID(1)

	if withRoot {
		root := ss.Spans().AppendEmpty()
		root.SetTraceID(traceID)
		root.SetSpanID(rootID)
		root.SetName("root")
		root.SetStartTimestamp(1_000)
		if overflow {
			root.SetEndTimestamp(101_000)
		} else {
			root.SetEndTimestamp(pcommon.Timestamp(uint64(total+1) * 1000))
		}
	}
	for i := from; i < to; i++ {
		sp := ss.Spans().AppendEmpty()
		sp.SetTraceID(traceID)
		sp.SetSpanID(attackSpanID(uint64(i + 2)))
		sp.SetParentSpanID(rootID)
		sp.SetName("child")
		var start pcommon.Timestamp
		if overflow {
			start = pcommon.Timestamp(uint64(1_000+i) * 1000)
		} else {
			start = pcommon.Timestamp(uint64(i+1) * 1000)
		}
		sp.SetStartTimestamp(start)
		sp.SetEndTimestamp(start + 1000)
	}
	return traces
}

func attackSpanID(n uint64) pcommon.SpanID {
	var id pcommon.SpanID
	binary.BigEndian.PutUint64(id[:], n)
	return id
}
