// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package criticalpath

import (
	"cmp"
	"slices"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

// childCursor hands out the children of one span in the order the critical
// path walk picks them. The children are sorted once by end time, latest first,
// and the cursor only moves forward, so every child is examined at most once
// no matter how many times the walk returns to the span.
//
// Moving forward is safe because every bound passed to next is strictly earlier
// than the previous one: a child that ends after the bound, or that starts at
// or after it, can never be picked again.
type childCursor struct {
	children []CPSpan
	next     int
}

func newChildCursor(spanMap map[pcommon.SpanID]CPSpan, currentSpan CPSpan) *childCursor {
	children := make([]CPSpan, 0, len(currentSpan.ChildSpanIDs))
	for _, childID := range currentSpan.ChildSpanIDs {
		if childSpan, ok := spanMap[childID]; ok {
			children = append(children, childSpan)
		}
	}
	// A stable sort keeps the first child in ChildSpanIDs order among children
	// that end at the same time.
	slices.SortStableFunc(children, func(a, b CPSpan) int {
		return cmp.Compare(b.StartTime+b.Duration, a.StartTime+a.Duration)
	})
	return &childCursor{children: children}
}

// lastFinishingChild returns the child span that finished last.
// If returningChildStartTime is provided, it returns the child span that finishes
// just before the specified returningChildStartTime. Children that end at time 0
// are never returned.
func (c *childCursor) lastFinishingChild(returningChildStartTime *uint64) *CPSpan {
	for c.next < len(c.children) {
		childSpan := c.children[c.next]
		childEndTime := childSpan.StartTime + childSpan.Duration
		if childEndTime == 0 {
			return nil
		}
		c.next++
		if returningChildStartTime != nil {
			// Find the child span that finished immediately before this span started.
			if childSpan.StartTime >= *returningChildStartTime || childEndTime > *returningChildStartTime {
				continue
			}
		}
		return &childSpan
	}
	return nil
}
