// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package indices

import (
	"fmt"
	"time"

	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
)

// DefaultMaxReadWindow is the widest time range a periodic rotation turns into index names
// unless it is built with WithMaxReadWindow. It is the widest range a span read covers under
// the default configuration: a read by trace ID looks back max_span_age (72h) and widens that
// range by max_trace_duration (24h) on both sides.
const DefaultMaxReadWindow = 72*time.Hour + 2*24*time.Hour

// ErrReadWindowTooWide is returned for a read whose time range is wider than the reader
// allows. It is an invalid query: a periodic rotation lists one index per rollover step of
// the range, so the range is refused before any name is listed.
var ErrReadWindowTooWide = tracestore.InvalidQuery("time range is too wide")

// windowIndependent is implemented by rotations whose ReadTargets returns the same names
// for every time range, so a range of any width costs a single lookup. A rotation that
// does not implement it is treated as listing names per rollover step.
type windowIndependent interface {
	readTargetsIgnoreRange() bool
}

func ignoresRange(r Rotation) bool {
	wi, ok := r.(windowIndependent)
	return ok && wi.readTargetsIgnoreRange()
}

// ReadTargetsWithin returns r.ReadTargets(startTime, endTime), or ErrReadWindowTooWide
// without listing any name when the range is longer than maxWindow and r names its
// targets by time. Alias and data stream rotations accept a range of any width.
func ReadTargetsWithin(r Rotation, startTime, endTime time.Time, maxWindow time.Duration) ([]string, error) {
	if window := endTime.Sub(startTime); window > maxWindow && !ignoresRange(r) {
		return nil, fmt.Errorf("%w: %s is longer than the maximum of %s", ErrReadWindowTooWide, window, maxWindow)
	}
	return r.ReadTargets(startTime, endTime), nil
}
