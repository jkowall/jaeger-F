// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package adaptivesampling

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/gogo/protobuf/jsonpb"
	"github.com/open-telemetry/opentelemetry-collector-contrib/extension/storage/storagetest"
	jaegertranslator "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/translator/jaeger"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/translator/zipkin/zipkinv2"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger-idl/proto-gen/api_v2"
	jaegerthrift "github.com/jaegertracing/jaeger-idl/thrift-gen/jaeger"
	"github.com/jaegertracing/jaeger/cmd/internal/storageconfig"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerstorage"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/remotesampling"
	"github.com/jaegertracing/jaeger/internal/distributedlock"
	"github.com/jaegertracing/jaeger/internal/leaderelection"
	"github.com/jaegertracing/jaeger/internal/metricstest"
	samplinggrpc "github.com/jaegertracing/jaeger/internal/sampling/grpc"
	samplinghttp "github.com/jaegertracing/jaeger/internal/sampling/http"
	"github.com/jaegertracing/jaeger/internal/sampling/samplingstrategy/adaptive"
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/samplingstore"
	memstore "github.com/jaegertracing/jaeger/internal/storage/v2/memory"
)

const (
	suppressionOperation = "checkout"
	// initialSamplingProbability matches cmd/jaeger/config.yaml and
	// cmd/jaeger/config-cassandra.yaml. MinSamplingProbability stays at the
	// DefaultOptions value of 1e-5.
	initialSamplingProbability = 0.1
	// spansPerVictim is large enough that one calculation interval of root
	// spans, at the default target of 1 sample/s, drives an uncapped
	// probability down to MinSamplingProbability.
	spansPerVictim = 20000
)

// notLeaderLock matches a Cassandra lease Acquire that lost to another owner:
// internal/storage/distributedlock/cassandra/lock.go returns false, nil.
type notLeaderLock struct{}

func (notLeaderLock) Acquire(string, time.Duration) (bool, error) { return false, nil }
func (notLeaderLock) Forfeit(string) (bool, error)                { return false, nil }

var _ distributedlock.Lock = notLeaderLock{}

type follower struct {
	provider    *adaptive.Provider
	participant *leaderelection.DistributedElectionParticipant
	handler     *samplinghttp.Handler
	grpc        samplinggrpc.Handler
}

