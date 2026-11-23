// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/internal/mcptools/internal/types"
	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerquery/querysvc"
	v1badger "github.com/jaegertracing/jaeger/internal/storage/v1/badger"
	v2badger "github.com/jaegertracing/jaeger/internal/storage/v2/badger"
	"github.com/jaegertracing/jaeger/internal/storage/v2/v1adapter"
	"github.com/jaegertracing/jaeger/internal/telemetry"
)

// maxDependencyTraces matches the Badger trace reader's default search limit.
// get_service_dependencies forwards any start/end the tool caller supplies
// and does not cap the window before Badger lifts its trace limit.
const maxDependencyTraces = 100

// TestMCPGetDependenciesDoesNotMaterializeEveryBadgerTrace fails while the
// MCP dependency tool, given a multi-year window, returns one edge for every
// trace stored in Badger.
func TestMCPGetDependenciesDoesNotMaterializeEveryBadgerTrace(t *testing.T) {
	cfg := *v1badger.DefaultConfig()
	cfg.Ephemeral = true
	cfg.SyncWrites = false
	factory, err := v2badger.NewFactory(cfg, telemetry.NoopSettings())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, factory.Close())
	})
	traceWriter, err := factory.CreateTraceWriter()
	require.NoError(t, err)
	dependencyReader, err := factory.CreateDependencyReader()
	require.NoError(t, err)
	writer := v1adapter.GetV1Writer(traceWriter)

	end := time.Now().Add(time.Minute).UTC()
	const stored = maxDependencyTraces + 50
	for i := range stored {
		traceID := model.TraceID{Low: uint64(i + 1), High: 13}
		parent := &model.Span{
			TraceID:       traceID,
			SpanID:        1,
			OperationName: "request",
			Process:       &model.Process{ServiceName: "frontend"},
			StartTime:     end.Add(-time.Hour).Add(time.Duration(i) * time.Millisecond),
			Duration:      time.Millisecond,
		}
		child := &model.Span{
			TraceID:       traceID,
			SpanID:        2,
			OperationName: "request",
			Process:       &model.Process{ServiceName: "payments-" + strconv.Itoa(i)},
			StartTime:     parent.StartTime,
			Duration:      time.Millisecond,
			References:    []model.SpanRef{model.NewChildOfRef(traceID, parent.SpanID)},
		}
		require.NoError(t, writer.WriteSpan(context.Background(), parent))
		require.NoError(t, writer.WriteSpan(context.Background(), child))
	}

	qs := querysvc.NewQueryService(nil, dependencyReader, querysvc.QueryServiceOptions{})
	_, output, err := NewGetDependenciesHandler(qs)(context.Background(), nil, types.GetDependenciesInput{
		StartTime: end.Add(-10 * 365 * 24 * time.Hour).Format(time.RFC3339),
		EndTime:   end.Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	require.LessOrEqualf(t, len(output.Dependencies), maxDependencyTraces,
		"the MCP dependency tool returned %d edges from %d stored traces", len(output.Dependencies), stored)
}
