// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package metrics_test

import (
	"testing"
	"time"

	"github.com/jaegertracing/jaeger/internal/metrics"
)

type mockTimer struct {
	recorded time.Duration
}

func (t *mockTimer) Record(d time.Duration) {
	t.recorded = d
}

func TestStopwatch(t *testing.T) {
	timer := &mockTimer{}
	sw := metrics.StartStopwatch(timer)

	// Test ElapsedTime
	time.Sleep(10 * time.Millisecond)
	elapsed1 := sw.ElapsedTime()
	if elapsed1 < 10*time.Millisecond {
		t.Errorf("Expected elapsed1 to be at least 10ms, got %v", elapsed1)
	}

	time.Sleep(10 * time.Millisecond)
	elapsed2 := sw.ElapsedTime()
	if elapsed2 <= elapsed1 {
		t.Errorf("Expected elapsed2 to be greater than elapsed1 (%v), got %v", elapsed1, elapsed2)
	}

	// Test Stop
	sw.Stop()
	if timer.recorded < elapsed2 {
		t.Errorf("Expected recorded duration to be at least %v, got %v", elapsed2, timer.recorded)
	}
	if timer.recorded > elapsed2+100*time.Millisecond {
		t.Errorf("Recorded duration %v is unexpectedly high (elapsed2: %v)", timer.recorded, elapsed2)
	}
}