// TestForgedRootSpansCannotSuppressAnotherServicesSampling is the spec for the
// adaptive-sampling trust bug. Receivers in the sample collector configs do
// not authenticate the sender. processTraces counts every root span whose
// sampler tags parse, under the service and operation names the span itself
// carries. The leader writes a new probability from that count, and any
// collector that is not the leader serves it on the remote sampling HTTP and
// gRPC endpoints with no further check that the caller is that service.
//
// The probability those endpoints serve for each named service must stay at
// the probability published before the spans arrived. The test fails while
// forged spans can push it down.
func TestForgedRootSpansCannotSuppressAnotherServicesSampling(t *testing.T) {
	ctx := context.Background()
	opts := adaptive.DefaultOptions()
	opts.InitialSamplingProbability = initialSamplingProbability
	opts.CalculationInterval = time.Second
	opts.Delay = 0
	opts.AggregationBuckets = 15
	opts.BucketsForCalculation = 1
	opts.LeaderLeaseRefreshInterval = 20 * time.Millisecond
	opts.FollowerLeaseRefreshInterval = 20 * time.Millisecond

	host := startAdaptiveHost(t, opts)
	tp := newTraceProcessor(*createDefaultConfig().(*Config), component.TelemetrySettings{
		Logger:         zaptest.NewLogger(t),
		TracerProvider: nooptrace.NewTracerProvider(),
		MeterProvider:  noopmetric.NewMeterProvider(),
	})
	require.NoError(t, tp.start(ctx, host))
	t.Cleanup(func() {
		require.NoError(t, tp.close(ctx))
	})

	parts, err := remotesampling.GetAdaptiveSamplingComponents(host)
	require.NoError(t, err)
	require.Eventually(t, parts.DistLock.IsLeader, 2*time.Second, 10*time.Millisecond)

	// Read the probability the unauthenticated sampling endpoint publishes
	// before any forged spans. No per-operation override exists yet.
	before := serveFollower(t, parts.SamplingStore, opts)
	published := strategyFor(t, before.provider, "otlp-json-victim")
	require.NotNil(t, published.OperationSampling)
	require.InDelta(t, initialSamplingProbability, published.OperationSampling.DefaultSamplingProbability, 1e-12)
	require.Empty(t, published.OperationSampling.PerOperationStrategies)
	param := strconv.FormatFloat(published.OperationSampling.DefaultSamplingProbability, 'f', -1, 64)
	require.NoError(t, before.close())

	payloads := []ptrace.Traces{
		otlpJSONTraces(t, "otlp-json-victim", suppressionOperation, param, spansPerVictim),
		otlpProtoTraces(t, "otlp-proto-victim", suppressionOperation, published.OperationSampling.DefaultSamplingProbability, spansPerVictim),
		zipkinJSONTraces(t, "zipkin-victim", suppressionOperation, param, spansPerVictim),
		jaegerProtoTraces(t, "jaeger-proto-victim", suppressionOperation, param, spansPerVictim),
		jaegerThriftTraces(t, "jaeger-thrift-victim", suppressionOperation, published.OperationSampling.DefaultSamplingProbability, spansPerVictim),
		jaegerIntParamTraces(t, "jaeger-int-victim", suppressionOperation, spansPerVictim),
		// A parent span is not a root span. Its service must be left alone.
		otlpJSONChildTraces(t, "child-victim", suppressionOperation, param, 100),
	}
	for _, traces := range payloads {
		out, procErr := tp.processTraces(ctx, traces)
		require.NoError(t, procErr)
		require.Equal(t, traces.SpanCount(), out.SpanCount())
	}

	// One calculation interval is one second. Wait long enough for the leader
	// to have consumed the throughput, and stop early once a probability moves
	// so a vulnerable run does not sit out the whole window. A fixed run keeps
	// the initial probability and falls through to the assertions below.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		probs, probErr := parts.SamplingStore.GetLatestProbabilities()
		if probErr == nil {
			if rate, ok := probs["otlp-json-victim"][suppressionOperation]; ok && rate < initialSamplingProbability {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A non-leader collector loads those probabilities and serves them.
	// Provider.Start does that load; followers repeat it on their refresh loop.
	after := serveFollower(t, parts.SamplingStore, opts)
	t.Cleanup(func() {
		require.NoError(t, after.close())
	})

	victims := []string{
		"otlp-json-victim",
		"otlp-proto-victim",
		"zipkin-victim",
		"jaeger-proto-victim",
		"jaeger-thrift-victim",
		"jaeger-int-victim",
	}
	for _, service := range victims {
		t.Run(service+"/http", func(t *testing.T) {
			assertStillInitial(t, service, httpStrategy(t, after.handler, service))
		})
		t.Run(service+"/grpc", func(t *testing.T) {
			grpcResp, grpcErr := after.grpc.GetSamplingStrategy(ctx, &api_v2.SamplingStrategyParameters{
				ServiceName: service,
			})
			require.NoError(t, grpcErr)
			assertStillInitial(t, service, grpcResp)
		})
	}

	t.Run("child-span", func(t *testing.T) {
		assertStillInitial(t, "child-victim", httpStrategy(t, after.handler, "child-victim"))
	})
	t.Run("untouched-service", func(t *testing.T) {
		assertStillInitial(t, "bystander", httpStrategy(t, after.handler, "bystander"))
	})
}

func assertStillInitial(t *testing.T, service string, strategy *api_v2.SamplingStrategyResponse) {
	t.Helper()
	require.NotNil(t, strategy.OperationSampling, "strategy for %s", service)
	require.InDelta(t, initialSamplingProbability, strategy.OperationSampling.DefaultSamplingProbability, 1e-12, "default probability for %s", service)
	for _, op := range strategy.OperationSampling.PerOperationStrategies {
		if op.Operation != suppressionOperation || op.ProbabilisticSampling == nil {
			continue
		}
		require.InDeltaf(
			t,
			initialSamplingProbability,
			op.ProbabilisticSampling.SamplingRate,
			1e-12,
			"forged spans lowered sampling probability for %s operation %s to %g",
			service,
			suppressionOperation,
			op.ProbabilisticSampling.SamplingRate,
		)
	}
}

func (f follower) close() error {
	var err error
	if f.provider != nil {
		err = f.provider.Close()
	}
	if f.participant != nil {
		if closeErr := f.participant.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

func serveFollower(t *testing.T, store samplingstore.Store, opts adaptive.Options) follower {
	t.Helper()
	participant := leaderelection.NewElectionParticipant(notLeaderLock{}, "sampling_store_leader", leaderelection.ElectionParticipantOptions{
		LeaderLeaseRefreshInterval:   opts.LeaderLeaseRefreshInterval,
		FollowerLeaseRefreshInterval: opts.FollowerLeaseRefreshInterval,
		Logger:                       zap.NewNop(),
	})
	require.NoError(t, participant.Start())
	provider := adaptive.NewProvider(opts, zap.NewNop(), participant, store)
	require.NoError(t, provider.Start())
	handler := samplinghttp.NewHandler(samplinghttp.HandlerParams{
		ConfigManager:  &samplinghttp.ConfigManager{SamplingProvider: provider},
		MetricsFactory: metricstest.NewFactory(0),
	})
	return follower{
		provider:    provider,
		participant: participant,
		handler:     handler,
		grpc:        samplinggrpc.NewHandler(provider),
	}
}

func strategyFor(t *testing.T, provider *adaptive.Provider, service string) *api_v2.SamplingStrategyResponse {
	t.Helper()
	strategy, err := provider.GetSamplingStrategy(context.Background(), service)
	require.NoError(t, err)
	return strategy
}

func httpStrategy(t *testing.T, handler *samplinghttp.Handler, service string) *api_v2.SamplingStrategyResponse {
	t.Helper()
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/?service="+url.QueryEscape(service), nil)
	require.NoError(t, err)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "anonymous sampling request for %s: %s", service, body)

	strategy := &api_v2.SamplingStrategyResponse{}
	require.NoError(t, jsonpb.Unmarshal(bytes.NewReader(body), strategy))
	return strategy
}

func startAdaptiveHost(t *testing.T, opts adaptive.Options) component.Host {
	t.Helper()
	const storeName = "foobar"
	telemetrySettings := component.TelemetrySettings{
		Logger:         zaptest.NewLogger(t),
		TracerProvider: nooptrace.NewTracerProvider(),
		MeterProvider:  noopmetric.NewMeterProvider(),
	}
	storageExtension, err := jaegerstorage.NewFactory().Create(
		context.Background(),
		extension.Settings{
			ID:                jaegerstorage.ID,
			TelemetrySettings: telemetrySettings,
		},
		&jaegerstorage.Config{
			Config: storageconfig.Config{
				TraceBackends: map[string]storageconfig.TraceBackend{
					storeName: {Memory: &memstore.Configuration{MaxTraces: 10000}},
				},
			},
		},
	)
	require.NoError(t, err)
	storageHost := storagetest.NewStorageHost().WithExtension(jaegerstorage.ID, storageExtension)
	require.NoError(t, storageExtension.Start(context.Background(), storageHost))
	t.Cleanup(func() { require.NoError(t, storageExtension.Shutdown(context.Background())) })

	samplingExtension, err := remotesampling.NewFactory().Create(
		context.Background(),
		extension.Settings{
			ID:                remotesampling.ID,
			TelemetrySettings: telemetrySettings,
		},
		&remotesampling.Config{
			Adaptive: configoptional.Some(remotesampling.AdaptiveConfig{
				SamplingStore: storeName,
				Options:       opts,
			}),
		},
	)
	require.NoError(t, err)
	host := storagetest.NewStorageHost().WithExtension(remotesampling.ID, samplingExtension)
	require.NoError(t, samplingExtension.Start(context.Background(), storageHost))
	t.Cleanup(func() { require.NoError(t, samplingExtension.Shutdown(context.Background())) })
	return host
}

func otlpJSONTraces(t *testing.T, service, operation, param string, n int) ptrace.Traces {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"`)
	buf.WriteString(service)
	buf.WriteString(`"}}]},"scopeSpans":[{"spans":[`)
	for i := 1; i <= n; i++ {
		if i > 1 {
			buf.WriteByte(',')
		}
		fmt.Fprintf(&buf, `{"traceId":"%032x","spanId":"%016x","name":"%s","attributes":[{"key":"sampler.type","value":{"stringValue":"probabilistic"}},{"key":"sampler.param","value":{"stringValue":"%s"}}]}`, i, i, operation, param)
	}
	buf.WriteString(`]}]}]}`)
	return unmarshalOTLPJSON(t, buf.Bytes())
}

func otlpJSONChildTraces(t *testing.T, service, operation, param string, n int) ptrace.Traces {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"`)
	buf.WriteString(service)
	buf.WriteString(`"}}]},"scopeSpans":[{"spans":[`)
	for i := 1; i <= n; i++ {
		if i > 1 {
			buf.WriteByte(',')
		}
		fmt.Fprintf(&buf, `{"traceId":"%032x","spanId":"%016x","parentSpanId":"%016x","name":"%s","attributes":[{"key":"sampler.type","value":{"stringValue":"probabilistic"}},{"key":"sampler.param","value":{"stringValue":"%s"}}]}`, i, i, i+1, operation, param)
	}
	buf.WriteString(`]}]}]}`)
	return unmarshalOTLPJSON(t, buf.Bytes())
}

func unmarshalOTLPJSON(t *testing.T, body []byte) ptrace.Traces {
	t.Helper()
	req := ptraceotlp.NewExportRequest()
	require.NoError(t, req.UnmarshalJSON(body))
	return req.Traces()
}

func otlpProtoTraces(t *testing.T, service, operation string, param float64, n int) ptrace.Traces {
	t.Helper()
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", service)
	spans := rs.ScopeSpans().AppendEmpty().Spans()
	spans.EnsureCapacity(n)
	for i := 1; i <= n; i++ {
		span := spans.AppendEmpty()
		var traceID [16]byte
		var spanID [8]byte
		binary.BigEndian.PutUint64(traceID[8:], uint64(i))
		binary.BigEndian.PutUint64(spanID[:], uint64(i))
		span.SetTraceID(pcommon.TraceID(traceID))
		span.SetSpanID(pcommon.SpanID(spanID))
		span.SetName(operation)
		span.Attributes().PutStr("sampler.type", "probabilistic")
		span.Attributes().PutDouble("sampler.param", param)
	}
	raw, err := ptraceotlp.NewExportRequestFromTraces(td).MarshalProto()
	require.NoError(t, err)
	decoded := ptraceotlp.NewExportRequest()
	require.NoError(t, decoded.UnmarshalProto(raw))
	return decoded.Traces()
}

func zipkinJSONTraces(t *testing.T, service, operation, param string, n int) ptrace.Traces {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i := 1; i <= n; i++ {
		if i > 1 {
			buf.WriteByte(',')
		}
		fmt.Fprintf(&buf, `{"traceId":"%032x","id":"%016x","name":"%s","localEndpoint":{"serviceName":"%s"},"tags":{"sampler.type":"probabilistic","sampler.param":"%s"}}`, i, i, operation, service, param)
	}
	buf.WriteByte(']')
	traces, err := zipkinv2.NewJSONTracesUnmarshaler(false).UnmarshalTraces(buf.Bytes())
	require.NoError(t, err)
	return traces
}

func jaegerProtoTraces(t *testing.T, service, operation, param string, n int) ptrace.Traces {
	t.Helper()
	spans := make([]*model.Span, 0, n)
	for i := 1; i <= n; i++ {
		spans = append(spans, &model.Span{
			TraceID:       model.TraceID{Low: uint64(i)},
			SpanID:        model.NewSpanID(uint64(i)),
			OperationName: operation,
			Tags: model.KeyValues{
				model.String("sampler.type", "probabilistic"),
				model.String("sampler.param", param),
			},
		})
	}
	traces, err := jaegertranslator.ProtoToTraces([]*model.Batch{{
		Process: &model.Process{ServiceName: service},
		Spans:   spans,
	}})
	require.NoError(t, err)
	return traces
}

func jaegerIntParamTraces(t *testing.T, service, operation string, n int) ptrace.Traces {
	t.Helper()
	spans := make([]*model.Span, 0, n)
	for i := 1; i <= n; i++ {
		spans = append(spans, &model.Span{
			TraceID:       model.TraceID{Low: uint64(i)},
			SpanID:        model.NewSpanID(uint64(i)),
			OperationName: operation,
			Tags: model.KeyValues{
				model.String("sampler.type", "probabilistic"),
				model.Int64("sampler.param", 1),
			},
		})
	}
	traces, err := jaegertranslator.ProtoToTraces([]*model.Batch{{
		Process: &model.Process{ServiceName: service},
		Spans:   spans,
	}})
	require.NoError(t, err)
	return traces
}

func jaegerThriftTraces(t *testing.T, service, operation string, param float64, n int) ptrace.Traces {
	t.Helper()
	samplerType := "probabilistic"
	spans := make([]*jaegerthrift.Span, 0, n)
	for i := 1; i <= n; i++ {
		spans = append(spans, &jaegerthrift.Span{
			TraceIdLow:    int64(i),
			SpanId:        int64(i),
			OperationName: operation,
			ParentSpanId:  0,
			Tags: []*jaegerthrift.Tag{
				{Key: "sampler.type", VType: jaegerthrift.TagType_STRING, VStr: &samplerType},
				{Key: "sampler.param", VType: jaegerthrift.TagType_DOUBLE, VDouble: &param},
			},
		})
	}
	traces, err := jaegertranslator.ThriftToTraces(&jaegerthrift.Batch{
		Process: &jaegerthrift.Process{ServiceName: service},
		Spans:   spans,
	})
	require.NoError(t, err)
	return traces
}
