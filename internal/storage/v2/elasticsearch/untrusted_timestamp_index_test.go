// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package elasticsearch

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	jaegerthrift "github.com/jaegertracing/jaeger-idl/thrift-gen/jaeger"
	"github.com/jaegertracing/jaeger/internal/jptrace/sanitizer"
	"github.com/jaegertracing/jaeger/internal/metrics"
	es "github.com/jaegertracing/jaeger/internal/storage/elasticsearch"
	escfg "github.com/jaegertracing/jaeger/internal/storage/elasticsearch/config"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/esclient"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/indices"
	"github.com/jaegertracing/jaeger/internal/storage/v2/elasticsearch/tracestore"
	"github.com/jaegertracing/jaeger/internal/storage/v2/elasticsearch/tracestore/core"
	storagegrpc "github.com/jaegertracing/jaeger/internal/storage/v2/grpc"
	jaegertranslator "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/translator/jaeger"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/translator/zipkin/zipkinv1"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/translator/zipkin/zipkinv2"
)

// acceptedClockSkew is the clock-skew allowance around now. The remediation is
// to reject or clamp a span start to a window of about max_span_age plus a
// small skew. One hour is enough for either choice; a fix that uses a smaller
// skew still passes because the out-of-window fixtures sit days away.
const acceptedClockSkew = time.Hour

// TestUntrustedSpanTimeDoesNotOpenPeriodicIndex is the spec for the periodic
// index write. It fails while a client-supplied span start time is copied into
// the Elasticsearch bulk _index name.
//
// A fix passes when every _bulk body that the writer actually sends names only
// indices whose period overlaps [now-max_span_age-skew, now+skew], or sends
// nothing for an out-of-window span (the span was rejected first). Clamping
// the timestamp onto a period inside that window is also accepted. A span
// thirty minutes old must still be written. The check is on the bytes the
// HTTP server received, so a refusal that happens after the bulk request was
// sent does not pass.
func TestUntrustedSpanTimeDoesNotOpenPeriodicIndex(t *testing.T) {
	now := time.Now().UTC()
	maxAge := DefaultConfig().MaxSpanAge
	require.Greater(t, maxAge, time.Hour, "the default max span age is the window these tests measure against")

	tests := []struct {
		name      string
		ingress   string
		hourly    bool
		async     bool
		sanitize  bool
		useExport bool
	}{
		{name: "otlp-json/daily/async/sanitized", ingress: "otlp-json", async: true, sanitize: true},
		{name: "otlp-proto/daily/sync/sanitized", ingress: "otlp-proto", async: false, sanitize: true},
		{name: "jaeger-proto/daily/async/sanitized", ingress: "jaeger-proto", async: true, sanitize: true},
		{name: "jaeger-thrift/daily/async/sanitized", ingress: "jaeger-thrift", async: true, sanitize: true},
		{name: "zipkin-v2/daily/async/sanitized", ingress: "zipkin-v2", async: true, sanitize: true},
		{name: "zipkin-v1/daily/async/sanitized", ingress: "zipkin-v1", async: true, sanitize: true},
		{name: "grpc-export/daily/async", ingress: "otlp-proto", async: true, useExport: true},
		{name: "otlp-json/hourly/async/sanitized", ingress: "otlp-json", hourly: true, async: true, sanitize: true},
		{name: "otlp-proto/daily/async/direct-writer", ingress: "otlp-proto", async: true},
		{name: "otlp-json/hourly/sync/direct-writer", ingress: "otlp-json", hourly: true, async: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			if tc.hourly {
				setHourlyRotation(&cfg)
			}
			layout := periodicLayout(cfg.ResolvedSpanRotation())
			require.Equal(t, layout, periodicLayout(cfg.ResolvedServiceRotation()))
			if tc.hourly {
				require.Contains(t, layout, "15", "hourly rotation must use an hour in the index date")
			}

			recorded := newBulkCapture()
			srv := httptest.NewServer(recorded)
			t.Cleanup(srv.Close)

			recent, far := attackTimestamps(now, maxAge)
			// One far span has a negative duration. The standard sanitizer
			// rewrites the end, and the start is what selects the index.
			negative := far[0].Add(-time.Second)

			farTraces := tracesFromIngress(t, tc.ingress, "checkout", far, &negative)
			recentTraces := tracesFromIngress(t, tc.ingress, "checkout", []time.Time{recent}, nil)

			err := writeProductPath(t, srv.URL, cfg, tc.async, tc.sanitize, tc.useExport, recentTraces)
			got := recorded.drain()
			require.NoError(t, err)
			require.NotEmpty(t, got, "a span started thirty minutes ago must still be bulk-indexed")
			require.Empty(t, outsideWindow(got, layout, now, maxAge))

			err = writeProductPath(t, srv.URL, cfg, tc.async, tc.sanitize, tc.useExport, farTraces)
			bad := outsideWindow(recorded.drain(), layout, now, maxAge)
			require.Emptyf(t, bad, "out-of-window span starts were written to periodic indices (write error: %v)", err)
		})
	}
}

