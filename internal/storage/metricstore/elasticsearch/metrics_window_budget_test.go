// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package elasticsearch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	es "github.com/jaegertracing/jaeger/internal/storage/elasticsearch"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/config"
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/metricstore"
)

// maxMetricsPoints matches the query HTTP handler budget: integer window/step.
// Call and error histograms use lookback+ratePer (calculateRateTimeRange);
// latency histograms use lookback. An over-budget query must fail before
// Search is issued. OpenSearch 2.19 allocates every empty date_histogram
// bucket in that request before it consults search.max_buckets.
const maxMetricsPoints = 10000

const emptyMetricsSearchResponse = `{
  "took": 1,
  "hits": {"total": 0, "hits": []},
  "aggregations": {"results_buckets": {"buckets": []}}
}`

func TestElasticsearchMetricsReaderRejectsOversizedWindow(t *testing.T) {
	end := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	attackLookback := 1000 * 24 * time.Hour
	require.Equal(t, int64(86_400_000_000), attackLookback.Milliseconds())

	step := time.Second
	rate := time.Second
	allowCallLookback := time.Duration(maxMetricsPoints)*step - rate
	rejectCallLookback := time.Duration(maxMetricsPoints) * step

	tests := []struct {
		name       string
		call       func(context.Context, *MetricsReader) error
		wantSearch bool
	}{
		{
			name: "call rates attack window",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetCallRates(ctx, &metricstore.CallRateQueryParameters{
					BaseQueryParameters: metricsParams(end, attackLookback, time.Millisecond, 10*time.Minute),
				})
				return err
			},
		},
		{
			name: "latencies attack window",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetLatencies(ctx, &metricstore.LatenciesQueryParameters{
					BaseQueryParameters: metricsParams(end, attackLookback, time.Millisecond, time.Millisecond),
					Quantile:            0.95,
				})
				return err
			},
		},
		{
			name: "error rates attack window",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetErrorRates(ctx, &metricstore.ErrorRateQueryParameters{
					BaseQueryParameters: metricsParams(end, attackLookback, time.Millisecond, 10*time.Minute),
				})
				return err
			},
		},
		{
			name: "call rates one millisecond step over one hour",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetCallRates(ctx, &metricstore.CallRateQueryParameters{
					BaseQueryParameters: metricsParams(end, time.Hour, time.Millisecond, time.Millisecond),
				})
				return err
			},
		},
		{
			name: "call rates one second step over one thousand days",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetCallRates(ctx, &metricstore.CallRateQueryParameters{
					BaseQueryParameters: metricsParams(end, attackLookback, time.Second, time.Millisecond),
				})
				return err
			},
		},
		{
			name: "call rates ratePer extends the histogram past the budget",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetCallRates(ctx, &metricstore.CallRateQueryParameters{
					BaseQueryParameters: metricsParams(end, time.Hour, 5*time.Second, attackLookback),
				})
				return err
			},
		},
		{
			name: "error rates ratePer extends the histogram past the budget",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetErrorRates(ctx, &metricstore.ErrorRateQueryParameters{
					BaseQueryParameters: metricsParams(end, time.Hour, 5*time.Second, attackLookback),
				})
				return err
			},
		},
		{
			name: "call rates one point over the budget",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetCallRates(ctx, &metricstore.CallRateQueryParameters{
					BaseQueryParameters: metricsParams(end, rejectCallLookback, step, rate),
				})
				return err
			},
		},
		{
			name: "latencies one point over the budget",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetLatencies(ctx, &metricstore.LatenciesQueryParameters{
					BaseQueryParameters: metricsParams(end, time.Duration(maxMetricsPoints+1)*step, step, time.Millisecond),
					Quantile:            0.95,
				})
				return err
			},
		},
		{
			name: "call rates at the budget",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetCallRates(ctx, &metricstore.CallRateQueryParameters{
					BaseQueryParameters: metricsParams(end, allowCallLookback, step, rate),
				})
				return err
			},
			wantSearch: true,
		},
		{
			name: "latencies at the budget",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetLatencies(ctx, &metricstore.LatenciesQueryParameters{
					BaseQueryParameters: metricsParams(end, time.Duration(maxMetricsPoints)*step, step, time.Millisecond),
					Quantile:            0.95,
				})
				return err
			},
			wantSearch: true,
		},
		{
			name: "error rates at the budget",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetErrorRates(ctx, &metricstore.ErrorRateQueryParameters{
					BaseQueryParameters: metricsParams(end, allowCallLookback, step, rate),
				})
				return err
			},
			wantSearch: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader, searches := newBudgetMetricsReader(t)
			err := tc.call(context.Background(), reader)
			if tc.wantSearch {
				require.NoError(t, err)
				require.Positive(t, searches.Load(), "in-budget query must still reach Elasticsearch")
				return
			}
			require.Error(t, err)
			require.Zero(t, searches.Load(), "over-budget query must not reach Elasticsearch")
		})
	}
}

func metricsParams(end time.Time, lookback, step, rate time.Duration) metricstore.BaseQueryParameters {
	return metricstore.BaseQueryParameters{
		ServiceNames: []string{"any"},
		SpanKinds:    []string{"SPAN_KIND_SERVER"},
		EndTime:      &end,
		Lookback:     &lookback,
		Step:         &step,
		RatePer:      &rate,
	}
}

func newBudgetMetricsReader(t *testing.T) (*MetricsReader, *atomic.Int32) {
	t.Helper()
	var searches atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "_search") {
			searches.Add(1)
			// Touch the body so a huge index list or aggregation is what was sent.
			require.NotEmpty(t, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, err = io.WriteString(w, emptyMetricsSearchResponse)
		require.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	cfg := &config.Configuration{
		Servers: []string{server.URL},
		Version: uint(es.OpenSearch2),
		Indices: testIndicesConfig,
	}
	reader := NewMetricsReader(clientProvider(t, cfg, zap.NewNop()), *cfg, zap.NewNop(), noop.NewTracerProvider(), testSpanRotation())
	return reader, &searches
}
