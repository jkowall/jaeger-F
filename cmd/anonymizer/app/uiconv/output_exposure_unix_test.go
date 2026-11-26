// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package uiconv

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const (
	exposureTraceID  = "2be38093ead7a083"
	exposureSentinel = "do-not-follow-symlink"
)

func withUmask(t *testing.T, mask int) {
	t.Helper()
	prev := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(prev) })
}

func requireOwnerOnly(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	require.NoError(t, err)
	require.Zero(t, info.Mode()&os.ModeSymlink, "%s is a symlink", path)
	perm := info.Mode().Perm()
	require.Zero(t, perm&0o077, "%s mode is %o; group or other users can read it", path, perm)
}

func TestUIFileIsOwnerOnly(t *testing.T) {
	withUmask(t, 0o022)
	ui := filepath.Join(t.TempDir(), "ui.json")
	err := Extract(Config{
		CapturedFile: "fixtures/trace_success.json",
		UIFile:       ui,
		TraceID:      exposureTraceID,
	}, zap.NewNop())
	require.NoError(t, err)
	requireOwnerOnly(t, ui)
}

func TestUIFileSymlinkIsNotFollowed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "ui.json")
	sentinel := []byte(exposureSentinel)
	require.NoError(t, os.WriteFile(target, sentinel, 0o644))
	require.NoError(t, os.Symlink(target, link))

	err := Extract(Config{
		CapturedFile: "fixtures/trace_success.json",
		UIFile:       link,
		TraceID:      exposureTraceID,
	}, zap.NewNop())
	got, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	require.Equal(t, sentinel, got)
	require.Error(t, err)
}
