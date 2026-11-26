// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	es "github.com/jaegertracing/jaeger/internal/storage/elasticsearch"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/config"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/esclient"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/indices"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
	"github.com/jaegertracing/jaeger/internal/storage/v2/elasticsearch/tracestore/core/dbmodel"
)

// maxSpanAge is Configuration.MaxSpanAge from DefaultConfig: the configured
// maximum lookback on span reads. A periodic search may list the indices inside
// that window and must refuse anything longer before it asks the rotation to
// walk the range. Alias rotation returns one name for any window.
const maxSpanAge = 72 * time.Hour

func TestPeriodicSearchesRejectUncappedWindows(t *testing.T) {
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
			raw := indices.NewPeriodicRotation(config.SpanIndexName, tc.layout, tc.step)
			rot := &recordingRotation{inner: raw}
			cap := &searchCapture{}
			reader := newWindowReader(t, rot, cap.searcher(t))
			accepted := len(raw.ReadTargets(start, start.Add(maxSpanAge)))
			require.Greater(t, accepted, 0)

			t.Run("window at max span age is searched", func(t *testing.T) {
				rot.widest, cap.uris = 0, nil
				err := searchAll(t, reader, start, start.Add(maxSpanAge))
				require.NoError(t, err)
				require.LessOrEqual(t, rot.widest, maxSpanAge, "ReadTargets was asked for %s", rot.widest)
				require.NotEmpty(t, cap.uris)
				for _, uri := range cap.uris {
					require.LessOrEqualf(t, len(indexNames(uri)), accepted,
						"search path listed %d indices: %s", len(indexNames(uri)), uri)
				}
			})

			t.Run("window past max span age is refused before listing", func(t *testing.T) {
				rot.widest, cap.uris = 0, nil
				err := searchAll(t, reader, start, start.Add(tc.window))
				listed := 0
				for _, uri := range cap.uris {
					listed += len(indexNames(uri))
				}
				if err == nil || !errors.Is(err, tracestore.ErrInvalidQuery) || rot.widest > maxSpanAge || len(cap.uris) != 0 {
					t.Fatalf("a %s window must be refused with %v before index names are built: err=%v widest=%s searches=%d names=%d",
						tc.window, tracestore.ErrInvalidQuery, err, rot.widest, len(cap.uris), listed)
				}
			})
		})
	}
}

func TestAliasedSearchKeepsASingleIndexForAnyWindow(t *testing.T) {
	// Year 1 is time.Time's zero value, which the reader rejects as an unset bound.
	start := time.Date(2, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
	rot := indices.NewAliasedRotation(config.SpanIndexName+"-write", config.SpanIndexName+"-read")
	cap := &searchCapture{}
	reader := newWindowReader(t, rot, cap.searcher(t))

	err := searchAll(t, reader, start, end)
	require.NoError(t, err)
	require.NotEmpty(t, cap.uris)
	for _, uri := range cap.uris {
		require.Equal(t, []string{config.SpanIndexName + "-read"}, indexNames(uri), "path %s", uri)
	}
}

func searchAll(t *testing.T, reader *SpanReader, start, end time.Time) error {
	t.Helper()
	ctx := context.Background()
	_, tracesErr := reader.FindTraceIDs(ctx, dbmodel.TraceQueryParameters{
		ServiceName:  "frontend",
		StartTimeMin: start,
		StartTimeMax: end,
		SearchDepth:  20,
	})
	_, spansErr := reader.FindSpans(ctx, dbmodel.SpanQueryParameters{
		StartTimeMin: start,
		StartTimeMax: end,
		PageSize:     20,
	})
	_, summariesErr := reader.FindTraceSummaries(ctx, dbmodel.TraceQueryParameters{
		ServiceName:  "frontend",
		StartTimeMin: start,
		StartTimeMax: end,
		SearchDepth:  20,
	})
	return errors.Join(tracesErr, spansErr, summariesErr)
}

func newWindowReader(t *testing.T, rotation indices.Rotation, searcher esclient.Searcher) *SpanReader {
	t.Helper()
	return NewSpanReader(SpanReaderParams{
		Searcher:            searcher,
		MaxSpanAge:          maxSpanAge,
		ServicesMaxLookback: maxSpanAge,
		MaxTraceDuration:    24 * time.Hour,
		MaxDocCount:         10_000,
		TagDotReplacement:   "@",
		Logger:              zap.NewNop(),
		Tracer:              noop.NewTracerProvider().Tracer("test"),
		SpanRotation:        rotation,
		ServiceRotation:     rotation,
	})
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

func (r *recordingRotation) WriteTarget(spanTime time.Time) string {
	return r.inner.WriteTarget(spanTime)
}

func (r *recordingRotation) WriteOpType() es.WriteOpType { return r.inner.WriteOpType() }

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
		_, _ = io.WriteString(w, `{}`)
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

func indexNames(uri string) []string {
	path, _, _ := strings.Cut(uri, "?")
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/_search")
	if path == "" {
		return nil
	}
	return strings.Split(path, ",")
}
