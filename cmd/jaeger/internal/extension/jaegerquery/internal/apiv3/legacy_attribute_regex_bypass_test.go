// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package apiv3

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	expression "github.com/jaegertracing/jaeger-idl/query/expression/v1"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/querysvc"
	"github.com/jaegertracing/jaeger/components/extension/jaegerquery/queryinterceptor"
	"github.com/jaegertracing/jaeger/internal/jptrace"
	"github.com/jaegertracing/jaeger/internal/proto/api_v3"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/esclient"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/indices"
	"github.com/jaegertracing/jaeger/internal/storage/v2/elasticsearch/tracestore"
	"github.com/jaegertracing/jaeger/internal/storage/v2/elasticsearch/tracestore/core"
)

const (
	// sensitiveAttribute is the attribute an access-control interceptor lets callers
	// compare for exact equality and refuses to let them match by pattern.
	sensitiveAttribute = "user.email"
	// attackerPattern is a legacy tag value. An equality compares that text; a Lucene
	// regexp with the same text matches every value that merely begins with "a".
	attackerPattern = "a.*"
)

// regexRefusal approves exact equality on sensitiveAttribute and refuses every other
// operator on it. It decides from the filter the query service shows it, and it returns
// that filter unchanged when it approves, which is the case the legacy-query fallback
// keys on.
type regexRefusal struct {
	queryinterceptor.UnsupportedSpanSearch

	calls  int
	ops    []expression.Operator
	denied bool
}

func (p *regexRefusal) OnTraceQuery(ctx context.Context, query queryinterceptor.TraceQuery) (context.Context, queryinterceptor.TraceQuery, error) {
	p.calls++
	if err := p.walk(query.Filter); err != nil {
		return ctx, query, err
	}
	return ctx, query, nil
}

func (p *regexRefusal) OnTraceResult(ctx context.Context, traces []ptrace.Traces) (context.Context, []ptrace.Traces, error) {
	return ctx, traces, nil
}

