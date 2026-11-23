// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package tracestore

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/internal/metrics"
	es "github.com/jaegertracing/jaeger/internal/storage/elasticsearch"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/config"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/esclient"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/indices"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
	"github.com/jaegertracing/jaeger/internal/storage/v2/elasticsearch/tracestore/core"
	"github.com/jaegertracing/jaeger/internal/telemetry/otelsemconv"
)

// maxSpansOneTraceRead is the most span documents one Elasticsearch/OpenSearch
// trace read may request or retain. It is the default max_doc_count, which the
// configuration describes as the maximum number of results to fetch per query.
// GetTraces and FindTraces both page with search_after until hits.total is
// exhausted; a trace larger than this bound must stop with a partial result or
// an error instead of issuing another page.
const maxSpansOneTraceRead = 10_000

// storedSpan is one span document produced by the real writer (spanDocID) and
// later served by the stand-in Elasticsearch _msearch endpoint.
type storedSpan struct {
	id        string
	traceID   string
	spanID    string
	startTime uint64
	source    []byte
}

// pagingSpanServer is a stand-in for Elasticsearch/OpenSearch built from the
// request and response shapes in esclient.Search and esclient.MultiSearch:
// each _msearch body line carries size and search_after, and each item's hits
// carry _source plus hits.total. It does not impose a cap of its own. The
// reader under test is what must stop.
type pagingSpanServer struct {
	spans   []storedSpan
	traceID string
	hits    atomic.Int64
	asked   atomic.Int64
}

func (s *pagingSpanServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch {
	case containsPath(r.URL.Path, "_msearch"):
		s.serveMultiSearch(w, body)
	case containsPath(r.URL.Path, "_search"):
		s.serveTraceIDSearch(w)
	default:
		http.NotFound(w, r)
	}
}

