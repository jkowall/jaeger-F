// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package indices

import (
	"time"

	es "github.com/jaegertracing/jaeger/internal/storage/elasticsearch"
)

// PeriodicRotation writes to time-based indices (e.g., "jaeger-span-2024-06-18" daily
// or "jaeger-span-2024-06-18-15" hourly) and reads by computing which indices fall
// within a time range.
type PeriodicRotation struct {
	indexPrefix       string
	dateLayout        string
	rolloverFrequency time.Duration
	// maxReadWindow bounds how much of a read range is turned into index names, since
	// ReadTargets lists one name per rollover step of the range.
	maxReadWindow time.Duration
}

var _ Rotation = (*PeriodicRotation)(nil)

// NewPeriodicRotation creates a PeriodicRotation that lists at most DefaultMaxReadWindow
// of indices per read.
// rolloverFrequency is the index rotation period (e.g., 24*time.Hour for daily, 1*time.Hour for hourly).
func NewPeriodicRotation(indexPrefix, dateLayout string, rolloverFrequency time.Duration) *PeriodicRotation {
	return periodicRotationWithin(indexPrefix, dateLayout, rolloverFrequency, DefaultMaxReadWindow)
}

func periodicRotationWithin(indexPrefix, dateLayout string, rolloverFrequency, maxReadWindow time.Duration) *PeriodicRotation {
	return &PeriodicRotation{
		indexPrefix:       indexPrefix,
		dateLayout:        dateLayout,
		rolloverFrequency: -rolloverFrequency,
		maxReadWindow:     maxReadWindow,
	}
}

func (s *PeriodicRotation) WriteTarget(spanTime time.Time) string {
	return IndexWithDate(s.indexPrefix, s.dateLayout, spanTime)
}

// ReadTargets lists the indices of the range, or of its last maxReadWindow when the range
// is wider. Readers refuse such a range before calling it (see ReadTargetsWithin); the
// bound here keeps any other caller from walking an unbounded range.
func (s *PeriodicRotation) ReadTargets(startTime, endTime time.Time) []string {
	return timeRangeIndicesWithin(s.indexPrefix, s.dateLayout, startTime, endTime, s.rolloverFrequency, s.maxReadWindow)
}

func (*PeriodicRotation) WriteOpType() es.WriteOpType     { return es.WriteOpIndex }
func (*PeriodicRotation) RequiresDocumentTimestamp() bool { return false }
