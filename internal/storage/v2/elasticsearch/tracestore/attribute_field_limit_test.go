// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package tracestore

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/internal/jptrace/sanitizer"
	"github.com/jaegertracing/jaeger/internal/metricstest"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/config"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/esclient"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/indices"
	"github.com/jaegertracing/jaeger/internal/storage/v2/elasticsearch/tracestore/core"
)

// promotedFieldCap is the most distinct span and resource attribute keys, combined,
// that may be stored as fields of one span index when every attribute is promoted.
// Elasticsearch 7.17.22, 8.19.0, and 9.5.3 default index.mapping.total_fields.limit
// to 1000 and reject the whole document that would add one more field. The static
// span mapping already spends part of that budget, and numeric_attributes adds a
// second mapped field per promoted key, so the keys themselves have to stay well
// under 1000.
const promotedFieldCap = 256

// recordingBulk is the batch writer the span writer actually calls. It keeps every
// document handed to it so a test can see a field that was indexed, not merely a
// field the writer considered.
type recordingBulk struct {
	mu    sync.Mutex
	items []esclient.BulkItem
}

func (r *recordingBulk) WriteBatch(_ context.Context, items []esclient.BulkItem) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, items...)
	return nil
}

func (r *recordingBulk) snapshot() []esclient.BulkItem {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]esclient.BulkItem, len(r.items))
	copy(out, r.items)
	return out
}

func newPromotingWriter(bulk esclient.BatchWriter) *TraceWriter {
	logger := zap.NewNop()
	return NewTraceWriter(core.SpanWriterParams{
		BatchWriter:       bulk,
		Logger:            logger,
		MetricsFactory:    metricstest.NewFactory(0),
		ServiceOperations: core.NewServiceOperationStorage(nil, logger, time.Hour),
		AllTagsAsFields:   true,
		TagDotReplacement: "@",
		SpanRotation:      indices.NewPeriodicRotation(config.SpanIndexName, "2006-01-02", 24*time.Hour),
		ServiceRotation:   indices.NewPeriodicRotation(config.ServiceIndexName, "2006-01-02", 24*time.Hour),
	})
}

// writeSanitized runs the storage exporter's sanitizer and then the Elasticsearch
// trace writer, which is the path a span takes after a receiver accepts it.
func writeSanitized(w *TraceWriter, td ptrace.Traces) error {
	return w.WriteTraces(context.Background(), sanitizer.Sanitize(td))
}

// traceWithAttributes builds one span. spanKeys and resourceKeys are attribute
// keys chosen by the sender. service.name is set so the empty-service sanitizer
// has nothing to rewrite. n distinguishes the span from others in the same test.
func traceWithAttributes(n int, operation string, spanKeys, resourceKeys []string) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	attrs := rs.Resource().Attributes()
	attrs.PutStr("service.name", "attacker-service")
	for _, key := range resourceKeys {
		attrs.PutStr(key, "resource-value")
	}
	span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	var traceID [16]byte
	var spanID [8]byte
	binary.BigEndian.PutUint64(traceID[8:], uint64(n+1))
	binary.BigEndian.PutUint64(spanID[:], uint64(n+1))
	span.SetTraceID(traceID)
	span.SetSpanID(spanID)
	span.SetName(operation)
	for _, key := range spanKeys {
		span.Attributes().PutStr(key, "span-value")
	}
	return td
}

func itemObject(t *testing.T, item esclient.BulkItem) map[string]any {
	t.Helper()
	var raw []byte
	switch body := item.Body.(type) {
	case json.RawMessage:
		raw = body
	case []byte:
		raw = body
	default:
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		raw = encoded
	}
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	return doc
}

// promotedKeys returns every field name the writer placed under tag or process.tag.
// Those are the names the span index template maps dynamically, one new field each.
func promotedKeys(t *testing.T, items []esclient.BulkItem) map[string]struct{} {
	t.Helper()
	keys := map[string]struct{}{}
	for _, item := range items {
		doc := itemObject(t, item)
		collectKeys(keys, doc["tag"])
		if process, ok := doc["process"].(map[string]any); ok {
			collectKeys(keys, process["tag"])
		}
	}
	return keys
}

func collectKeys(dst map[string]struct{}, value any) {
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	for key := range object {
		dst[key] = struct{}{}
	}
}

func operationNames(t *testing.T, items []esclient.BulkItem) map[string]struct{} {
	t.Helper()
	names := map[string]struct{}{}
	for _, item := range items {
		doc := itemObject(t, item)
		name, ok := doc["operationName"].(string)
		if ok && name != "" {
			names[name] = struct{}{}
		}
	}
	return names
}

