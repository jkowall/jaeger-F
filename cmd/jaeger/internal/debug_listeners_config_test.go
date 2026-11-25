// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package internal

import (
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShippedConfigsKeepDebugListenersOnLoopback(t *testing.T) {
	t.Setenv("JAEGER_LISTEN_HOST", "0.0.0.0")
	debugExtensions := map[string]bool{"expvar": true, "zpages": true, "pprof": true}

	files, err := filepath.Glob("../config*.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	files = append(files, "all-in-one.yaml")

	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			conf := resolveBatchingConfig(t, file)
			extensions, _ := conf.Get("extensions").(map[string]any)
			checked := 0
			for id, raw := range extensions {
				extType, _, _ := strings.Cut(id, "/")
				if !debugExtensions[extType] {
					continue
				}
				cfg, _ := raw.(map[string]any)
				endpoint, ok := cfg["endpoint"].(string)
				if !ok {
					continue
				}
				host, _, err := net.SplitHostPort(endpoint)
				require.NoError(t, err)
				assert.Containsf(t, []string{"localhost", "127.0.0.1", "::1"}, host,
					"extension %s must not listen on %q", id, endpoint)
				checked++
			}
			if file == "all-in-one.yaml" {
				assert.Equal(t, 2, checked, "expected expvar and zpages endpoints in the embedded config")
			}
		})
	}
}
