// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jaegertracing/jaeger/internal/proto-gen/api_v2/metrics"
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/metricstore"
)

// maxMetricsPoints is the most points a metrics query may ask a backend to
// build, computed as integer window/step. OpenSearch 2.19's date_histogram
// reduce allocates one bucket per step across extended_bounds, including
// empty buckets, before search.max_buckets is applied
// (InternalDateHistogram.addEmptyBuckets). Elasticsearch 8.15/9.0 aborts that
// walk near its default of 65536 buckets, and Prometheus 0.315 rejects a range
// query above 11000 points before executing it. 10000 sits under both caps, so
// a single request cannot make the metrics store materialize the caller's window.
//
// Call-rate and error-rate histograms extend the window by ratePer
// (calculateRateTimeRange). Latency queries and Prometheus range queries use
// lookback alone. A query at exactly this budget is still served.
const maxMetricsPoints = 10000

// recordingMetricsReader counts reader calls. A metrics request that is over
// budget must be rejected before any of these methods run.
type recordingMetricsReader struct {
	mu    sync.Mutex
	calls int
}

func (r *recordingMetricsReader) note() (*metrics.MetricFamily, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return &metrics.MetricFamily{Name: "ok"}, nil
}

func (r *recordingMetricsReader) GetLatencies(context.Context, *metricstore.LatenciesQueryParameters) (*metrics.MetricFamily, error) {
	return r.note()
}

func (r *recordingMetricsReader) GetCallRates(context.Context, *metricstore.CallRateQueryParameters) (*metrics.MetricFamily, error) {
	return r.note()
}

func (r *recordingMetricsReader) GetErrorRates(context.Context, *metricstore.ErrorRateQueryParameters) (*metrics.MetricFamily, error) {
	return r.note()
}

func (r *recordingMetricsReader) GetDimensions(context.Context) ([]metricstore.Dimension, error) {
	return nil, nil
}

func (r *recordingMetricsReader) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestMetricsEndpointsRejectOversizedWindow(t *testing.T) {
	// Durations in the query string are milliseconds, matching parseMetricsQueryParams.
	attackLookback := strconv.FormatInt((1000 * 24 * time.Hour).Milliseconds(), 10)
	require.Equal(t, "86400000000", attackLookback)

	boundaryStep := time.Second
	allowCallWindow := time.Duration(maxMetricsPoints) * boundaryStep
	// ratePer contributes to the call/error histogram window, so the lookback
	// that stays inside the budget is one step shorter than the latency window.
	allowCallLookback := allowCallWindow - boundaryStep
	rejectCallLookback := allowCallWindow

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantCalls  int
	}{
		{
			name:       "calls attack window",
			path:       "/api/metrics/calls?service=any&lookback=" + attackLookback + "&step=1",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "calls attack window grouped by operation",
			path:       "/api/metrics/calls?service=any&groupByOperation=true&lookback=" + attackLookback + "&step=1",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "latencies attack window",
			path:       "/api/metrics/latencies?service=any&quantile=0.95&lookback=" + attackLookback + "&step=1",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "errors attack window",
			path:       "/api/metrics/errors?service=any&lookback=" + attackLookback + "&step=1",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "calls one millisecond step over one hour",
			path:       "/api/metrics/calls?service=any&lookback=" + ms(time.Hour) + "&step=1",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "calls one second step over one thousand days",
			path:       "/api/metrics/calls?service=any&lookback=" + attackLookback + "&step=" + ms(time.Second),
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "calls ratePer extends the histogram past the budget",
			path: "/api/metrics/calls?service=any&lookback=" + ms(time.Hour) + "&step=" + ms(5*time.Second) +
				"&ratePer=" + attackLookback,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "errors ratePer extends the histogram past the budget",
			path: "/api/metrics/errors?service=any&lookback=" + ms(time.Hour) + "&step=" + ms(5*time.Second) +
				"&ratePer=" + attackLookback,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "calls one point over the budget",
			path:       "/api/metrics/calls?service=any&lookback=" + ms(rejectCallLookback) + "&step=" + ms(boundaryStep) + "&ratePer=" + ms(boundaryStep),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "latencies one point over the budget",
			path:       "/api/metrics/latencies?service=any&quantile=0.95&lookback=" + ms(time.Duration(maxMetricsPoints+1)*boundaryStep) + "&step=" + ms(boundaryStep),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "calls at the budget",
			path:       "/api/metrics/calls?service=any&lookback=" + ms(allowCallLookback) + "&step=" + ms(boundaryStep) + "&ratePer=" + ms(boundaryStep),
			wantStatus: http.StatusOK,
			wantCalls:  1,
		},
		{
			name:       "latencies at the budget",
			path:       "/api/metrics/latencies?service=any&quantile=0.95&lookback=" + ms(time.Duration(maxMetricsPoints)*boundaryStep) + "&step=" + ms(boundaryStep) + "&ratePer=1",
			wantStatus: http.StatusOK,
			wantCalls:  1,
		},
		{
			name:       "errors at the budget",
			path:       "/api/metrics/errors?service=any&lookback=" + ms(allowCallLookback) + "&step=" + ms(boundaryStep) + "&ratePer=" + ms(boundaryStep),
			wantStatus: http.StatusOK,
			wantCalls:  1,
		},
		{
			name:       "calls default window",
			path:       "/api/metrics/calls?service=any",
			wantStatus: http.StatusOK,
			wantCalls:  1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader := &recordingMetricsReader{}
			ts := initializeTestServer(t, HandlerOptions.MetricsQueryService(reader))
			req, err := http.NewRequest(http.MethodGet, ts.server.URL+tc.path, nil)
			require.NoError(t, err)
			resp, err := httpClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, tc.wantStatus, resp.StatusCode, "body: %s", body)
			require.Equal(t, tc.wantCalls, reader.callCount(), "metrics reader calls, body: %s", body)
		})
	}
}

func ms(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}
