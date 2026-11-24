// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package gogocodec

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	expressionproto "github.com/jaegertracing/jaeger/internal/proto/expression/v1"
)

// nestedField returns depth length-delimited fields with the given number, each holding the
// next, around leaf. It writes from the outside in, so building it takes linear time.
func nestedField(num protowire.Number, depth int, leaf []byte) []byte {
	sizes := make([]int, depth+1)
	sizes[0] = len(leaf)
	for i := 1; i <= depth; i++ {
		sizes[i] = protowire.SizeTag(num) + protowire.SizeBytes(sizes[i-1])
	}
	out := make([]byte, 0, sizes[depth])
	for i := depth; i > 0; i-- {
		out = protowire.AppendTag(out, num, protowire.BytesType)
		out = protowire.AppendVarint(out, uint64(sizes[i-1]))
	}
	return append(out, leaf...)
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func tag(num protowire.Number, typ protowire.Type) []byte {
	return protowire.AppendTag(nil, num, typ)
}

func TestNestsDeeperThan(t *testing.T) {
	const limit = 3
	deep := nestedField(1, limit+1, nil)
	overflowingVarint := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}
	group := concat(
		tag(9, protowire.StartGroupType),
		tag(1, protowire.VarintType), []byte{1},
		tag(2, protowire.Fixed64Type), make([]byte, 8),
		tag(3, protowire.Fixed32Type), make([]byte, 4),
		tag(4, protowire.BytesType), []byte{1, 0},
		tag(5, protowire.StartGroupType), tag(5, protowire.EndGroupType),
		tag(9, protowire.EndGroupType),
	)
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{name: "empty", data: nil},
		{name: "at the limit", data: nestedField(1, limit, []byte("x"))},
		{name: "past the limit", data: deep, want: true},
		{name: "past the limit in a later field", data: concat(nestedField(1, 2, nil), nestedField(2, limit+1, nil)), want: true},
		{name: "after scalars", data: concat(tag(1, protowire.VarintType), []byte{0x96, 0x01}, tag(2, protowire.Fixed64Type), make([]byte, 8), tag(3, protowire.Fixed32Type), make([]byte, 4), deep), want: true},
		{name: "after a varint whose bits overflow", data: concat(tag(1, protowire.VarintType), overflowingVarint, deep), want: true},
		{name: "after a group", data: concat(group, deep), want: true},
		{name: "after a field that is not a message", data: concat(tag(1, protowire.BytesType), []byte{1, 0xff}, deep), want: true},
		{name: "deep only inside a group", data: concat(tag(9, protowire.StartGroupType), deep, tag(9, protowire.EndGroupType))},
		{name: "a truncated tag", data: []byte{0x80}},
		{name: "after a tag longer than ten bytes", data: concat(overflowingVarint[:9], []byte{0xff, 0x01}, deep)},
		{name: "after a varint longer than ten bytes", data: concat(tag(1, protowire.VarintType), overflowingVarint[:9], []byte{0xff, 0x01}, deep)},
		{name: "after a truncated fixed64", data: concat(tag(1, protowire.Fixed64Type), make([]byte, 7))},
		{name: "after a length past the end", data: concat(tag(1, protowire.BytesType), []byte{0x7f}, deep)},
		{name: "after a truncated length", data: concat(tag(1, protowire.BytesType), []byte{0x80})},
		{name: "after a length longer than ten bytes", data: concat(tag(1, protowire.BytesType), overflowingVarint[:9], []byte{0xff, 0x01}, deep)},
		{name: "after a negative length", data: concat(tag(1, protowire.BytesType), overflowingVarint[:9], []byte{0x01}, deep)},
		{name: "after an end group", data: concat(tag(9, protowire.EndGroupType), deep)},
		{name: "after an invalid wire type", data: concat([]byte{0x0e}, deep)},
		{name: "after an unterminated group", data: concat(tag(9, protowire.StartGroupType), deep)},
		{name: "a group with a truncated tag", data: concat(tag(9, protowire.StartGroupType), []byte{0x80})},
		{name: "after a group with a truncated varint", data: concat(tag(9, protowire.StartGroupType), tag(1, protowire.VarintType), []byte{0x80})},
		{name: "after a group with a length past the end", data: concat(tag(9, protowire.StartGroupType), tag(1, protowire.BytesType), []byte{0x7f}, deep)},
		{name: "after a group with an invalid wire type", data: concat(tag(9, protowire.StartGroupType), []byte{0x0f}, tag(9, protowire.EndGroupType), deep)},
		{name: "after a group with a truncated fixed32", data: concat(tag(9, protowire.StartGroupType), tag(1, protowire.Fixed32Type), []byte{0})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, nestsDeeperThan(test.data, limit))
		})
	}
}

func TestCodecUnmarshalRefusesDeepNesting(t *testing.T) {
	c := newCodec()
	// An unknown field of model.Span, decoded by gogo.
	err := c.Unmarshal(mem.BufferSlice{mem.SliceBuffer(nestedField(1000, maxNestingDepth+1, nil))}, &model.Span{})
	require.ErrorIs(t, err, errNestedTooDeeply)

	// A gogo-generated message outside the Jaeger proto-gen packages, decoded by protobuf-go
	// through the message's own Unmarshal.
	err = c.Unmarshal(mem.BufferSlice{mem.SliceBuffer(nestedField(2, maxNestingDepth+1, nil))}, &expressionproto.Call{})
	require.ErrorIs(t, err, errNestedTooDeeply)

	call := &expressionproto.Call{Op: "and", Args: []*expressionproto.Expression{
		{Term: &expressionproto.Expression_Call{Call: &expressionproto.Call{Op: "not"}}},
	}}
	data, err := call.Marshal()
	require.NoError(t, err)
	var decoded expressionproto.Call
	require.NoError(t, c.Unmarshal(mem.BufferSlice{mem.SliceBuffer(data)}, &decoded))
	assert.Equal(t, "not", decoded.GetArgs()[0].GetCall().GetOp())

	span := &model.Span{OperationName: "foo", TraceID: model.NewTraceID(1, 2)}
	spanData, err := c.Marshal(span)
	require.NoError(t, err)
	decodedSpan := &model.Span{}
	require.NoError(t, c.Unmarshal(spanData, decodedSpan))
	assert.Equal(t, span, decodedSpan)
}
