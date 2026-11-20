// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package privatefile

import (
	"os"
)

// Create opens path for writing as an empty file readable only by its owner,
// creating it if it does not exist.
func Create(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
}
