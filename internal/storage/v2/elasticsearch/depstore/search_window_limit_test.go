// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package depstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	es "github.com/jaegertracing/jaeger/internal/storage/elasticsearch"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/config"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/esclient"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/indices"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
)

// maxSpanAge is the default configured lookback (Configuration.MaxSpanAge).
// Dependency reads use the same periodic index walk as trace search, so a
// lookback longer than that maximum is a client error and must not be listed.
const maxSpanAge = 72 * time.Hour

func TestGetDependenciesRejectsUncappedLookback(t *testing.T) {
	end := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	raw := indices.NewPeriodicRotation(config.DependencyIndexName, "2006-01-02", 24*time.Hour)
	rot := &recordingRotation{inner: raw}
	cap := &searchCapture{}
	store := NewDependencyStore(Params{
		Searcher:    cap.searcher(t),
		Logger:      zap.NewNop(),
		MaxDocCount: 10_000,
		Rotation:    rot,
	})
	accepted := len(raw.ReadTargets(end.Add(-maxSpanAge), end))
	require.Greater(t, accepted, 0)

	t.Run("lookback at max span age is searched", func(t *testing.T) {
		rot.widest, cap.uris = 0, nil
		_, err := store.GetDependencies(context.Background(), end, maxSpanAge)
		require.NoError(t, err)
		require.LessOrEqual(t, rot.widest, maxSpanAge)
		require.NotEmpty(t, cap.uris)
		for _, uri := range cap.uris {
			require.LessOrEqual(t, stringsCount(uri), accepted, "path %s", uri)
		}
	})

	t.Run("multi-year lookback is refused before listing", func(t *testing.T) {
		rot.widest, cap.uris = 0, nil
		_, err := store.GetDependencies(context.Background(), end, 20*365*24*time.Hour)
		listed := 0
		for _, uri := range cap.uris {
			listed += stringsCount(uri)
		}
		if err == nil || !errors.Is(err, tracestore.ErrInvalidQuery) || rot.widest > maxSpanAge || len(cap.uris) != 0 {
			t.Fatalf("a multi-year dependency lookback must be refused with %v before index names are built: err=%v widest=%s searches=%d names=%d",
				tracestore.ErrInvalidQuery, err, rot.widest, len(cap.uris), listed)
		}
	})
}

func TestGetDependenciesAliasLookbackDoesNotWalkIndices(t *testing.T) {
	end := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	rot := indices.NewAliasedRotation(config.DependencyIndexName+"-write", config.DependencyIndexName+"-read")
	cap := &searchCapture{}
	store := NewDependencyStore(Params{
		Searcher:    cap.searcher(t),
		Logger:      zap.NewNop(),
		MaxDocCount: 10_000,
		Rotation:    rot,
	})

	_, err := store.GetDependencies(context.Background(), end, 20*365*24*time.Hour)
	require.NoError(t, err)
	require.NotEmpty(t, cap.uris)
	for _, uri := range cap.uris {
		require.Contains(t, uri, config.DependencyIndexName+"-read")
		require.NotContains(t, uri, ",")
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
		_, _ = io.WriteString(w, `{"hits":{"hits":[]}}`)
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

func stringsCount(uri string) int {
	n := 1
	for _, c := range uri {
		if c == ',' {
			n++
		}
		if c == '?' {
			break
		}
	}
	return n
}
