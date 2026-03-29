// Copyright (c) 2024 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package jjson

import (
	"testing"
	"github.com/stretchr/testify/assert"
)

func TestUnmarshal(t *testing.T) {
	type testStruct struct {
		Foo string `json:"foo"`
	}

	t.Run("success", func(t *testing.T) {
		var v testStruct
		err := Unmarshal([]byte(`{"foo":"bar"}`), &v, "test")
		assert.NoError(t, err)
		assert.Equal(t, "bar", v.Foo)
	})

	t.Run("failure", func(t *testing.T) {
		var v testStruct
		err := Unmarshal([]byte(`{"foo":1}`), &v, "test")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to unmarshal test")
	})
}
