// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package writer

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger-idl/model/v1"
)

const exposureSentinel = "do-not-follow-symlink"

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

func originalTraceSpan() *model.Span {
	return &model.Span{
		TraceID:       model.NewTraceID(1, 2),
		SpanID:        model.NewSpanID(1),
		OperationName: "secret-operation",
		Process: &model.Process{
			ServiceName: "secret-service",
		},
		Tags: []model.KeyValue{
			model.String("customer.token", "secret-tag-value"),
		},
		Logs: []model.Log{{
			Timestamp: time.Unix(0, 0).UTC(),
			Fields: []model.KeyValue{
				model.String("logKey", "secret-log-value"),
			},
		}},
		Duration:  time.Second,
		StartTime: time.Unix(0, 0).UTC(),
	}
}

func TestCapturedFileHoldsOriginalTraceAndIsOwnerOnly(t *testing.T) {
	withUmask(t, 0o022)
	dir := t.TempDir()
	captured := filepath.Join(dir, "captured.json")
	anonymized := filepath.Join(dir, "anonymized.json")
	w := newExposureWriter(t, captured, anonymized, filepath.Join(dir, "mapping.json"))
	require.NoError(t, w.WriteSpan(originalTraceSpan()))
	w.Close()

	body, err := os.ReadFile(captured)
	require.NoError(t, err)
	text := string(body)
	require.Contains(t, text, "secret-service")
	require.Contains(t, text, "secret-tag-value")
	require.Contains(t, text, "secret-log-value")
	requireOwnerOnly(t, captured)
}

func TestAnonymizedFileIsOwnerOnly(t *testing.T) {
	withUmask(t, 0o022)
	dir := t.TempDir()
	anonymized := filepath.Join(dir, "anonymized.json")
	w := newExposureWriter(t, filepath.Join(dir, "captured.json"), anonymized, filepath.Join(dir, "mapping.json"))
	require.NoError(t, w.WriteSpan(originalTraceSpan()))
	w.Close()
	requireOwnerOnly(t, anonymized)
}

func TestCapturedFileSymlinkIsNotFollowed(t *testing.T) {
	assertSymlinkIgnored(t, "captured")
}

func TestAnonymizedFileSymlinkIsNotFollowed(t *testing.T) {
	assertSymlinkIgnored(t, "anonymized")
}

func assertSymlinkIgnored(t *testing.T, role string) {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, role+".json")
	other := filepath.Join(dir, "other.json")
	sentinel := []byte(exposureSentinel)
	require.NoError(t, os.WriteFile(target, sentinel, 0o644))
	require.NoError(t, os.Symlink(target, link))

	captured, anonymized := other, link
	if role == "captured" {
		captured, anonymized = link, other
	}
	w, err := New(Config{
		CapturedFile:   captured,
		AnonymizedFile: anonymized,
		MappingFile:    filepath.Join(dir, "mapping.json"),
	}, zap.NewNop())
	if w != nil {
		t.Cleanup(w.Close)
	}
	got, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	require.Equal(t, sentinel, got)
	require.Error(t, err)
}

func newExposureWriter(t *testing.T, captured, anonymized, mapping string) *Writer {
	t.Helper()
	w, err := New(Config{
		CapturedFile:   captured,
		AnonymizedFile: anonymized,
		MappingFile:    mapping,
	}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(w.Close)
	return w
}
