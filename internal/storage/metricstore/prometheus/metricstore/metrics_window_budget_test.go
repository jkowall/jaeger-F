// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package metricstore

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/internal/config/promcfg"
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/metricstore"
)

// maxMetricsPoints matches the query HTTP handler budget: integer lookback/step.
// Prometheus range queries use that pair as Range.Start/End/Step. An over-budget
// query must fail before query_range is issued. The Prometheus client unmarshals
// the whole matrix into memory, and a backend that honors the requested step
// returns one sample per step.
const maxMetricsPoints = 10000

const emptyPromRangeResponse = `{"status":"success","data":{"resultType":"matrix","result":[]}}`

func TestPrometheusMetricsReaderRejectsOversizedWindow(t *testing.T) {
	end := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	attackLookback := 1000 * 24 * time.Hour
	require.Equal(t, int64(86_400_000_000), attackLookback.Milliseconds())

	step := time.Second
	allowLookback := time.Duration(maxMetricsPoints) * step
	rejectLookback := time.Duration(maxMetricsPoints+1) * step

	tests := []struct {
		name      string
		call      func(context.Context, *MetricsReader) error
		wantQuery bool
	}{
		{
			name: "call rates attack window",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetCallRates(ctx, &metricstore.CallRateQueryParameters{
					BaseQueryParameters: promParams(end, attackLookback, time.Millisecond, 10*time.Minute),
				})
				return err
			},
		},
		{
			name: "latencies attack window",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetLatencies(ctx, &metricstore.LatenciesQueryParameters{
					BaseQueryParameters: promParams(end, attackLookback, time.Millisecond, 10*time.Minute),
					Quantile:            0.95,
				})
				return err
			},
		},
		{
			name: "error rates attack window",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetErrorRates(ctx, &metricstore.ErrorRateQueryParameters{
					BaseQueryParameters: promParams(end, attackLookback, time.Millisecond, 10*time.Minute),
				})
				return err
			},
		},
		{
			name: "call rates one millisecond step over one hour",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetCallRates(ctx, &metricstore.CallRateQueryParameters{
					BaseQueryParameters: promParams(end, time.Hour, time.Millisecond, 10*time.Minute),
				})
				return err
			},
		},
		{
			name: "call rates one second step over one thousand days",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetCallRates(ctx, &metricstore.CallRateQueryParameters{
					BaseQueryParameters: promParams(end, attackLookback, time.Second, 10*time.Minute),
				})
				return err
			},
		},
		{
			name: "call rates one point over the budget",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetCallRates(ctx, &metricstore.CallRateQueryParameters{
					BaseQueryParameters: promParams(end, rejectLookback, step, 10*time.Minute),
				})
				return err
			},
		},
		{
			name: "latencies one point over the budget",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetLatencies(ctx, &metricstore.LatenciesQueryParameters{
					BaseQueryParameters: promParams(end, rejectLookback, step, 10*time.Minute),
					Quantile:            0.95,
				})
				return err
			},
		},
		{
			name: "error rates one point over the budget",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetErrorRates(ctx, &metricstore.ErrorRateQueryParameters{
					BaseQueryParameters: promParams(end, rejectLookback, step, 10*time.Minute),
				})
				return err
			},
		},
		{
			name: "call rates at the budget",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetCallRates(ctx, &metricstore.CallRateQueryParameters{
					BaseQueryParameters: promParams(end, allowLookback, step, 10*time.Minute),
				})
				return err
			},
			wantQuery: true,
		},
		{
			name: "latencies at the budget",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetLatencies(ctx, &metricstore.LatenciesQueryParameters{
					BaseQueryParameters: promParams(end, allowLookback, step, 10*time.Minute),
					Quantile:            0.95,
				})
				return err
			},
			wantQuery: true,
		},
		{
			name: "error rates at the budget",
			call: func(ctx context.Context, r *MetricsReader) error {
				_, err := r.GetErrorRates(ctx, &metricstore.ErrorRateQueryParameters{
					BaseQueryParameters: promParams(end, allowLookback, step, 10*time.Minute),
				})
				return err
			},
			wantQuery: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader, queries := newBudgetPromReader(t)
			err := tc.call(context.Background(), reader)
			if tc.wantQuery {
				require.NoError(t, err)
				require.Positive(t, queries.Load(), "in-budget query must still reach Prometheus")
				return
			}
			require.Error(t, err)
			require.Zero(t, queries.Load(), "over-budget query must not reach Prometheus")
		})
	}
}

func promParams(end time.Time, lookback, step, rate time.Duration) metricstore.BaseQueryParameters {
	return metricstore.BaseQueryParameters{
		ServiceNames: []string{"any"},
		SpanKinds:    []string{"SPAN_KIND_SERVER"},
		EndTime:      &end,
		Lookback:     &lookback,
		Step:         &step,
		RatePer:      &rate,
	}
}

func newBudgetPromReader(t *testing.T) (*MetricsReader, *atomic.Int32) {
	t.Helper()
	var queries atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query_range" {
			queries.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, emptyPromRangeResponse)
		require.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	reader, err := NewMetricsReader(promcfg.Configuration{
		ServerURL:      server.URL,
		ConnectTimeout: 5 * time.Second,
	}, zap.NewNop(), noop.NewTracerProvider(), nil)
	require.NoError(t, err)
	return reader, &queries
}
