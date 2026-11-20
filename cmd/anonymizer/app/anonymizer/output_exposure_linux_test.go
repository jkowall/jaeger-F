// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package anonymizer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jaegertracing/jaeger/cmd/anonymizer/app/internal/symlinkrace"
)

func exposureChild() bool {
	if os.Getenv("JAEGER_ANON_EXPOSURE_CHILD") != "1" {
		return false
	}
	saveExposureMapping(os.Getenv("JAEGER_ANON_EXPOSURE_LINK"))
	_ = os.WriteFile(os.Getenv("JAEGER_ANON_EXPOSURE_STATUS"), []byte("done"), 0o600)
	return true
}

func TestMappingOpenIsAtomicAgainstSymlinkSwap(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "mapping.json")
	statusPath := filepath.Join(dir, "status")
	sentinel := []byte(exposureSentinel)
	require.NoError(t, os.WriteFile(target, sentinel, 0o644))

	res, err := symlinkrace.PlantBeforeOpen(os.Args[0], nil, symlinkrace.ChildEnv(
		"JAEGER_ANON_EXPOSURE_CHILD=1",
		"JAEGER_ANON_EXPOSURE_LINK="+link,
		"JAEGER_ANON_EXPOSURE_STATUS="+statusPath,
	), link, target)
	require.NoError(t, err, "tracer output: %s", res.Output)
	require.Equal(t, 0, res.ExitCode, "child output: %s", res.Output)
	require.True(t, res.Planted, "open of %s was not observed; child output: %s", link, res.Output)

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, sentinel, got)
}
