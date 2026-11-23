// Copyright (c) 2020 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"flag"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.opentelemetry.io/collector/config/configtls"

	"github.com/jaegertracing/jaeger/internal/config/tlscfg"
)

var queryTLSFlagsCfg = tlscfg.ClientFlagsConfig{Prefix: "query"}

// Options represent configurable parameters for jaeger-anonymizer
type Options struct {
	QueryGRPCHostPort string
	QueryTLS          configtls.ClientConfig
	MaxSpansCount     int
	TraceID           string
	OutputDir         string
	HashStandardTags  bool
	HashCustomTags    bool
	HashLogs          bool
	HashProcess       bool
	StartTime         int64
	EndTime           int64
}

const (
	queryGRPCHostPortFlag = "query-host-port"
	outputDirFlag         = "output-dir"
	traceIDFlag           = "trace-id"
	hashStandardTagsFlag  = "hash-standard-tags"
	hashCustomTagsFlag    = "hash-custom-tags"
	hashLogsFlag          = "hash-logs"
	hashProcessFlag       = "hash-process"
	maxSpansCount         = "max-spans-count"
	startTime             = "start-time"
	endTime               = "end-time"
)

// AddFlags adds flags for anonymizer main program
func (o *Options) AddFlags(command *cobra.Command) {
	command.Flags().StringVar(
		&o.QueryGRPCHostPort,
		queryGRPCHostPortFlag,
		"localhost:16686",
		"The host:port of the jaeger-query endpoint. Without --query.tls.enabled only a loopback address is accepted",
	)
	tlsFlags := new(flag.FlagSet)
	queryTLSFlagsCfg.AddFlags(tlsFlags)
	command.Flags().AddGoFlagSet(tlsFlags)
	command.Flags().StringVar(
		&o.OutputDir,
		outputDirFlag,
		"/tmp",
		"The directory to store the anonymized trace",
	)
	command.Flags().StringVar(
		&o.TraceID,
		traceIDFlag,
		"",
		"The trace-id of trace to anonymize",
	)
	command.Flags().BoolVar(
		&o.HashStandardTags,
		hashStandardTagsFlag,
		false,
		"Whether to hash standard tags",
	)
	command.Flags().BoolVar(
		&o.HashCustomTags,
		hashCustomTagsFlag,
		false,
		"Whether to hash custom tags",
	)
	command.Flags().BoolVar(
		&o.HashLogs,
		hashLogsFlag,
		false,
		"Whether to hash logs",
	)
	command.Flags().BoolVar(
		&o.HashProcess,
		hashProcessFlag,
		false,
		"Whether to hash process",
	)
	command.Flags().IntVar(
		&o.MaxSpansCount,
		maxSpansCount,
		-1,
		"The maximum number of spans to anonymize",
	)
	command.Flags().Int64Var(
		&o.StartTime,
		startTime,
		0,
		"The start time of time window for searching trace, timestampe in unix nanoseconds",
	)
	command.Flags().Int64Var(
		&o.EndTime,
		endTime,
		0,
		"The end time of time window for searching trace, timestampe in unix nanoseconds",
	)

	// mark traceid flag as mandatory
	command.MarkFlagRequired(traceIDFlag)
}

// InitFromViper populates the TLS options from flags bound to v.
func (o *Options) InitFromViper(v *viper.Viper) error {
	tlsCfg, err := queryTLSFlagsCfg.InitFromViper(v)
	if err != nil {
		return err
	}
	o.QueryTLS = tlsCfg
	return nil
}
