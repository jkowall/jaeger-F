// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package criticalpath

import (
	"errors"
	"sort"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// Section represents a section of the critical path
type Section struct {
	SpanID       string `json:"span_id"`
	SectionStart uint64 `json:"section_start"` // in microseconds
	SectionEnd   uint64 `json:"section_end"`   // in microseconds
}

// computeCriticalPath computes the critical path sections of a trace.
// The algorithm begins with the top-level span and iterates through the last finishing children (LFCs).
// It recursively computes the critical path for each LFC span.
// Upon return from recursion, the algorithm walks backward and picks another child that
// finished just before the LFC's start.
//
// Parameters:
//   - spanMap: A map associating span IDs with spans
//   - spanID: The ID of the current span
//   - criticalPath: An array of critical path sections (accumulated result)
//   - returningChildStartTime: Optional parameter representing the span's start time.
//     It is provided only during the recursive return phase.
//
// Returns: An array of critical path sections for the trace
//
// Example:
//
//	|-------------spanA--------------|
//	   |--spanB--|    |--spanC--|
//
// The LFC of spanA is spanC, as it finishes last among its child spans.
// After invoking CP recursively on LFC, for spanC there is no LFC, so the algorithm walks backward.
// At this point, it uses returningChildStartTime (startTime of spanC) to select another child that finished
// immediately before the LFC's start.
func computeCriticalPath(
	spanMap map[pcommon.SpanID]CPSpan,
	spanID pcommon.SpanID,
	criticalPath []Section,
	returningChildStartTime *uint64,
) []Section {
	selector := newChildSelector(spanMap)

	currentSpanID := spanID
	for {
		currentSpan, ok := spanMap[currentSpanID]
		if !ok {
			return criticalPath
		}

		lastFinishingChildSpan := selector.findLastFinishingChild(currentSpan, returningChildStartTime)

		endTime := currentSpan.StartTime + currentSpan.Duration
		if returningChildStartTime != nil {
			endTime = *returningChildStartTime
		}

		if lastFinishingChildSpan != nil {
			spanCriticalSection := Section{
				SpanID:       currentSpan.SpanID.String(),
				SectionStart: lastFinishingChildSpan.StartTime + lastFinishingChildSpan.Duration,
				SectionEnd:   endTime,
			}

			if spanCriticalSection.SectionStart != spanCriticalSection.SectionEnd {
				criticalPath = append(criticalPath, spanCriticalSection)
			}

			// Now focus shifts to the lastFinishingChildSpan of current span.
			currentSpanID = lastFinishingChildSpan.SpanID
			returningChildStartTime = nil
			continue
		}

		// If there is no last finishing child then total section up to startTime of span is on critical path.
		spanCriticalSection := Section{
			SpanID:       currentSpan.SpanID.String(),
			SectionStart: currentSpan.StartTime,
			SectionEnd:   endTime,
		}

		if spanCriticalSection.SectionStart != spanCriticalSection.SectionEnd {
			criticalPath = append(criticalPath, spanCriticalSection)
		}

		// Now as there are no LFCs focus shifts to parent span from startTime of span.
		if len(currentSpan.References) == 0 {
			return criticalPath
		}

		parentSpanID := currentSpan.References[0].SpanID
		returningChildStartTime = &currentSpan.StartTime
		currentSpanID = parentSpanID
	}
}

type childSelector struct {
	spanMap map[pcommon.SpanID]CPSpan
	state   map[pcommon.SpanID]childSelectorState
}

type childSelectorState struct {
	children []CPSpan
	nextIdx  int
}

func newChildSelector(spanMap map[pcommon.SpanID]CPSpan) *childSelector {
	return &childSelector{
		spanMap: spanMap,
		state:   make(map[pcommon.SpanID]childSelectorState),
	}
}

func (s *childSelector) findLastFinishingChild(currentSpan CPSpan, returningChildStartTime *uint64) *CPSpan {
	state, ok := s.state[currentSpan.SpanID]
	if !ok {
		state = childSelectorState{children: sortedChildrenByEndTimeDesc(currentSpan, s.spanMap)}
	}

	for state.nextIdx < len(state.children) {
		candidate := state.children[state.nextIdx]
		candidateEnd := candidate.StartTime + candidate.Duration

		if returningChildStartTime != nil && candidateEnd >= *returningChildStartTime {
			state.nextIdx++
			continue
		}

		state.nextIdx++
		s.state[currentSpan.SpanID] = state
		child := candidate
		return &child
	}

	s.state[currentSpan.SpanID] = state
	return nil
}

func sortedChildrenByEndTimeDesc(currentSpan CPSpan, spanMap map[pcommon.SpanID]CPSpan) []CPSpan {
	children := make([]CPSpan, 0, len(currentSpan.ChildSpanIDs))

	for _, childID := range currentSpan.ChildSpanIDs {
		childSpan, ok := spanMap[childID]
		if !ok {
			continue
		}
		children = append(children, childSpan)
	}

	sort.Slice(children, func(i, j int) bool {
		leftEndTime := children[i].StartTime + children[i].Duration
		rightEndTime := children[j].StartTime + children[j].Duration
		if leftEndTime != rightEndTime {
			return leftEndTime > rightEndTime
		}
		return children[i].StartTime > children[j].StartTime
	})

	return children
}

// ComputeCriticalPathFromTraces computes the critical path for a given trace
func ComputeCriticalPathFromTraces(traces ptrace.Traces) ([]Section, error) {
	// Find the root span (the one with no parent)
	var rootSpanID pcommon.SpanID
	found := false

	for i := 0; i < traces.ResourceSpans().Len() && !found; i++ {
		rs := traces.ResourceSpans().At(i)
		for j := 0; j < rs.ScopeSpans().Len() && !found; j++ {
			ss := rs.ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				span := ss.Spans().At(k)
				if span.ParentSpanID().IsEmpty() {
					rootSpanID = span.SpanID()
					found = true
					break
				}
			}
		}
	}

	if !found {
		return nil, errors.New("no root span found in trace")
	}

	// Create a map of CPSpan objects to avoid modifying the original trace
	spanMap := CreateCPSpanMap(traces)
	if len(spanMap) == 0 {
		return nil, errors.New("empty trace")
	}

	var criticalPath []Section

	// Apply the algorithm
	defer func() {
		if r := recover(); r != nil {
			criticalPath = nil
		}
	}()

	refinedSpanMap := getChildOfSpans(spanMap)
	sanitizedSpanMap := removeOverflowingChildren(refinedSpanMap)
	criticalPath = computeCriticalPath(sanitizedSpanMap, rootSpanID, criticalPath, nil)

	if criticalPath == nil {
		return nil, errors.New("error while computing critical path for trace")
	}

	return criticalPath, nil
}
