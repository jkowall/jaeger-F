// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package writer

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
	role := os.Getenv("JAEGER_ANON_EXPOSURE_ROLE")
	link := os.Getenv("JAEGER_ANON_EXPOSURE_LINK")
	other := os.Getenv("JAEGER_ANON_EXPOSURE_OTHER")
	status := os.Getenv("JAEGER_ANON_EXPOSURE_STATUS")
	_ = os.WriteFile(status, []byte(childOpen(role, link, other)), 0o600)
	return true
}

func childOpen(role, link, other string) string {
	captured, anonymized := other, link
	if role == "captured" {
		captured, anonymized = link, other
	}
	w, err := New(Config{
		CapturedFile:   captured,
		AnonymizedFile: anonymized,
		MappingFile:    filepath.Join(filepath.Dir(link), "mapping.json"),
	}, zap.NewNop())
	if err != nil {
		return "error: " + err.Error()
	}
	defer w.Close()
	if err := w.WriteSpan(originalTraceSpan()); err != nil {
		return "error: " + err.Error()
	}
	return "ok"
}

func TestCapturedOpenIsAtomicAgainstSymlinkSwap(t *testing.T) {
	assertOpenAtomic(t, "captured")
}

func TestAnonymizedOpenIsAtomicAgainstSymlinkSwap(t *testing.T) {
	assertOpenAtomic(t, "anonymized")
}

func assertOpenAtomic(t *testing.T, role string) {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, role+".json")
	other := filepath.Join(dir, "other.json")
	statusPath := filepath.Join(dir, "status")
	sentinel := []byte(exposureSentinel)
	require.NoError(t, os.WriteFile(target, sentinel, 0o644))

	res, err := symlinkrace.PlantBeforeOpen(os.Args[0], nil, symlinkrace.ChildEnv(
		"JAEGER_ANON_EXPOSURE_CHILD=1",
		"JAEGER_ANON_EXPOSURE_ROLE="+role,
		"JAEGER_ANON_EXPOSURE_LINK="+link,
		"JAEGER_ANON_EXPOSURE_OTHER="+other,
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
