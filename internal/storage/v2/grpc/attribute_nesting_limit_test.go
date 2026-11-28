// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package grpc

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"

	"github.com/jaegertracing/jaeger/internal/proto-gen/storage/v2"
)

// TestToTraceQueryParamsBoundsAttributeNesting pins the conversion that runs after a query
// has decoded. Nesting of 32 must round-trip into the reader parameters. Nesting of 64 must
// be rejected before a reader would walk it. Array and key-value-list wrappers both count,
// including when they alternate. This fails while setValueToMap and setValueToSlice recurse
// with no depth limit.
func TestToTraceQueryParamsBoundsAttributeNesting(t *testing.T) {
	handler := NewHandler(nil, nil, nil)
	for _, shape := range []string{"array", "kvlist", "mixed"} {
		t.Run(shape, func(t *testing.T) {
			params, err := handler.toTraceQueryParams(queryWithNesting(shape, 32))
			require.NoError(t, err)
			require.Equal(t, 32, pdataAttributeNesting(params.Attributes))

			_, err = handler.toTraceQueryParams(queryWithNesting(shape, 64))
			require.Error(t, err)
		})
	}
}

func queryWithNesting(shape string, depth int) *storage.TraceQueryParameters {
	return &storage.TraceQueryParameters{
		SearchDepth: 1,
		Attributes: []*storage.KeyValue{{
			Key:   "k",
			Value: nestedAttributeValue(shape, depth),
		}},
	}
}

func nestedAttributeValue(shape string, depth int) *storage.AnyValue {
	av := &storage.AnyValue{Value: &storage.AnyValue_StringValue{StringValue: "x"}}
	for i := range depth {
		useArray := shape == "array" || (shape == "mixed" && i%2 == 0)
		if useArray {
			av = &storage.AnyValue{Value: &storage.AnyValue_ArrayValue{ArrayValue: &storage.ArrayValue{
				Values: []*storage.AnyValue{av},
			}}}
			continue
		}
		av = &storage.AnyValue{Value: &storage.AnyValue_KvlistValue{KvlistValue: &storage.KeyValueList{
			Values: []*storage.KeyValue{{Key: "k", Value: av}},
		}}}
	}
	return av
}

func pdataAttributeNesting(m pcommon.Map) int {
	depth := -1
	m.Range(func(_ string, v pcommon.Value) bool {
		depth = pdataValueNesting(v)
		return false
	})
	return depth
}

func pdataValueNesting(v pcommon.Value) int {
	switch v.Type() {
	case pcommon.ValueTypeSlice:
		s := v.Slice()
		if s.Len() == 0 {
			return 1
		}
		return 1 + pdataValueNesting(s.At(0))
	case pcommon.ValueTypeMap:
		child := 0
		v.Map().Range(func(_ string, inner pcommon.Value) bool {
			child = pdataValueNesting(inner)
			return false
		})
		return 1 + child
	default:
		return 0
	}
}
