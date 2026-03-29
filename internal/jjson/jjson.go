// Copyright (c) 2024 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package jjson

import (
	"encoding/json"
	"fmt"
)

// Unmarshal unmarshals data into v and returns a wrapped error with the provided description.
func Unmarshal(data []byte, v any, desc string) error {
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("failed to unmarshal %s: %w", desc, err)
	}
	return nil
}
