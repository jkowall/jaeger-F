// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package elasticsearch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	es "github.com/jaegertracing/jaeger/internal/storage/elasticsearch"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/config"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/esclient"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/indices"
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/metricstore"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
	esv2 "github.com/jaegertracing/jaeger/internal/storage/v2/elasticsearch"
)

func TestMetricsRejectUncappedLookback(t *testing.T) {
	cfg := esv2.DefaultConfig()
	maxSpanAge := cfg.MaxSpanAge
	end := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	raw := indices.BuildRotation(cfg.Indices.IndexPrefix, config.SpanIndexName, cfg.ResolvedSpanRotation(), nil, zap.NewNop())
	rot := &recordingRotation{inner: raw}
	cap := &searchCapture{}
	reader := NewMetricsReader(cap.searcher(t), cfg, zap.NewNop(), noop.NewTracerProvider(), rot)
	accepted := len(raw.ReadTargets(end.Add(-maxSpanAge), end))
	require.Greater(t, accepted, 0)

	queries := []struct {
		name string
		run  func(lookback time.Duration) error
	}{
		{name: "latencies", run: func(lookback time.Duration) error {
			_, err := reader.GetLatencies(context.Background(), &metricstore.LatenciesQueryParameters{
				BaseQueryParameters: baseMetricsQuery(end, lookback),
				Quantile:            0.95,
			})
			return err
		}},
		{name: "calls", run: func(lookback time.Duration) error {
			_, err := reader.GetCallRates(context.Background(), &metricstore.CallRateQueryParameters{
				BaseQueryParameters: baseMetricsQuery(end, lookback),
			})
			return err
		}},
		{name: "errors", run: func(lookback time.Duration) error {
			_, err := reader.GetErrorRates(context.Background(), &metricstore.ErrorRateQueryParameters{
				BaseQueryParameters: baseMetricsQuery(end, lookback),
			})
			return err
		}},
	}
	for _, query := range queries {
		t.Run(query.name, func(t *testing.T) {
			t.Run("lookback at max span age is searched", func(t *testing.T) {
				rot.widest, cap.uris = 0, nil
				err := query.run(maxSpanAge)
				require.NoError(t, err)
				// Call and error rates extend the index window by ratePer (10 minutes).
				require.LessOrEqual(t, rot.widest, maxSpanAge+time.Hour)
				require.NotEmpty(t, cap.uris)
				for _, uri := range cap.uris {
					require.LessOrEqual(t, commaCount(uri)+1, accepted+2, "path %s", uri)
				}
			})
			t.Run("multi-year lookback is refused before listing", func(t *testing.T) {
				rot.widest, cap.uris = 0, nil
				err := query.run(20 * 365 * 24 * time.Hour)
				listed := 0
				for _, uri := range cap.uris {
					listed += commaCount(uri) + 1
				}
				if err == nil || !errors.Is(err, tracestore.ErrInvalidQuery) || rot.widest > maxSpanAge+time.Hour || len(cap.uris) != 0 {
					t.Fatalf("a multi-year metrics lookback must be refused with %v before index names are built: err=%v widest=%s searches=%d names=%d",
						tracestore.ErrInvalidQuery, err, rot.widest, len(cap.uris), listed)
				}
			})
		})
	}
}

func baseMetricsQuery(end time.Time, lookback time.Duration) metricstore.BaseQueryParameters {
	step := time.Minute
	rate := 10 * time.Minute
	return metricstore.BaseQueryParameters{
		ServiceNames: []string{"frontend"},
		EndTime:      &end,
		Lookback:     &lookback,
		Step:         &step,
		RatePer:      &rate,
		SpanKinds:    []string{"server"},
	}
}

type recordingRotation struct {
	inner  indices.Rotation
	widest time.Duration
}

func (r *recordingRotation) ReadTargets(start, end time.Time) []string {
	if d := end.Sub(start); d > r.widest {
		r.widest = d
	}
	return r.inner.ReadTargets(start, end)
}

func (r *recordingRotation) WriteTarget(ts time.Time) string { return r.inner.WriteTarget(ts) }
func (r *recordingRotation) WriteOpType() es.WriteOpType     { return r.inner.WriteOpType() }
func (r *recordingRotation) RequiresDocumentTimestamp() bool {
	return r.inner.RequiresDocumentTimestamp()
}

type searchCapture struct {
	uris []string
}

func (c *searchCapture) searcher(t *testing.T) esclient.Searcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.uris = append(c.uris, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		// An empty date histogram is a successful metrics search with no points.
		_, _ = io.WriteString(w, `{"aggregations":{"results_buckets":{"buckets":[]}}}`)
	}))
	t.Cleanup(srv.Close)
	client, err := esclient.NewClient(context.Background(), &config.Configuration{
		Servers: []string{srv.URL},
		Version: uint(es.ElasticV7),
	}, zap.NewNop(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return esclient.SearchClient{Client: client}
}

func commaCount(s string) int {
	n := 0
	for _, c := range s {
		if c == '?' {
			break
		}
		if c == ',' {
			n++
		}
	}
	return n
}
