// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

// Package symlinkrace lets tests place a symlink at an output path in the
// window after any earlier stat and before the kernel runs openat.
package symlinkrace
