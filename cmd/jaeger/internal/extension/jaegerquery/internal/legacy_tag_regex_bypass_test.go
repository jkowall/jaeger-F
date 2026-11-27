// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/jaegertracing/jaeger-idl/proto-gen/api_v2"
	expression "github.com/jaegertracing/jaeger-idl/query/expression/v1"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/querysvc"
	"github.com/jaegertracing/jaeger/components/extension/jaegerquery/queryinterceptor"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/esclient"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/indices"
	"github.com/jaegertracing/jaeger/internal/storage/v2/elasticsearch/tracestore"
	"github.com/jaegertracing/jaeger/internal/storage/v2/elasticsearch/tracestore/core"
)

const (
	sensitiveTag    = "user.email"
	tagPatternValue = "a.*"
)

// tagRegexRefusal is the access policy under test: exact equality on sensitiveTag is
// allowed, and any other operator on it is refused. The query is returned unchanged
// on approval so a fallback that compares filters still sees "nothing changed".
type tagRegexRefusal struct {
	queryinterceptor.UnsupportedSpanSearch

	calls  int
	ops    []expression.Operator
	denied bool
}

func (p *tagRegexRefusal) OnTraceQuery(ctx context.Context, query queryinterceptor.TraceQuery) (context.Context, queryinterceptor.TraceQuery, error) {
	p.calls++
	if err := p.walk(query.Filter); err != nil {
		return ctx, query, err
	}
	return ctx, query, nil
}

func (p *tagRegexRefusal) OnTraceResult(ctx context.Context, traces []ptrace.Traces) (context.Context, []ptrace.Traces, error) {
	return ctx, traces, nil
}

func (p *tagRegexRefusal) walk(call *expression.Call) error {
	if call == nil {
		return nil
	}
	if len(call.Args) > 0 {
		if ref, ok := call.Args[0].(*expression.AttributeRef); ok && ref != nil && ref.Key == sensitiveTag {
			p.ops = append(p.ops, call.Op)
			if call.Op != expression.OpEq {
				p.denied = true
				return fmt.Errorf("refusing %s on %s: %w", call.Op, ref.Key, queryinterceptor.ErrAccessDenied)
			}
		}
	}
	for _, arg := range call.Args {
		nested, ok := arg.(*expression.Call)
		if !ok {
			continue
		}
		if err := p.walk(nested); err != nil {
			return err
		}
	}
	return nil
}

type tagCaptureSearcher struct {
	reqs []esclient.SearchRequest
}

func (c *tagCaptureSearcher) Search(_ context.Context, _ []string, req esclient.SearchRequest) (*esclient.SearchResponse, error) {
	c.reqs = append(c.reqs, req)
	return &esclient.SearchResponse{}, nil
}

func (c *tagCaptureSearcher) MultiSearch(context.Context, []esclient.MultiSearchRequest) ([]esclient.SearchResponse, error) {
	return nil, fmt.Errorf("tag search must not fetch traces before the id query is checked")
}

func regexpFields(src any, value string) []string {
	var fields []string
	var walk func(any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			if raw, ok := n["regexp"]; ok {
				bodyByField, _ := raw.(map[string]any)
				for field, body := range bodyByField {
					obj, _ := body.(map[string]any)
					if got, _ := obj["value"].(string); got == value {
						fields = append(fields, field)
					}
				}
				return
			}
			for _, child := range n {
				walk(child)
			}
		case []any:
			for _, child := range n {
				walk(child)
			}
		default:
		}
	}
	walk(src)
	return fields
}

// TestLegacyTagsRegex_NotExecutedWhenEqualityIsApproved covers the v2 gRPC tags map,
// the legacy tag parameter. It reaches the same query service and Elasticsearch reader
// as query.attributes. The search must be refused, or run as the equality the
// interceptor approved, and must not send the tag value as a Lucene regexp.
func TestLegacyTagsRegex_NotExecutedWhenEqualityIsApproved(t *testing.T) {
	policy := &tagRegexRefusal{}
	searcher := &tagCaptureSearcher{}
	reader := tracestore.NewTraceReader(core.SpanReaderParams{
		Searcher:          searcher,
		Logger:            zap.NewNop(),
		Tracer:            noop.NewTracerProvider().Tracer("test"),
		MaxTraceDuration:  time.Hour,
		MaxDocCount:       1000,
		TagDotReplacement: "@",
		SpanRotation:      indices.NewAliasedRotation("jaeger-span", "jaeger-span"),
		ServiceRotation:   indices.NewAliasedRotation("jaeger-service", "jaeger-service"),
	})
	qs := querysvc.NewQueryService(reader, nil, querysvc.QueryServiceOptions{
		Interceptors: []queryinterceptor.Interceptor{policy},
	})
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	handler := NewGRPCHandler(qs, GRPCHandlerOptions{})
	err := handler.FindTraces(&api_v2.FindTracesRequest{
		Query: &api_v2.TraceQueryParameters{
			ServiceName:  "frontend",
			Tags:         map[string]string{sensitiveTag: tagPatternValue},
			StartTimeMin: start,
			StartTimeMax: start.Add(time.Hour),
		},
	}, &v2FindStream{ctx: t.Context()})
	require.NoError(t, err)

	require.NotZero(t, policy.calls, "the interceptor has to see the search")
	var patternFields []string
	for _, req := range searcher.reqs {
		require.NotNil(t, req.Query)
		src, srcErr := req.Query.Source()
		require.NoError(t, srcErr)
		patternFields = append(patternFields, regexpFields(src, tagPatternValue)...)
	}
	if len(patternFields) > 0 {
		slices.Sort(patternFields)
		t.Fatalf("interceptor approved %v on %s and Elasticsearch ran regexp %q on %v (denied=%v)",
			policy.ops, sensitiveTag, tagPatternValue, patternFields, policy.denied)
	}
	if policy.denied {
		require.Empty(t, searcher.reqs, "a refused search must not reach Elasticsearch")
		return
	}
	require.Contains(t, policy.ops, expression.OpEq)
	require.NotEmpty(t, searcher.reqs, "an approved search must reach Elasticsearch so the executed predicate can be checked")
}

type v2FindStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *v2FindStream) Context() context.Context { return s.ctx }

func (*v2FindStream) Send(*api_v2.SpansResponseChunk) error { return nil }
