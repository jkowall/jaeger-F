// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package criticalpath

import (
	"go.opentelemetry.io/collector/pdata/pcommon"
)

// removeOverflowingChildren removes or adjusts child spans that overflow their parent's time range.
// An overflowing child span is one whose time range falls outside its parent span's time range.
// The function adjusts the start time and duration of overflowing child spans
// to ensure they fit within the time range of their parent span.
func removeOverflowingChildren(spanMap map[pcommon.SpanID]CPSpan) map[pcommon.SpanID]CPSpan {
	// Get all span IDs
	spanIDs := make([]pcommon.SpanID, 0, len(spanMap))
	for spanID := range spanMap {
		spanIDs = append(spanIDs, spanID)
	}

	// Dropped children are removed from their parent's ChildSpanIDs once, after
	// the loop, so that dropping many children of one parent stays linear.
	droppedChildren := make(map[pcommon.SpanID]map[pcommon.SpanID]struct{})
	dropChild := func(parentID, childID pcommon.SpanID) {
		if droppedChildren[parentID] == nil {
			droppedChildren[parentID] = make(map[pcommon.SpanID]struct{})
		}
		droppedChildren[parentID][childID] = struct{}{}
	}

	for _, spanID := range spanIDs {
		span, ok := spanMap[spanID]
		if !ok || span.ParentSpanID.IsEmpty() {
			continue
		}

		// parentSpan will be undefined when its parent was dropped previously
		parentSpan, parentExists := spanMap[span.ParentSpanID]
		if !parentExists {
			// Drop the child spans of dropped parent span
			delete(spanMap, span.SpanID)
			continue
		}

		childEndTime := span.StartTime + span.Duration
		parentEndTime := parentSpan.StartTime + parentSpan.Duration

		if span.StartTime >= parentSpan.StartTime {
			if span.StartTime >= parentEndTime {
				// child outside of parent range => drop the child span
				//      |----parent----|
				//                        |----child--|
				delete(spanMap, span.SpanID)
				dropChild(parentSpan.SpanID, span.SpanID)
				continue
			}
			if childEndTime > parentEndTime {
				// child end after parent, truncate is needed
				//      |----parent----|
				//              |----child--|
				span.Duration = parentEndTime - span.StartTime
				spanMap[span.SpanID] = span
				continue
			}
			// everything looks good
			// |----parent----|
			//   |----child--|
			continue
		}

		switch {
		case childEndTime <= parentSpan.StartTime:
			// child outside of parent range => drop the child span
			//                      |----parent----|
			//       |----child--|
			delete(spanMap, span.SpanID)
			dropChild(parentSpan.SpanID, span.SpanID)
		case childEndTime <= parentEndTime:
			// child start before parent, truncate is needed
			//      |----parent----|
			//   |----child--|
			span.StartTime = parentSpan.StartTime
			span.Duration = childEndTime - parentSpan.StartTime
			spanMap[span.SpanID] = span
		default:
			// child start before parent and end after parent, truncate is needed
			//      |----parent----|
			//  |---------child---------|
			span.StartTime = parentSpan.StartTime
			span.Duration = parentEndTime - parentSpan.StartTime
			spanMap[span.SpanID] = span
		}
	}

	for parentID, parentSpan := range spanMap {
		dropped, ok := droppedChildren[parentID]
		if !ok {
			continue
		}
		filteredChildren := make([]pcommon.SpanID, 0, len(parentSpan.ChildSpanIDs))
		for _, childID := range parentSpan.ChildSpanIDs {
			if _, isDropped := dropped[childID]; !isDropped {
				filteredChildren = append(filteredChildren, childID)
			}
		}
		parentSpan.ChildSpanIDs = filteredChildren
		spanMap[parentID] = parentSpan
	}

	return spanMap
}
