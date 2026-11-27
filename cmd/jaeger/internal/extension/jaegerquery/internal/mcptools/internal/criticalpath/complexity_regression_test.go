// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package criticalpath

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// These tests fail while the critical-path walk rescans every child on each
// return to a parent, and while removeOverflowingChildren rebuilds a parent's
// child list once per dropped child. Both are quadratic in the number of
// children. A linear walk (sort each child list once, then move a cursor
// backward) and a single rebuild of each child list finish inside the budgets
// below and still return the same sections.

const (
	// sequentialChildren is a root plus this many back-to-back children.
	// At this commit, 4_000 such children took ~400ms and the time quadrupled
	// when the child count doubled, so 10_000 takes a few seconds.
	sequentialChildren = 10_000
	sequentialBudget   = 300 * time.Millisecond

	// overflowingChildren all fall outside the root, so every one is dropped.
	// At this commit, 20_000 drops took ~400ms and scaled the same way.
	overflowingChildren = 40_000
	overflowingBudget   = 250 * time.Millisecond
)

func TestSequentialChildrenCriticalPathStaysBounded(t *testing.T) {
	small := timeCompute(t, sequentialChildrenTrace(sequentialChildren/4))
	largeStarted := time.Now()
	sections, err := ComputeCriticalPathFromTraces(sequentialChildrenTrace(sequentialChildren))
	large := time.Since(largeStarted)
	require.NoError(t, err)
	requireSubquadratic(t, small, large, sequentialBudget)

	require.Len(t, sections, sequentialChildren)
	var covered uint64
	for _, section := range sections {
		require.Equal(t, uint64(1), section.SectionEnd-section.SectionStart)
		covered += section.SectionEnd - section.SectionStart
	}
	require.Equal(t, uint64(sequentialChildren), covered)
}

func TestOverflowingChildrenSanitizedWithoutQuadraticRebuild(t *testing.T) {
	small := timeCompute(t, overflowingChildrenTrace(overflowingChildren/4))
	largeStarted := time.Now()
	sections, err := ComputeCriticalPathFromTraces(overflowingChildrenTrace(overflowingChildren))
	large := time.Since(largeStarted)
	require.NoError(t, err)
	requireSubquadratic(t, small, large, overflowingBudget)

	require.Len(t, sections, 1)
	require.Equal(t, uint64(100), sections[0].SectionEnd-sections[0].SectionStart)
	require.Equal(t, "0000000000000001", sections[0].SpanID)
}

func timeCompute(t *testing.T, traces ptrace.Traces) time.Duration {
	t.Helper()
	started := time.Now()
	_, err := ComputeCriticalPathFromTraces(traces)
	elapsed := time.Since(started)
	require.NoError(t, err)
	return elapsed
}

// requireSubquadratic fails a quadratic rescan. Four times as many children
// costs about sixteen times as long when every return scans the whole child
// list, and under eight times as long when each child is examined a constant
// number of times after one sort.
func requireSubquadratic(t *testing.T, small, large, budget time.Duration) {
	t.Helper()
	require.Less(t, large, budget, "elapsed %s, budget %s", large, budget)
	if small > 10*time.Millisecond {
		require.Lessf(t, large, small*8, "grew from %s to %s when the child count grew 4x", small, large)
	}
}

// sequentialChildrenTrace is a root covering [1us, children+1us) and one child
// per microsecond, each a direct child of the root. The critical path is those
// children in reverse order; every return to the root has to pick the previous
// child.
func sequentialChildrenTrace(children int) ptrace.Traces {
	return childrenTrace(children, 0, children, true, false)
}

// overflowingChildrenTrace is a 100us root whose children all start after it ends.
func overflowingChildrenTrace(children int) ptrace.Traces {
	return childrenTrace(children, 0, children, true, true)
}

func childrenTrace(rootSpan, from, to int, withRoot, overflow bool) ptrace.Traces {
	traces := ptrace.NewTraces()
	rs := traces.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "attacker")
	ss := rs.ScopeSpans().AppendEmpty()

	var traceID pcommon.TraceID
	traceID[15] = 1
	rootID := numberedSpanID(1)

	if withRoot {
		root := ss.Spans().AppendEmpty()
		root.SetTraceID(traceID)
		root.SetSpanID(rootID)
		root.SetName("root")
		root.SetStartTimestamp(1_000)
		if overflow {
			root.SetEndTimestamp(101_000) // 1us .. 101us
		} else {
			root.SetEndTimestamp(pcommon.Timestamp(uint64(rootSpan+1) * 1000))
		}
	}
	for i := from; i < to; i++ {
		sp := ss.Spans().AppendEmpty()
		sp.SetTraceID(traceID)
		sp.SetSpanID(numberedSpanID(uint64(i + 2)))
		sp.SetParentSpanID(rootID)
		sp.SetName("child")
		var start pcommon.Timestamp
		if overflow {
			start = pcommon.Timestamp(uint64(1_000+i) * 1000) // after the root
		} else {
			start = pcommon.Timestamp(uint64(i+1) * 1000)
		}
		sp.SetStartTimestamp(start)
		sp.SetEndTimestamp(start + 1000)
	}
	return traces
}

func numberedSpanID(n uint64) pcommon.SpanID {
	var id pcommon.SpanID
	binary.BigEndian.PutUint64(id[:], n)
	return id
}
