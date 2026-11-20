// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package adaptive

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger/internal/hostname"
	epmocks "github.com/jaegertracing/jaeger/internal/leaderelection/mocks"
	"github.com/jaegertracing/jaeger/internal/metricstest"
	"github.com/jaegertracing/jaeger/internal/sampling/samplingstrategy"
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/samplingstore"
	samplingmodel "github.com/jaegertracing/jaeger/internal/storage/v1/api/samplingstore/model"
	memstore "github.com/jaegertracing/jaeger/internal/storage/v2/memory"
)

// Bounds the adaptive sampling aggregator must enforce. A later fix has to
// keep at most this many service/operation pairs in an aggregation interval,
// and at most this many bytes of each service or operation name. Pairs and
// name bytes beyond the bounds are dropped or folded into one overflow
// entry whose name is within the length bound. The same bounds apply to
// throughput written to storage and to buckets rebuilt from storage.
const (
	maxAdaptiveSamplingOperations  = 1024
	maxAdaptiveSamplingNameLength  = 256
	adaptiveSamplingAttackMultiple = 3
)

func TestHandleRootSpanRetainsBoundedOperationNames(t *testing.T) {
	agg := newCardinalityAggregator(t, memstore.NewSamplingStore(2))
	recordAttackRootSpans(agg)

	names := throughputNames(agg.(*aggregator).currentThroughput)
	assertNameBounds(t, names)
}

func TestHandleRootSpanKeepsOperationsBelowTheCap(t *testing.T) {
	agg := newCardinalityAggregator(t, memstore.NewSamplingStore(2))
	for _, op := range []string{"checkout", "refund"} {
		agg.HandleRootSpan(rootSpan("payments", op))
	}
	// A child span and a root span without sampler tags are not throughput.
	childTraceID := model.NewTraceID(1, 1)
	agg.HandleRootSpan(&model.Span{
		TraceID:       childTraceID,
		OperationName: "child-op",
		Process:       &model.Process{ServiceName: "payments"},
		References: []model.SpanRef{{
			RefType: model.ChildOf,
			TraceID: childTraceID,
			SpanID:  model.NewSpanID(9),
		}},
		Tags: samplerTags(),
	})
	agg.HandleRootSpan(&model.Span{
		OperationName: "missing-sampler",
		Process:       &model.Process{ServiceName: "payments"},
	})

	got := agg.(*aggregator).currentThroughput
	require.Len(t, got, 1)
	require.Len(t, got["payments"], 2)
	require.EqualValues(t, 1, got["payments"]["checkout"].Count)
	require.EqualValues(t, 1, got["payments"]["refund"].Count)
}

