// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package anonymizer

func exposureChild() bool { return false }
