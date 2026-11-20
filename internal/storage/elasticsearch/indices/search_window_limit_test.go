// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package indices

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The longest window a periodic lookup may turn into index names is the default
// span-read lookback (72h) plus the default trace-duration padding on both sides
// (24h + 24h). A caller-supplied window must not change that bound: alias and
// data-stream rotation do not walk the window at all.
const maxListedWindow = 72*time.Hour + 2*24*time.Hour

func TestTimeRangeIndices_DoesNotWalkAnUnboundedWindow(t *testing.T) {
	start := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		layout string
		step   time.Duration
		window time.Duration
	}{
		{name: "daily", layout: "2006-01-02", step: 24 * time.Hour, window: 20 * 365 * 24 * time.Hour},
		{name: "hourly", layout: "2006-01-02-15", step: time.Hour, window: 400 * 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed := timeRangeIndices("jaeger-span", tc.layout, start, start.Add(maxListedWindow), -tc.step)
			require.Greater(t, len(allowed), 1, "a max-span-age window still lists every rollover step it covers")
			require.Less(t, len(allowed), 200, "the accepted window itself must stay small")

			huge := timeRangeIndices("jaeger-span", tc.layout, start, start.Add(tc.window), -tc.step)
			require.LessOrEqualf(t, len(huge), len(allowed),
				"listed %d index names for a %s window; the lookup must stop at the %d names of the max listed window",
				len(huge), tc.window, len(allowed))
		})
	}
}

func TestRemoteClusterRotation_DoesNotMultiplyAnUnboundedWindow(t *testing.T) {
	start := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	inner := NewPeriodicRotation("jaeger-span", "2006-01-02-15", time.Hour)
	rot := NewRemoteClusterRotation(inner, []string{"cluster-a", "cluster-b"})

	allowed := len(inner.ReadTargets(start, start.Add(maxListedWindow))) * 3
	require.Greater(t, allowed, 3)

	got := rot.ReadTargets(start, start.Add(400*24*time.Hour))
	require.LessOrEqualf(t, len(got), allowed,
		"listed %d names after prefixing remote clusters; an unbounded window must not be multiplied", len(got))
}

func TestAliasAndDataStreamRotation_IgnoreTheWindow(t *testing.T) {
	start := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

	alias := NewAliasedRotation("jaeger-span-write", "jaeger-span-read")
	require.Equal(t, []string{"jaeger-span-read"}, alias.ReadTargets(start, end))

	stream := NewDataStreamRotation("jaeger.spans", "")
	require.Equal(t, []string{"jaeger.spans"}, stream.ReadTargets(start, end))

	migrating := NewDataStreamRotation("jaeger.spans", "jaeger-span-read")
	require.Equal(t, []string{"jaeger-span-read"}, migrating.ReadTargets(start, end))
}
