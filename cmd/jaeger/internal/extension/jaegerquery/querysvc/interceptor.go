// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package querysvc

import (
	"context"
	"fmt"
	"iter"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	expression "github.com/jaegertracing/jaeger-idl/query/expression/v1"
	"github.com/jaegertracing/jaeger/components/extension/jaegerquery/queryinterceptor"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
)

// toInterceptorTraceQuery and fromInterceptorTraceQuery convert at the contract boundary, so the
// internal query type never crosses it.
func toInterceptorTraceQuery(q tracestore.TraceQueryParams) queryinterceptor.TraceQuery {
	return queryinterceptor.TraceQuery{
		Filter:       q.Filter,
		StartTimeMin: q.StartTimeMin,
		StartTimeMax: q.StartTimeMax,
	}
}

func fromInterceptorTraceQuery(q queryinterceptor.TraceQuery, original tracestore.TraceQueryParams) tracestore.TraceQueryParams {
	return tracestore.TraceQueryParams{
		Attributes:   pcommon.NewMap(),
		Filter:       q.Filter,
		StartTimeMin: q.StartTimeMin,
		StartTimeMax: q.StartTimeMax,
		SearchDepth:  original.SearchDepth,
		Pagination:   original.Pagination,
	}
}

// onTraceQuery runs every interceptor's OnTraceQuery in order, threading the context each returns into the
// next. The final context is returned so the caller can pass it to the storage reader and to
// OnTraceResult, letting an interceptor carry per-query state (a resolved caller identity, say) from
// the pre-query hook to the return path.
//
// The interceptors are shown the query in filter shape whatever shape it arrived in, because gating
// a search means reading and narrowing its predicates, and an interceptor should not have to find
// them in two places. A filter one of them leaves behind is not converted back: the query service
// chooses the outgoing shape from what the reader declared, and a predicate an interceptor added is
// held to the same capability check as one the caller sent.
//
// The filter the interceptors return is the one dispatched, even when none of them changed it.
// A legacy query is never sent on in its legacy shape instead, because a backend may evaluate the
// legacy fields as something other than the equalities the interceptors were shown: Elasticsearch
// matches a legacy tag value as a regular expression, and over the event level as well. An
// interceptor that approved an exact lookup would otherwise have approved a pattern search.
//
// The nil rule is checked after every hook rather than once at the end, because a predicate one
// interceptor adds is a restriction the next must not be able to remove: a query that had no
// filter, gained one, and lost it again would otherwise pass as the time-range search it started
// as.
func (qs QueryService) onTraceQuery(ctx context.Context, query tracestore.TraceQueryParams) (context.Context, tracestore.TraceQueryParams, error) {
	queryPostIntercept := toInterceptorTraceQuery(query.ToFilterShape())
	hadPredicates := queryPostIntercept.Filter != nil
	var err error
	for _, interceptor := range qs.options.Interceptors {
		ctx, queryPostIntercept, err = interceptor.OnTraceQuery(ctx, queryPostIntercept)
		if err != nil {
			return ctx, query, err
		}
		if hadPredicates && queryPostIntercept.Filter == nil {
			return ctx, query, errInterceptorDroppedFilter
		}
		hadPredicates = queryPostIntercept.Filter != nil
	}
	if queryPostIntercept.Filter != nil {
		queryPostIntercept.Filter, err = finalizeInterceptorFilter(queryPostIntercept.Filter)
		if err != nil {
			return ctx, query, err
		}
	}
	return ctx, fromInterceptorTraceQuery(queryPostIntercept, query), nil
}

// toInterceptorSpanQuery and fromInterceptorSpanQuery are the span search's converters at the
// same boundary. A span query has one shape, so only Pagination stays behind on the internal
// query: an interceptor shapes what is searched, not how the result is paged.
func toInterceptorSpanQuery(q tracestore.SpanQueryParams) queryinterceptor.SpanQuery {
	return queryinterceptor.SpanQuery{
		Filter:       q.Filter,
		StartTimeMin: q.StartTimeMin,
		StartTimeMax: q.StartTimeMax,
	}
}

func fromInterceptorSpanQuery(q queryinterceptor.SpanQuery, original tracestore.SpanQueryParams) tracestore.SpanQueryParams {
	return tracestore.SpanQueryParams{
		StartTimeMin: q.StartTimeMin,
		StartTimeMax: q.StartTimeMax,
		Filter:       q.Filter,
		OrderBy:      original.OrderBy,
		Pagination:   original.Pagination,
	}
}

