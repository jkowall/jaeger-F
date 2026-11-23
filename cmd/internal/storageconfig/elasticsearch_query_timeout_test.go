// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package storageconfig

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/confmap"
)

// maxDefaultQueryTimeout matches the bound enforced for DefaultConfig. The
// storage loader starts from that default and overlays the operator's file, so
// a configuration that never sets query_timeout — including the Elasticsearch
// sample — must still inherit a non-zero deadline.
const maxDefaultQueryTimeout = 30 * time.Second

func TestElasticsearchConfigWithoutQueryTimeoutIsBounded(t *testing.T) {
	for _, backend := range []string{"elasticsearch", "opensearch"} {
		t.Run(backend, func(t *testing.T) {
			conf := confmap.NewFromStringMap(map[string]any{
				backend: map[string]any{
					"server_urls": []any{"http://127.0.0.1:9200"},
				},
			})
			var cfg TraceBackend
			require.NoError(t, cfg.Unmarshal(conf))
			got := cfg.Elasticsearch
			if backend == "opensearch" {
				got = cfg.Opensearch
			}
			require.NotNil(t, got)
			require.Greater(t, got.QueryTimeout, time.Duration(0),
				"%s configuration that omits query_timeout inherits a zero timeout", backend)
			require.LessOrEqual(t, got.QueryTimeout, maxDefaultQueryTimeout)
		})
	}
}