func TestSaveThroughputWritesBoundedNames(t *testing.T) {
	store := memstore.NewSamplingStore(2)
	agg := newCardinalityAggregator(t, store)
	recordAttackRootSpans(agg)

	agg.(*aggregator).saveThroughput()

	stored, err := store.GetThroughput(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assertStoredBounds(t, stored)
}

func TestAggregateThroughputBoundsReloadedNames(t *testing.T) {
	// Storage (or another collector) can already hold one record per attacker
	// name. Reloading that slice must not rebuild an unbounded map, and must
	// not keep a name longer than the length bound.
	input := attackThroughputRecords()
	p, err := newPostAggregator(DefaultOptions(), "host", memstore.NewSamplingStore(2), &epmocks.ElectionParticipant{}, metricstest.NewFactory(0), zap.NewNop())
	require.NoError(t, err)

	aggregated := p.aggregateThroughput(input)
	assertNameBounds(t, throughputNames(aggregated))

	p.prependThroughputBucket(&throughputBucket{
		throughput: aggregated,
		interval:   time.Minute,
		endTime:    time.Now(),
	})
	probabilities, qps := p.calculateProbabilitiesAndQPS()
	assertNameBounds(t, probabilityNames(probabilities))
	assertNameBounds(t, probabilityNames(qps))
	assertNameBounds(t, cacheNames(p.serviceCache[0]))
}

func TestAggregationLoopKeepsBoundedBuckets(t *testing.T) {
	// Warm the hostname identifier outside the fake clock. It reads the
	// host name and a random suffix once per process.
	_, err := hostname.AsIdentifier()
	require.NoError(t, err)

	synctest.Test(t, func(t *testing.T) {
		store := memstore.NewSamplingStore(2)
		participant := &epmocks.ElectionParticipant{}
		participant.On("IsLeader").Return(true)
		opts := cardinalityOptions()
		agg, err := NewAggregator(opts, zap.NewNop(), metricstest.NewFactory(0), participant, store)
		require.NoError(t, err)
		recordAttackRootSpans(agg)

		agg.Start()
		defer func() {
			require.NoError(t, agg.Close())
		}()

		time.Sleep(opts.CalculationInterval + time.Millisecond)
		synctest.Wait()

		stored, err := store.GetThroughput(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		require.NoError(t, err)
		assertStoredBounds(t, stored)

		post := agg.(*aggregator).postAggregator
		require.NotEmpty(t, post.throughputs)
		require.LessOrEqual(t, len(post.throughputs), opts.AggregationBuckets)
		for _, bucket := range post.throughputs {
			assertNameBounds(t, throughputNames(bucket.throughput))
		}
		post.mu.RLock()
		probabilities := post.probabilities
		qps := post.qps
		post.mu.RUnlock()
		assertNameBounds(t, probabilityNames(probabilities))
		assertNameBounds(t, probabilityNames(qps))

		latest, err := store.GetLatestProbabilities()
		require.NoError(t, err)
		assertNameBounds(t, probabilityNames(latest))
	})
}

func newCardinalityAggregator(t *testing.T, store samplingstore.Store) samplingstrategy.Aggregator {
	t.Helper()
	participant := &epmocks.ElectionParticipant{}
	participant.On("IsLeader").Return(true)
	agg, err := NewAggregator(cardinalityOptions(), zap.NewNop(), metricstest.NewFactory(0), participant, store)
	require.NoError(t, err)
	return agg
}

func cardinalityOptions() Options {
	opts := DefaultOptions()
	opts.CalculationInterval = time.Second
	opts.AggregationBuckets = 2
	opts.BucketsForCalculation = 1
	opts.Delay = 0
	return opts
}

func recordAttackRootSpans(agg samplingstrategy.Aggregator) {
	long := strings.Repeat("n", maxAdaptiveSamplingNameLength*16)
	agg.HandleRootSpan(rootSpan(long, long))
	n := maxAdaptiveSamplingOperations * adaptiveSamplingAttackMultiple
	for i := range n {
		agg.HandleRootSpan(rootSpan(fmt.Sprintf("svc-%d", i), fmt.Sprintf("op-%d", i)))
	}
}

func attackThroughputRecords() []*samplingmodel.Throughput {
	long := strings.Repeat("n", maxAdaptiveSamplingNameLength*16)
	n := maxAdaptiveSamplingOperations * adaptiveSamplingAttackMultiple
	out := make([]*samplingmodel.Throughput, 0, n+1)
	out = append(out, &samplingmodel.Throughput{
		Service:       long,
		Operation:     long,
		Count:         1,
		Probabilities: map[string]struct{}{"0.001000": {}},
	})
	for i := range n {
		out = append(out, &samplingmodel.Throughput{
			Service:       fmt.Sprintf("svc-%d", i),
			Operation:     fmt.Sprintf("op-%d", i),
			Count:         1,
			Probabilities: map[string]struct{}{"0.001000": {}},
		})
	}
	return out
}

func rootSpan(service, operation string) *model.Span {
	return &model.Span{
		OperationName: operation,
		Process:       &model.Process{ServiceName: service},
		Tags:          samplerTags(),
	}
}

func samplerTags() model.KeyValues {
	return model.KeyValues{
		model.String(model.SamplerTypeKey, "probabilistic"),
		model.String(model.SamplerParamKey, "0.001"),
	}
}

func throughputNames(throughput serviceOperationThroughput) []string {
	var names []string
	for service, operations := range throughput {
		for operation := range operations {
			names = append(names, service+"\x00"+operation)
		}
	}
	return names
}

func probabilityNames(m map[string]map[string]float64) []string {
	var names []string
	for service, operations := range m {
		for operation := range operations {
			names = append(names, service+"\x00"+operation)
		}
	}
	return names
}

func cacheNames(cache SamplingCache) []string {
	var names []string
	for service, operations := range cache {
		for operation := range operations {
			names = append(names, service+"\x00"+operation)
		}
	}
	return names
}

func assertStoredBounds(t *testing.T, stored []*samplingmodel.Throughput) {
	t.Helper()
	names := make([]string, 0, len(stored))
	for _, throughput := range stored {
		names = append(names, throughput.Service+"\x00"+throughput.Operation)
	}
	assertNameBounds(t, names)
}

func assertNameBounds(t *testing.T, names []string) {
	t.Helper()
	require.NotEmpty(t, names)
	require.LessOrEqual(t, len(names), maxAdaptiveSamplingOperations)
	for _, name := range names {
		service, operation, ok := strings.Cut(name, "\x00")
		require.True(t, ok)
		require.LessOrEqual(t, len(service), maxAdaptiveSamplingNameLength)
		require.LessOrEqual(t, len(operation), maxAdaptiveSamplingNameLength)
	}
}