func (p *regexRefusal) walk(call *expression.Call) error {
	if call == nil {
		return nil
	}
	if len(call.Args) > 0 {
		if ref, ok := call.Args[0].(*expression.AttributeRef); ok && ref != nil && ref.Key == sensitiveAttribute {
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

// captureSearcher records the searches the Elasticsearch reader builds. It stands in
// for the cluster only after that reader has already turned the query into a request.
type captureSearcher struct {
	reqs []esclient.SearchRequest
}

func (c *captureSearcher) Search(_ context.Context, _ []string, req esclient.SearchRequest) (*esclient.SearchResponse, error) {
	c.reqs = append(c.reqs, req)
	// An empty aggregation is "no trace ids", so the reader does not fetch documents.
	return &esclient.SearchResponse{}, nil
}

func (c *captureSearcher) MultiSearch(context.Context, []esclient.MultiSearchRequest) ([]esclient.SearchResponse, error) {
	return nil, fmt.Errorf("tag search must not fetch traces before the id query is checked")
}

type regexpHit struct {
	field string
	value string
}

func regexpHits(src any) []regexpHit {
	var hits []regexpHit
	var walk func(any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			if raw, ok := n["regexp"]; ok {
				fields, _ := raw.(map[string]any)
				for field, body := range fields {
					obj, _ := body.(map[string]any)
					value, _ := obj["value"].(string)
					hits = append(hits, regexpHit{field: field, value: value})
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
	return hits
}

func termValues(src any) []string {
	var values []string
	var walk func(any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			if raw, ok := n["term"]; ok {
				fields, _ := raw.(map[string]any)
				for _, body := range fields {
					switch b := body.(type) {
					case string:
						values = append(values, b)
					case map[string]any:
						if value, ok := b["value"].(string); ok {
							values = append(values, value)
						}
					default:
					}
				}
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
	return values
}

func (c *captureSearcher) hits(t *testing.T) (regexp []regexpHit, terms []string) {
	t.Helper()
	for _, req := range c.reqs {
		require.NotNil(t, req.Query)
		src, err := req.Query.Source()
		require.NoError(t, err)
		regexp = append(regexp, regexpHits(src)...)
		terms = append(terms, termValues(src)...)
	}
	return regexp, terms
}

// esQueryService is jaeger-query in front of the Elasticsearch/OpenSearch trace reader.
// OpenSearch is that same reader (cmd/internal/storageconfig/factory.go constructs both
// with elasticsearch.NewFactory).
func esQueryService(t *testing.T, searcher esclient.Searcher, policy queryinterceptor.Interceptor) *querysvc.QueryService {
	t.Helper()
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
	return querysvc.NewQueryService(reader, nil, querysvc.QueryServiceOptions{
		Interceptors: []queryinterceptor.Interceptor{policy},
	})
}

func searchWindow() (time.Time, time.Time) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return start, start.Add(time.Hour)
}

func legacyAttributeQuery() url.Values {
	start, end := searchWindow()
	q := url.Values{}
	q.Set(paramServiceNameDeprecated, "frontend")
	q.Set(paramAttributes, `{"`+sensitiveAttribute+`":"`+attackerPattern+`"}`)
	q.Set(paramTimeMinDeprecated, start.Format(time.RFC3339Nano))
	q.Set(paramTimeMaxDeprecated, end.Format(time.RFC3339Nano))
	return q
}

func (c *captureSearcher) assertApprovedPredicate(t *testing.T, policy *regexRefusal, requireTerm bool) {
	t.Helper()
	require.NotZero(t, policy.calls, "the interceptor has to see the search")
	hits, terms := c.hits(t)
	var patternFields []string
	for _, hit := range hits {
		if hit.value == attackerPattern {
			patternFields = append(patternFields, hit.field)
		}
	}
	if len(patternFields) > 0 {
		slices.Sort(patternFields)
		t.Fatalf("interceptor approved %v on %s and Elasticsearch ran regexp %q on %v (denied=%v)",
			policy.ops, sensitiveAttribute, attackerPattern, patternFields, policy.denied)
	}
	if policy.denied {
		require.Empty(t, c.reqs, "a refused search must not reach Elasticsearch")
		return
	}
	require.Contains(t, policy.ops, expression.OpEq)
	require.NotEmpty(t, c.reqs, "an approved search must reach Elasticsearch so the executed predicate can be checked")
	if requireTerm {
		require.Contains(t, terms, attackerPattern, "an approved equality has to be the term query storage runs")
	}
}

// TestLegacyAttributeRegex_NotExecutedWhenEqualityIsApproved is the spec for the
// legacy-tag bypass. A caller who is allowed an exact lookup on a sensitive attribute,
// and refused a pattern match on it, sends that attribute through the legacy tag fields.
// The search must then be refused, or storage must run the equality the interceptor
// approved. It must not run the caller's text as a Lucene regular expression.
//
// Span search is not covered: a span query has no legacy attribute fields, so the
// interceptor is shown the filter the caller sent. Memory, Badger, Cassandra and
// ClickHouse compare a legacy attribute as text equality, so they do not share this
// execution. The find_traces MCP tool compiles SQL into a filter rather than legacy
// attributes.
func TestLegacyAttributeRegex_NotExecutedWhenEqualityIsApproved(t *testing.T) {
	t.Run("http find traces", func(t *testing.T) {
		policy := &regexRefusal{}
		searcher := &captureSearcher{}
		gw := &HTTPGateway{
			QueryService: esQueryService(t, searcher, policy),
			Logger:       zap.NewNop(),
		}
		mux := http.NewServeMux()
		gw.RegisterRoutes(mux)

		req := httptest.NewRequest(http.MethodGet, routeFindTraces+"?"+legacyAttributeQuery().Encode(), http.NoBody)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		searcher.assertApprovedPredicate(t, policy, false)
	})

	t.Run("http find trace summaries", func(t *testing.T) {
		policy := &regexRefusal{}
		searcher := &captureSearcher{}
		gw := &HTTPGateway{
			QueryService: esQueryService(t, searcher, policy),
			Logger:       zap.NewNop(),
		}
		mux := http.NewServeMux()
		gw.RegisterRoutes(mux)

		req := httptest.NewRequest(http.MethodGet, routeFindSummaries+"?"+legacyAttributeQuery().Encode(), http.NoBody)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		searcher.assertApprovedPredicate(t, policy, false)
	})

	t.Run("grpc find traces", func(t *testing.T) {
		policy := &regexRefusal{}
		searcher := &captureSearcher{}
		start, end := searchWindow()
		h := &Handler{QueryService: esQueryService(t, searcher, policy)}
		err := h.internalFindTraces(t.Context(), &api_v3.FindTracesRequest{
			Query: &api_v3.TraceQueryParameters{
				ServiceName:  "frontend",
				Attributes:   map[string]string{sensitiveAttribute: attackerPattern},
				StartTimeMin: start,
				StartTimeMax: end,
			},
		}, func(*jptrace.TracesData) error { return nil })
		require.NoError(t, err)
		searcher.assertApprovedPredicate(t, policy, false)
	})

	t.Run("grpc find trace summaries", func(t *testing.T) {
		policy := &regexRefusal{}
		searcher := &captureSearcher{}
		start, end := searchWindow()
		h := &Handler{QueryService: esQueryService(t, searcher, policy)}
		err := h.FindTraceSummaries(&api_v3.FindTraceSummariesRequest{
			Query: &api_v3.TraceQueryParameters{
				ServiceName:  "frontend",
				Attributes:   map[string]string{sensitiveAttribute: attackerPattern},
				StartTimeMin: start,
				StartTimeMax: end,
			},
		}, &summaryStream{ctx: t.Context()})
		require.NoError(t, err)
		searcher.assertApprovedPredicate(t, policy, false)
	})

	// The same interceptor refuses a pattern the caller wrote as a filter, and storage
	// is not asked. This is the policy working when the predicate is shown as it runs.
	t.Run("an explicit regex filter is refused", func(t *testing.T) {
		policy := &regexRefusal{}
		searcher := &captureSearcher{}
		gw := &HTTPGateway{
			QueryService: esQueryService(t, searcher, policy),
			Logger:       zap.NewNop(),
		}
		mux := http.NewServeMux()
		gw.RegisterRoutes(mux)

		start, end := searchWindow()
		q := url.Values{}
		q.Set(paramTimeMin, start.Format(time.RFC3339Nano))
		q.Set(paramTimeMax, end.Format(time.RFC3339Nano))
		q.Set(paramFilter, `{"op":"regex","args":[{"attr":{"key":"`+sensitiveAttribute+`"}},{"scalar":{"value":"`+attackerPattern+`"}}]}`)
		req := httptest.NewRequest(http.MethodGet, routeFindTraces+"?"+q.Encode(), http.NoBody)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		require.Equal(t, http.StatusForbidden, rec.Code)
		searcher.assertApprovedPredicate(t, policy, false)
	})

	// An equality the caller wrote as a filter is the predicate storage runs: a term
	// query for that text, including when the text contains regular-expression syntax.
	t.Run("an explicit equality filter is a term query", func(t *testing.T) {
		policy := &regexRefusal{}
		searcher := &captureSearcher{}
		gw := &HTTPGateway{
			QueryService: esQueryService(t, searcher, policy),
			Logger:       zap.NewNop(),
		}
		mux := http.NewServeMux()
		gw.RegisterRoutes(mux)

		start, end := searchWindow()
		q := url.Values{}
		q.Set(paramTimeMin, start.Format(time.RFC3339Nano))
		q.Set(paramTimeMax, end.Format(time.RFC3339Nano))
		q.Set(paramFilter, `{"op":"eq","args":[{"attr":{"key":"`+sensitiveAttribute+`"}},{"scalar":{"value":"`+attackerPattern+`"}}]}`)
		req := httptest.NewRequest(http.MethodGet, routeFindTraces+"?"+q.Encode(), http.NoBody)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		searcher.assertApprovedPredicate(t, policy, true)
	})
}

type summaryStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *summaryStream) Context() context.Context { return s.ctx }

func (*summaryStream) Send(*api_v3.FindTraceSummariesResponse) error { return nil }