func (s *pagingSpanServer) serveTraceIDSearch(w http.ResponseWriter) {
	payload, err := json.Marshal(map[string]any{
		"aggregations": map[string]any{
			"traceIDs": map[string]any{
				"buckets": []map[string]any{
					{"key": s.traceID, "doc_count": len(s.spans)},
				},
			},
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(payload)
}

func (s *pagingSpanServer) serveMultiSearch(w http.ResponseWriter, body []byte) {
	lines := bytes.Split(body, []byte("\n"))
	var responses []map[string]any
	// MultiSearch writes a header line and a body line per sub-request.
	for i := 0; i+1 < len(lines); i += 2 {
		if len(bytes.TrimSpace(lines[i+1])) == 0 {
			continue
		}
		var req struct {
			Size        int   `json:"size"`
			SearchAfter []any `json:"search_after"`
		}
		if err := json.Unmarshal(lines[i+1], &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.asked.Add(int64(req.Size))
		hits := s.page(req.Size, req.SearchAfter)
		s.hits.Add(int64(len(hits)))
		responses = append(responses, map[string]any{
			"status": http.StatusOK,
			"hits": map[string]any{
				"total": map[string]any{"value": len(s.spans), "relation": "eq"},
				"hits":  hits,
			},
		})
	}
	payload, err := json.Marshal(map[string]any{"responses": responses})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(payload)
}

func (s *pagingSpanServer) page(size int, searchAfter []any) []map[string]any {
	if size <= 0 {
		return []map[string]any{}
	}
	var afterTime uint64
	var afterSpan string
	hasCursor := len(searchAfter) > 0
	if hasCursor {
		afterTime = jsonNumberToUint(searchAfter[0])
		if len(searchAfter) > 1 {
			afterSpan, _ = searchAfter[1].(string)
		}
	}
	hits := make([]map[string]any, 0, size)
	for i := range s.spans {
		sp := &s.spans[i]
		if hasCursor {
			if sp.startTime < afterTime || (sp.startTime == afterTime && sp.spanID <= afterSpan) {
				continue
			}
		}
		hits = append(hits, map[string]any{"_source": json.RawMessage(sp.source)})
		if len(hits) == size {
			break
		}
	}
	return hits
}

func jsonNumberToUint(v any) uint64 {
	switch n := v.(type) {
	case float64:
		return uint64(n)
	case json.Number:
		i, _ := n.Int64()
		return uint64(i)
	default:
		return 0
	}
}

func containsPath(path, part string) bool {
	return len(path) >= len(part) && (path == part || bytes.Contains([]byte(path), []byte(part)))
}

type recordingBatch struct {
	items []esclient.BulkItem
}

func (b *recordingBatch) WriteBatch(_ context.Context, items []esclient.BulkItem) error {
	b.items = append(b.items, items...)
	return nil
}

func TestTraceReadStopsBeforeLoadingEverySpan(t *testing.T) {
	// One more span than the cap, so a reader that pages until hits.total is
	// exhausted has to issue a second search. Page size is the default
	// max_doc_count, the same value production uses as the search window.
	spanCount := maxSpansOneTraceRead + 1
	pageSize := maxSpansOneTraceRead

	traceID := pcommon.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	base := time.Now().UTC().Truncate(time.Microsecond)
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr(otelsemconv.ServiceNameKey, "grow")
	scope := rs.ScopeSpans().AppendEmpty()
	for i := 0; i < spanCount; i++ {
		sp := scope.Spans().AppendEmpty()
		sp.SetTraceID(traceID)
		var spanID pcommon.SpanID
		binary.BigEndian.PutUint64(spanID[:], uint64(i+1))
		sp.SetSpanID(spanID)
		sp.SetName("op")
		start := base.Add(time.Duration(i) * time.Microsecond)
		sp.SetStartTimestamp(pcommon.NewTimestampFromTime(start))
		sp.SetEndTimestamp(pcommon.NewTimestampFromTime(start.Add(time.Microsecond)))
	}

	batch := &recordingBatch{}
	rotation := indices.NewPeriodicRotation("jaeger-span", "2006-01-02", 24*time.Hour)
	serviceRotation := indices.NewPeriodicRotation("jaeger-service", "2006-01-02", 24*time.Hour)
	writer := NewTraceWriter(core.SpanWriterParams{
		BatchWriter:       batch,
		Logger:            zap.NewNop(),
		MetricsFactory:    metrics.NullFactory,
		ServiceOperations: core.NewServiceOperationStorage(nil, zap.NewNop(), time.Hour),
		SpanRotation:      rotation,
		ServiceRotation:   serviceRotation,
	})
	require.NoError(t, writer.WriteTraces(context.Background(), td))

	spans := spanDocuments(t, batch.items)
	require.Len(t, spans, spanCount, "each distinct span id must be stored as its own document")
	ids := make(map[string]struct{}, len(spans))
	for _, sp := range spans {
		ids[sp.id] = struct{}{}
	}
	require.Len(t, ids, spanCount, "span document ids must differ when the span ids differ")
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].startTime != spans[j].startTime {
			return spans[i].startTime < spans[j].startTime
		}
		return spans[i].spanID < spans[j].spanID
	})

	backend := &pagingSpanServer{spans: spans, traceID: spans[0].traceID}
	srv := httptest.NewServer(backend)
	t.Cleanup(srv.Close)

	// The stand-in reads the JSON search body. Compression does not change the
	// body the reader builds; it is off here so the stand-in sees that body.
	cfg := &config.Configuration{
		Servers:          []string{srv.URL},
		Version:          uint(es.ElasticV7),
		HTTPCompression:  false,
		MaxDocCount:      pageSize,
		MaxSpanAge:       72 * time.Hour,
		MaxTraceDuration: 24 * time.Hour,
	}
	client, err := esclient.NewClient(context.Background(), cfg, zap.NewNop(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	searcher := esclient.SearchClient{Client: client}

	// The stand-in must actually page the whole corpus. Otherwise a reader that
	// stops early cannot be distinguished from a stand-in that dropped spans.
	backend.hits.Store(0)
	backend.asked.Store(0)
	require.Equal(t, spanCount, drainSpanPages(t, searcher, pageSize))

	reader := NewTraceReader(core.SpanReaderParams{
		Searcher:         searcher,
		MaxSpanAge:       cfg.MaxSpanAge,
		MaxTraceDuration: cfg.MaxTraceDuration,
		MaxDocCount:      cfg.MaxDocCount,
		Logger:           zap.NewNop(),
		Tracer:           noop.NewTracerProvider().Tracer("test"),
		SpanRotation:     rotation,
		ServiceRotation:  serviceRotation,
	})

	t.Run("GetTraces", func(t *testing.T) {
		assertTraceReadBounded(t, backend, func() (int, error) {
			var n int
			var readErr error
			reader.GetTraces(context.Background(), tracestore.GetTraceParams{TraceID: traceID})(func(traces []ptrace.Traces, err error) bool {
				if err != nil {
					readErr = err
					return false
				}
				for _, tr := range traces {
					n += tr.SpanCount()
				}
				return true
			})
			return n, readErr
		})
	})

	t.Run("FindTraces", func(t *testing.T) {
		assertTraceReadBounded(t, backend, func() (int, error) {
			var n int
			var readErr error
			query := tracestore.TraceQueryParams{
				ServiceName:  "grow",
				Attributes:   pcommon.NewMap(),
				StartTimeMin: base.Add(-time.Hour),
				StartTimeMax: base.Add(time.Hour),
				SearchDepth:  20,
			}
			reader.FindTraces(context.Background(), query)(func(traces []ptrace.Traces, err error) bool {
				if err != nil {
					readErr = err
					return false
				}
				for _, tr := range traces {
					n += tr.SpanCount()
				}
				return true
			})
			return n, readErr
		})
	})
}

func assertTraceReadBounded(t *testing.T, backend *pagingSpanServer, read func() (int, error)) {
	t.Helper()
	backend.hits.Store(0)
	backend.asked.Store(0)
	got, err := read()
	// A page that is served before the read refuses still counts. The read must
	// not ask the backend for more documents than the cap, and it must not keep
	// the ones it received past that cap.
	require.LessOrEqual(t, int(backend.asked.Load()), maxSpansOneTraceRead,
		"trace read requested %d documents from Elasticsearch; the cap is %d", backend.asked.Load(), maxSpansOneTraceRead)
	require.LessOrEqual(t, int(backend.hits.Load()), maxSpansOneTraceRead,
		"trace read accepted %d span documents; the cap is %d", backend.hits.Load(), maxSpansOneTraceRead)
	if err == nil {
		require.LessOrEqual(t, got, maxSpansOneTraceRead,
			"trace read returned %d spans; the cap is %d", got, maxSpansOneTraceRead)
	}
}

func drainSpanPages(t *testing.T, searcher esclient.SearchClient, pageSize int) int {
	t.Helper()
	var after []any
	got := 0
	for {
		resps, err := searcher.MultiSearch(context.Background(), []esclient.MultiSearchRequest{{
			Indices: []string{"jaeger-span"},
			Search: esclient.SearchRequest{
				Size:           pageSize,
				SearchAfter:    after,
				TrackTotalHits: true,
			},
		}})
		require.NoError(t, err)
		require.Len(t, resps, 1)
		require.NoError(t, resps[0].Err())
		hits := resps[0].Hits.Hits
		if len(hits) == 0 {
			break
		}
		got += len(hits)
		var doc struct {
			StartTime uint64 `json:"startTime"`
			SpanID    string `json:"spanID"`
		}
		require.NoError(t, json.Unmarshal(hits[len(hits)-1].Source, &doc))
		after = []any{doc.StartTime, doc.SpanID}
		if got >= resps[0].Hits.Total.Value {
			break
		}
	}
	return got
}

func spanDocuments(t *testing.T, items []esclient.BulkItem) []storedSpan {
	t.Helper()
	spans := make([]storedSpan, 0, len(items))
	for _, item := range items {
		raw, ok := item.Body.(json.RawMessage)
		if !ok {
			continue
		}
		var doc struct {
			TraceID   string `json:"traceID"`
			SpanID    string `json:"spanID"`
			StartTime uint64 `json:"startTime"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil || doc.TraceID == "" || doc.SpanID == "" {
			continue
		}
		spans = append(spans, storedSpan{
			id:        item.ID,
			traceID:   doc.TraceID,
			spanID:    doc.SpanID,
			startTime: doc.StartTime,
			source:    append([]byte(nil), raw...),
		})
	}
	return spans
}
