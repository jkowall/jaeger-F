// Copyright (c) 2018 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package spanstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger/internal/metrics"
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/spanstore"
	"github.com/jaegertracing/jaeger/internal/storage/v1/badger"
)

func TestGetOperationsWithSpanKind(t *testing.T) {
	f := badger.NewFactory()
	f.Config.Ephemeral = true
	err := f.Initialize(metrics.NullFactory, zap.NewNop())
	require.NoError(t, err)
	defer f.Close()

	sw, err := f.CreateSpanWriter()
	require.NoError(t, err)

	sr, err := f.CreateSpanReader()
	require.NoError(t, err)

	tid := time.Now()
	serviceName := "test-service"

	spans := []struct {
		operation string
		kind      string
	}{
		{"op1", "server"},
		{"op2", "client"},
		{"op3", ""},
	}

	for i, s := range spans {
		span := &model.Span{
			TraceID: model.TraceID{Low: uint64(i + 1), High: 1},
			SpanID:  model.SpanID(i + 1),
			Process: &model.Process{ServiceName: serviceName},
			OperationName: s.operation,
			StartTime: tid,
		}
		if s.kind != "" {
			span.Tags = model.KeyValues{
				model.KeyValue{Key: "span.kind", VStr: s.kind, VType: model.StringType},
			}
		}
		err = sw.WriteSpan(context.Background(), span)
		require.NoError(t, err)
	}

	testCases := []struct {
		name     string
		kind     string
		expected []spanstore.Operation
	}{
		{
			name: "all operations",
			kind: "",
			expected: []spanstore.Operation{
				{Name: "op1", SpanKind: "server"},
				{Name: "op2", SpanKind: "client"},
				{Name: "op3", SpanKind: ""},
			},
		},
		{
			name: "server operations",
			kind: "server",
			expected: []spanstore.Operation{
				{Name: "op1", SpanKind: "server"},
			},
		},
		{
			name: "client operations",
			kind: "client",
			expected: []spanstore.Operation{
				{Name: "op2", SpanKind: "client"},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ops, err := sr.GetOperations(context.Background(), spanstore.OperationQueryParameters{
				ServiceName: serviceName,
				SpanKind:    tc.kind,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.expected, ops)
		})
	}
}
