// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package uiconv

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/cmd/anonymizer/app/internal/symlinkrace"
)

func exposureChild() bool {
	if os.Getenv("JAEGER_ANON_EXPOSURE_CHILD") != "1" {
		return false
	}
	link := os.Getenv("JAEGER_ANON_EXPOSURE_LINK")
	input := os.Getenv("JAEGER_ANON_EXPOSURE_OTHER")
	status := os.Getenv("JAEGER_ANON_EXPOSURE_STATUS")
	err := Extract(Config{
		CapturedFile: input,
		UIFile:       link,
		TraceID:      exposureTraceID,
	}, zap.NewNop())
	msg := "ok"
	if err != nil {
		msg = "error: " + err.Error()
	}
	_ = os.WriteFile(status, []byte(msg), 0o600)
	return true
}

func TestUIOpenIsAtomicAgainstSymlinkSwap(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "ui.json")
	statusPath := filepath.Join(dir, "status")
	sentinel := []byte(exposureSentinel)
	require.NoError(t, os.WriteFile(target, sentinel, 0o644))
	input, err := filepath.Abs("fixtures/trace_success.json")
	require.NoError(t, err)

	res, err := symlinkrace.PlantBeforeOpen(os.Args[0], nil, symlinkrace.ChildEnv(
		"JAEGER_ANON_EXPOSURE_CHILD=1",
		"JAEGER_ANON_EXPOSURE_LINK="+link,
		"JAEGER_ANON_EXPOSURE_OTHER="+input,
		"JAEGER_ANON_EXPOSURE_STATUS="+statusPath,
	), link, target)
	require.NoError(t, err, "tracer output: %s", res.Output)
	require.Equal(t, 0, res.ExitCode, "child output: %s", res.Output)
	require.True(t, res.Planted, "open of %s was not observed; child output: %s", link, res.Output)

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, sentinel, got)
	status, err := os.ReadFile(statusPath)
	require.NoError(t, err)
	require.Contains(t, string(status), "error")
}
