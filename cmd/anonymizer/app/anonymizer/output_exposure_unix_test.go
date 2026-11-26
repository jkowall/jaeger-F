// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package anonymizer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const exposureSentinel = "do-not-follow-symlink"

func TestMappingFileSymlinkIsNotFollowed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "mapping.json")
	sentinel := []byte(exposureSentinel)
	require.NoError(t, os.WriteFile(target, sentinel, 0o644))
	require.NoError(t, os.Symlink(target, link))

	saveExposureMapping(link)
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, sentinel, got)
}

func saveExposureMapping(path string) {
	a := &Anonymizer{
		logger: zap.NewNop(),
		mapping: mapping{
			Services:   map[string]string{"secret-service": "hashed-service"},
			Operations: map[string]string{},
		},
		mappingFile: path,
	}
	a.SaveMapping()
}