// attackTimestamps returns one in-window start and many starts that fall on
// distinct days (or hours, once formatted) outside max_span_age.
func attackTimestamps(now time.Time, maxAge time.Duration) (recent time.Time, far []time.Time) {
	recent = now.Add(-30 * time.Minute)
	far = []time.Time{
		now.Add(-(maxAge + 48*time.Hour)),
		now.Add(7 * 24 * time.Hour),
		time.Date(1999, 6, 15, 3, 4, 5, 0, time.UTC),
		time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC),
		time.Unix(0, 0).UTC(),
	}
	// One hundred further days, each its own UTC date, all older than the window.
	for i := range 100 {
		far = append(far, now.Add(-time.Duration(30+i)*24*time.Hour).UTC())
	}
	return recent, far
}

func setHourlyRotation(cfg *escfg.Configuration) {
	hourly := configoptional.Some(escfg.PeriodicRotation{
		DateLayout:        "2006-01-02-15",
		RolloverFrequency: "hour",
	})
	cfg.Indices.Spans.Rotation.Periodic = hourly
	cfg.Indices.Services.Rotation.Periodic = hourly
	cfg.Indices.Dependencies.Rotation.Periodic = hourly
	cfg.Indices.Sampling.Rotation.Periodic = hourly
}

func periodicLayout(rc escfg.RotationConfig) string {
	p := rc.Periodic.Get()
	if p == nil || p.DateLayout == "" {
		return "2006-01-02"
	}
	return p.DateLayout
}

// tracesFromIngress builds the pdata batch the collector hands to storage for
// this receiver. endBefore, when set, is the end timestamp of the first span
// and is earlier than that span's start.
func tracesFromIngress(t *testing.T, ingress, service string, starts []time.Time, endBefore *time.Time) ptrace.Traces {
	t.Helper()
	switch ingress {
	case "otlp-json":
		return roundTripOTLP(t, false, otlpTraces(service, starts, endBefore))
	case "otlp-proto":
		return roundTripOTLP(t, true, otlpTraces(service, starts, endBefore))
	case "jaeger-proto":
		td, err := jaegertranslator.ProtoToTraces([]*model.Batch{jaegerBatch(service, starts, endBefore)})
		require.NoError(t, err)
		return td
	case "jaeger-thrift":
		td, err := jaegertranslator.ThriftToTraces(jaegerThriftBatch(service, starts, endBefore))
		require.NoError(t, err)
		return td
	case "zipkin-v2":
		raw := zipkinV2JSON(t, service, starts)
		td, err := zipkinv2.NewJSONTracesUnmarshaler(false).UnmarshalTraces(raw)
		require.NoError(t, err)
		return td
	case "zipkin-v1":
		raw := zipkinV1JSON(t, service, starts)
		td, err := zipkinv1.NewJSONTracesUnmarshaler(false).UnmarshalTraces(raw)
		require.NoError(t, err)
		return td
	default:
		t.Fatalf("unknown ingress %q", ingress)
		return ptrace.Traces{}
	}
}

func roundTripOTLP(t *testing.T, proto bool, td ptrace.Traces) ptrace.Traces {
	t.Helper()
	req := ptraceotlp.NewExportRequestFromTraces(td)
	var err error
	var body []byte
	if proto {
		body, err = req.MarshalProto()
	} else {
		body, err = req.MarshalJSON()
	}
	require.NoError(t, err)
	decoded := ptraceotlp.NewExportRequest()
	if proto {
		err = decoded.UnmarshalProto(body)
	} else {
		err = decoded.UnmarshalJSON(body)
	}
	require.NoError(t, err)
	return decoded.Traces()
}

func otlpTraces(service string, starts []time.Time, endBefore *time.Time) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", service)
	ss := rs.ScopeSpans().AppendEmpty()
	for i, start := range starts {
		span := ss.Spans().AppendEmpty()
		span.SetName("op")
		span.SetTraceID(traceID(i + 1))
		span.SetSpanID(spanID(i + 1))
		span.SetStartTimestamp(pcommon.NewTimestampFromTime(start))
		end := start.Add(time.Millisecond)
		if i == 0 && endBefore != nil {
			end = *endBefore
		}
		span.SetEndTimestamp(pcommon.NewTimestampFromTime(end))
	}
	return td
}