// onSpanQuery runs every interceptor's OnSpanQuery in order, threading the context each returns
// into the next, as onTraceQuery does for a trace search. The filter the interceptors leave behind
// is finalized and sent on; a span query has one shape, so there is no legacy branch.
//
// The nil rule is checked after every hook rather than once at the end, because a predicate one
// interceptor adds is a restriction the next must not be able to remove: a query that had no
// filter, gained one, and lost it again would otherwise pass as the time-range search it started as.
func (qs QueryService) onSpanQuery(ctx context.Context, query tracestore.SpanQueryParams) (context.Context, tracestore.SpanQueryParams, error) {
	queryPostIntercept := toInterceptorSpanQuery(query)
	hadPredicates := queryPostIntercept.Filter != nil
	var err error
	for _, interceptor := range qs.options.Interceptors {
		ctx, queryPostIntercept, err = interceptor.OnSpanQuery(ctx, queryPostIntercept)
		if err != nil {
			return ctx, query, err
		}
		if hadPredicates && queryPostIntercept.Filter == nil {
			return ctx, query, errInterceptorDroppedFilter
		}
		hadPredicates = queryPostIntercept.Filter != nil
	}
	if queryPostIntercept.Filter != nil {
		queryPostIntercept.Filter, err = finalizeInterceptorFilter(queryPostIntercept.Filter)
		if err != nil {
			return ctx, query, err
		}
	}
	return ctx, fromInterceptorSpanQuery(queryPostIntercept, query), nil
}

// finalizeInterceptorFilter finalizes the filter an interceptor returned and rejects what it must
// not hand to storage. An interceptor builds its filter by hand, in code jaeger-query does not
// control, and a malformed tree is typically answered by a backend matching nothing rather than
// refusing — so a search meant to be narrowed would come back wrong with nothing to say why.
//
// It finalizes rather than only validates, so that an interceptor's predicate reaches storage as the
// equal of one a caller sent, having been through the same stage. The one mistake that fails open,
// a filter dropped from a query that had predicates, is refused by the caller after each hook, so
// the filter that arrives here is never nil.
func finalizeInterceptorFilter(returned *expression.Call) (*expression.Call, error) {
	finalized, err := tracestore.FinalizeFilter(returned)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInterceptorFilter, err)
	}
	return finalized, nil
}

// interceptTraceResults hands every batch of seq to the interceptors' OnTraceResult in order, threading the
// context each returns into the next so that state can accumulate across a multi-batch result.
// An OnTraceResult error ends the stream rather than yielding later batches, which could leak results
// the failed sanitize or redaction was meant to withhold.
//
// It wraps the batches as storage yielded them, before the query service aggregates and adjusts
// them, so an interceptor rewrites the traces the reader actually returned.
func (qs QueryService) interceptTraceResults(
	ctx context.Context,
	seq iter.Seq2[[]ptrace.Traces, error],
) iter.Seq2[[]ptrace.Traces, error] {
	if len(qs.options.Interceptors) == 0 {
		return seq
	}
	return interceptBatches(seq, qs.traceResultInterceptor(ctx))
}

// traceResultInterceptor returns the function that hands one batch to every interceptor's
// OnTraceResult in order. The context each returns feeds the next call, across every batch the
// function is given, so one function serves a whole result stream however it is chunked.
func (qs QueryService) traceResultInterceptor(ctx context.Context) func([]ptrace.Traces) ([]ptrace.Traces, error) {
	return func(traces []ptrace.Traces) ([]ptrace.Traces, error) {
		var err error
		for _, interceptor := range qs.options.Interceptors {
			ctx, traces, err = interceptor.OnTraceResult(ctx, traces)
			if err != nil {
				return nil, err
			}
		}
		return traces, nil
	}
}

func interceptBatches(
	seq iter.Seq2[[]ptrace.Traces, error],
	intercept func([]ptrace.Traces) ([]ptrace.Traces, error),
) iter.Seq2[[]ptrace.Traces, error] {
	return func(yield func([]ptrace.Traces, error) bool) {
		for traces, err := range seq {
			if err != nil {
				if !yield(nil, err) {
					return
				}
				continue
			}
			traces, err = intercept(traces)
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(traces, nil) {
				return
			}
		}
	}
}

// interceptSpanResults hands every chunk of seq to the interceptors' OnSpanResult in order, with
// the same context threading and error handling as interceptTraceResults. The chunk's page token
// passes through untouched.
func (qs QueryService) interceptSpanResults(
	ctx context.Context,
	seq iter.Seq2[tracestore.PageChunk[ptrace.Traces], error],
) iter.Seq2[tracestore.PageChunk[ptrace.Traces], error] {
	if len(qs.options.Interceptors) == 0 {
		return seq
	}
	return func(yield func(tracestore.PageChunk[ptrace.Traces], error) bool) {
		for chunk, err := range seq {
			if err != nil {
				if !yield(tracestore.PageChunk[ptrace.Traces]{}, err) {
					return
				}
				continue
			}
			for _, interceptor := range qs.options.Interceptors {
				ctx, chunk.Results, err = interceptor.OnSpanResult(ctx, chunk.Results)
				if err != nil {
					yield(tracestore.PageChunk[ptrace.Traces]{}, err)
					return
				}
			}
			if !yield(chunk, nil) {
				return
			}
		}
	}
}