// TestPromotedAttributeKeysStayWithinFieldCap drives attacker-chosen span and
// resource attribute keys through the sanitizer and the trace writer with
// tags-as-fields all enabled. Keys past the cap must not be handed to the bulk
// writer, and a span that would introduce one must be rejected rather than indexed
// with that field removed. A span that only repeats an already-accepted key (or
// carries no extra key, when none were accepted) is still indexed.
func TestPromotedAttributeKeysStayWithinFieldCap(t *testing.T) {
	bulk := &recordingBulk{}
	writer := newPromotingWriter(bulk)

	const flood = promotedFieldCap + 144
	var failures int
	for i := 0; i < flood; i++ {
		key := fmt.Sprintf("attr-%04d", i)
		var td ptrace.Traces
		if i%2 == 0 {
			td = traceWithAttributes(i, "flood", []string{key}, nil)
		} else {
			td = traceWithAttributes(i, "flood", nil, []string{key})
		}
		if err := writeSanitized(writer, td); err != nil {
			failures++
		}
	}

	keys := promotedKeys(t, bulk.snapshot())
	assert.LessOrEqualf(t, len(keys), promotedFieldCap,
		"distinct promoted attribute keys = %d, cap = %d", len(keys), promotedFieldCap)
	assert.NotZero(t, failures, "a key past the cap must be rejected, not indexed")

	overflow := traceWithAttributes(flood+1, "rejected-span", []string{"not-yet-seen"}, nil)
	require.Error(t, writeSanitized(writer, overflow), "a new attribute key past the cap must fail the write")
	assert.NotContains(t, promotedKeys(t, bulk.snapshot()), "not-yet-seen")
	assert.NotContains(t, operationNames(t, bulk.snapshot()), "rejected-span",
		"the span that would add a field was indexed")

	var repeat ptrace.Traces
	if len(keys) == 0 {
		repeat = traceWithAttributes(flood+2, "accepted-again", nil, nil)
	} else {
		var accepted string
		for key := range keys {
			accepted = key
			break
		}
		repeat = traceWithAttributes(flood+2, "accepted-again", []string{accepted}, nil)
	}
	require.NoError(t, writeSanitized(writer, repeat), "a span that adds no new field must still be indexed")
	assert.Contains(t, operationNames(t, bulk.snapshot()), "accepted-again")

	mixedKeys := []string{"also-new"}
	if len(keys) > 0 {
		for key := range keys {
			mixedKeys = append(mixedKeys, key)
			break
		}
	}
	mixed := traceWithAttributes(flood+3, "mixed-rejected", mixedKeys, nil)
	require.Error(t, writeSanitized(writer, mixed))
	after := bulk.snapshot()
	assert.NotContains(t, promotedKeys(t, after), "also-new")
	assert.NotContains(t, operationNames(t, after), "mixed-rejected",
		"a span that mixes an accepted key with a new key must not be indexed at all")
}

// TestOneSpanCannotIntroduceMoreFieldsThanTheCap rejects a single span whose
// attribute keys would cross the cap, and requires that none of those keys are
// indexed. Truncating the document down to the cap still poisons the mapping.
func TestOneSpanCannotIntroduceMoreFieldsThanTheCap(t *testing.T) {
	bulk := &recordingBulk{}
	writer := newPromotingWriter(bulk)

	keys := make([]string, promotedFieldCap+1)
	for i := range keys {
		keys[i] = fmt.Sprintf("one-span-%04d", i)
	}
	err := writeSanitized(writer, traceWithAttributes(1, "too-many-keys", keys, nil))
	require.Error(t, err)
	got := promotedKeys(t, bulk.snapshot())
	assert.Empty(t, got, "over-cap span was indexed with keys %v", got)
	assert.NotContains(t, operationNames(t, bulk.snapshot()), "too-many-keys")
}

// TestConcurrentPromotionCannotExceedFieldCap sends distinct keys in parallel
// through one writer. The documents that were actually handed to the bulk writer
// must stay inside the cap even if two calls both observe spare capacity before
// either records the key it is about to write. A call that returns an error must
// not have indexed its key.
func TestConcurrentPromotionCannotExceedFieldCap(t *testing.T) {
	bulk := &recordingBulk{}
	writer := newPromotingWriter(bulk)

	const n = promotedFieldCap + 144
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("concurrent-%04d", i)
			errs[i] = writeSanitized(writer, traceWithAttributes(10_000+i, "concurrent", []string{key}, nil))
		}()
	}
	wg.Wait()

	keys := promotedKeys(t, bulk.snapshot())
	assert.LessOrEqualf(t, len(keys), promotedFieldCap,
		"concurrent writes promoted %d keys, cap = %d", len(keys), promotedFieldCap)

	var rejected int
	for i, err := range errs {
		key := fmt.Sprintf("concurrent-%04d", i)
		_, indexed := keys[key]
		if err != nil {
			rejected++
			assert.Falsef(t, indexed, "key %s was indexed and the write still returned %v", key, err)
			continue
		}
		assert.Truef(t, indexed, "key %s was reported as indexed but is absent from the bulk documents", key)
	}
	assert.NotZero(t, rejected, "keys past the cap must be rejected under concurrent writers")
}