func traceID(n int) pcommon.TraceID {
	var id pcommon.TraceID
	id[15] = byte(n)
	id[14] = byte(n >> 8)
	id[13] = byte(n >> 16)
	id[12] = byte(n >> 24)
	return id
}

func spanID(n int) pcommon.SpanID {
	var id pcommon.SpanID
	id[7] = byte(n)
	id[6] = byte(n >> 8)
	id[5] = byte(n >> 16)
	id[4] = byte(n >> 24)
	return id
}

func jaegerBatch(service string, starts []time.Time, endBefore *time.Time) *model.Batch {
	spans := make([]*model.Span, len(starts))
	for i, start := range starts {
		dur := time.Millisecond
		if i == 0 && endBefore != nil {
			dur = endBefore.Sub(start)
		}
		spans[i] = &model.Span{
			TraceID:       model.NewTraceID(0, uint64(i+1)),
			SpanID:        model.NewSpanID(uint64(i + 1)),
			OperationName: "op",
			StartTime:     start,
			Duration:      dur,
		}
	}
	return &model.Batch{
		Process: &model.Process{ServiceName: service},
		Spans:   spans,
	}
}

func jaegerThriftBatch(service string, starts []time.Time, endBefore *time.Time) *jaegerthrift.Batch {
	spans := make([]*jaegerthrift.Span, len(starts))
	for i, start := range starts {
		dur := int64(time.Millisecond / time.Microsecond)
		if i == 0 && endBefore != nil {
			dur = endBefore.Sub(start).Microseconds()
		}
		spans[i] = &jaegerthrift.Span{
			TraceIdLow:    int64(i + 1),
			SpanId:        int64(i + 1),
			OperationName: "op",
			StartTime:     start.UnixMicro(),
			Duration:      dur,
		}
	}
	return &jaegerthrift.Batch{
		Process: &jaegerthrift.Process{ServiceName: service},
		Spans:   spans,
	}
}

func zipkinV2JSON(t *testing.T, service string, starts []time.Time) []byte {
	t.Helper()
	spans := make([]map[string]any, len(starts))
	for i, start := range starts {
		spans[i] = map[string]any{
			"traceId":   hexTrace(i + 1),
			"id":        hexSpan(i + 1),
			"name":      "op",
			"timestamp": start.UnixMicro(),
			"duration":  int64(time.Millisecond / time.Microsecond),
			"localEndpoint": map[string]any{
				"serviceName": service,
			},
		}
	}
	raw, err := json.Marshal(spans)
	require.NoError(t, err)
	return raw
}

func zipkinV1JSON(t *testing.T, service string, starts []time.Time) []byte {
	t.Helper()
	spans := make([]map[string]any, len(starts))
	for i, start := range starts {
		spans[i] = map[string]any{
			"traceId":   hexTrace(i + 1),
			"id":        hexSpan(i + 1),
			"name":      "op",
			"timestamp": start.UnixMicro(),
			"duration":  int64(time.Millisecond / time.Microsecond),
			"annotations": []map[string]any{{
				"timestamp": start.UnixMicro(),
				"value":     "cs",
				"endpoint": map[string]any{
					"serviceName": service,
					"ipv4":        "127.0.0.1",
				},
			}},
		}
	}
	raw, err := json.Marshal(spans)
	require.NoError(t, err)
	return raw
}

func hexTrace(n int) string {
	return "0000000000000000" + hexSpan(n)
}

func hexSpan(n int) string {
	return strings.Repeat("0", 8) + leftPadHex(uint64(n), 8)
}

func leftPadHex(n uint64, width int) string {
	s := strconv.FormatUint(n, 16)
	if len(s) >= width {
		return s
	}
	return strings.Repeat("0", width-len(s)) + s
}

