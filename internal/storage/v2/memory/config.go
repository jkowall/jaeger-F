// Copyright (c) 2018 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package memory

import "github.com/asaskevich/govalidator"

const (
	defaultMaxSpansPerTrace = 1024
	defaultMaxBytesPerTrace = 1 << 20
)

// Configuration describes the options to customize the storage behavior.
type Configuration struct {
	// MaxTraces is the maximum amount of traces to store in memory.
	// Traces are stored in a ring buffer, so MaxTraces must be greater than zero.
	// If multi-tenancy is enabled, this limit applies per tenant.
	MaxTraces uint32 `mapstructure:"max_traces"`
	// MaxSpansPerTrace is the maximum number of spans stored for one trace ID.
	// Spans that arrive for a trace that is already at the limit are dropped.
	// Zero means the default of 1024.
	MaxSpansPerTrace uint32 `mapstructure:"max_spans_per_trace"`
	// MaxBytesPerTrace is the maximum protobuf-encoded size, in bytes, of the
	// spans stored for one trace ID. Spans that would take a trace over the
	// limit are dropped. Zero means the default of 1 MiB.
	MaxBytesPerTrace uint32 `mapstructure:"max_bytes_per_trace"`
}

func (c *Configuration) Validate() error {
	if c.MaxTraces == 0 {
		return errInvalidMaxTraces
	}

	_, err := govalidator.ValidateStruct(c)
	return err
}

func (c *Configuration) maxSpansPerTrace() int {
	if c.MaxSpansPerTrace == 0 {
		return defaultMaxSpansPerTrace
	}
	return int(c.MaxSpansPerTrace)
}

func (c *Configuration) maxBytesPerTrace() int {
	if c.MaxBytesPerTrace == 0 {
		return defaultMaxBytesPerTrace
	}
	return int(c.MaxBytesPerTrace)
}
