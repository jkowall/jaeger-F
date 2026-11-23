// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package metricstore

import (
	"errors"
	"fmt"
	"time"
)

// MaxQueryPoints is the largest number of points per series that a metrics query may ask a
// backend to produce, counted as the query window divided by the step, rounded down.
// Backends materialize one point per step across the whole window (Elasticsearch and
// OpenSearch fill empty date_histogram buckets, Prometheus returns one sample per step),
// so the budget bounds both the backend's work and the response the query service loads.
// It sits below Prometheus' own limit of 11000 points per range query.
const MaxQueryPoints = 10000

// ErrTooManyQueryPoints is returned when a metrics query window holds more than MaxQueryPoints steps.
var ErrTooManyQueryPoints = errors.New("metrics query window holds too many steps")

// CheckQueryPoints returns an error unless step is positive and the query window, the sum of
// the given non-negative durations, divided by step is at most MaxQueryPoints.
// The sum is never materialized, so durations whose total does not fit in a time.Duration
// are handled without overflow.
func CheckQueryPoints(step time.Duration, window ...time.Duration) error {
	if step <= 0 {
		return fmt.Errorf("metrics query step must be positive, got %v", step)
	}
	var points int64
	var remainder time.Duration
	for _, d := range window {
		if d < 0 {
			return fmt.Errorf("metrics query window must not be negative, got %v", d)
		}
		q, r := int64(d/step), d%step
		if remainder >= step-r {
			q++
			remainder -= step - r
		} else {
			remainder += r
		}
		if q > MaxQueryPoints-points {
			return fmt.Errorf("%w: at most %d steps of %v are allowed", ErrTooManyQueryPoints, MaxQueryPoints, step)
		}
		points += q
	}
	return nil
}