// writeProductPath sends td through the Elasticsearch trace writer. sanitize
// matches storageexporter.TraceWriter, which runs sanitizer.Sanitize before
// WriteTraces. useExport matches the remote-storage gRPC Export handler, which
// sanitizes and then calls the same writer. The direct path is QueryService
// archive writes, which call WriteTraces with the trace they already stored.
func writeProductPath(t *testing.T, serverURL string, cfg escfg.Configuration, async, sanitize, useExport bool, td ptrace.Traces) error {
	t.Helper()
	logger := zap.NewNop()
	client, err := esclient.NewClient(context.Background(), &escfg.Configuration{
		Servers:         []string{serverURL},
		Version:         uint(es.ElasticV8),
		HTTPCompression: true,
	}, logger, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	var batch esclient.BatchWriter
	var flush func() error
	if async {
		indexer, err := esclient.NewBulkIndexer(client, esclient.BulkIndexerConfig{
			FlushBytes:    5_000_000,
			FlushInterval: time.Hour,
			Workers:       1,
		}, metrics.NullFactory, logger)
		require.NoError(t, err)
		batch = indexer
		flush = indexer.Close
	} else {
		batch = esclient.NewSyncBulkWriter(client, 5_000_000, false, metrics.NullFactory, logger)
		flush = func() error { return nil }
	}

	writer := tracestore.NewTraceWriter(core.SpanWriterParams{
		BatchWriter:       batch,
		Logger:            logger,
		MetricsFactory:    metrics.NullFactory,
		ServiceOperations: core.NewServiceOperationStorage(nil, logger, time.Hour),
		SpanRotation: indices.BuildRotation(
			cfg.Indices.IndexPrefix,
			escfg.SpanIndexName,
			cfg.ResolvedSpanRotation(),
			nil,
			logger,
		),
		ServiceRotation: indices.BuildRotation(
			cfg.Indices.IndexPrefix,
			escfg.ServiceIndexName,
			cfg.ResolvedServiceRotation(),
			nil,
			logger,
		),
	})

	ctx := context.Background()
	var writeErr error
	switch {
	case useExport:
		handler := storagegrpc.NewHandler(nil, writer, nil)
		_, writeErr = handler.Export(ctx, ptraceotlp.NewExportRequestFromTraces(td))
	case sanitize:
		// storageexporter.TraceWriter.WriteTraces does sanitizer.Sanitize then storage.WriteTraces.
		writeErr = writer.WriteTraces(ctx, sanitizer.Sanitize(td))
	default:
		writeErr = writer.WriteTraces(ctx, td)
	}
	return errors.Join(writeErr, flush())
}

type bulkCapture struct {
	indices []string
}

func newBulkCapture() *bulkCapture {
	return &bulkCapture{}
}

func (c *bulkCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, zerr := gzip.NewReader(bytes.NewReader(body))
		if zerr != nil {
			http.Error(w, zerr.Error(), http.StatusBadRequest)
			return
		}
		body, err = io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	names := indexNamesFromBulk(body)
	c.indices = append(c.indices, names...)

	items := make([]string, len(names))
	for i := range items {
		items[i] = `{"index":{"status":201}}`
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"errors":false,"took":1,"items":[` + strings.Join(items, ",") + `]}`))
}

func (c *bulkCapture) drain() []string {
	out := c.indices
	c.indices = nil
	return out
}

func indexNamesFromBulk(body []byte) []string {
	var lines [][]byte
	for _, line := range bytes.Split(body, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			lines = append(lines, line)
		}
	}
	var names []string
	for i := 0; i+1 < len(lines); i += 2 {
		var meta map[string]struct {
			Index string `json:"_index"`
		}
		if err := json.Unmarshal(lines[i], &meta); err != nil {
			continue
		}
		for _, action := range meta {
			if action.Index != "" {
				names = append(names, action.Index)
			}
		}
	}
	return names
}

// outsideWindow returns index names whose trailing period does not overlap
// [now-maxAge-acceptedClockSkew, now+acceptedClockSkew]. A name with no
// periodic suffix (a write alias or data stream) is not a new date index.
func outsideWindow(indices []string, layout string, now time.Time, maxAge time.Duration) []string {
	windowStart := now.Add(-(maxAge + acceptedClockSkew))
	windowEnd := now.Add(acceptedClockSkew)
	period := 24 * time.Hour
	if strings.Contains(layout, "15") {
		period = time.Hour
	}
	suffixLen := len(now.UTC().Format(layout))
	var bad []string
	for _, name := range indices {
		if len(name) <= suffixLen || name[len(name)-suffixLen-1] != '-' {
			continue
		}
		suffix := name[len(name)-suffixLen:]
		start, err := time.ParseInLocation(layout, suffix, time.UTC)
		if err != nil {
			continue
		}
		periodEnd := start.Add(period)
		if !periodEnd.After(windowStart) || !start.Before(windowEnd) {
			bad = append(bad, name)
		}
	}
	return bad
}
