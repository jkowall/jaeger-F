// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger/cmd/anonymizer/app/writer"
)

// TestIdentifierMapIsNotWrittenToStderr runs the command path that shares a
// trace: the process logger from main (zap.NewDevelopment), writer.WriteSpan,
// and writer.Close. Close saves the identifier map. Stderr may name the
// mapping file. It must not contain the original service or operation names,
// because those names are what a reader needs to reverse the anonymized file.
func TestIdentifierMapIsNotWrittenToStderr(t *testing.T) {
	const serviceName = "ledgercheckoutservice"
	const operationName = "postjournalentry"

	readStderr := redirectProcessStderr(t)

	dir := t.TempDir()
	mappingFile := filepath.Join(dir, "mapping.json")
	anonymizedFile := filepath.Join(dir, "anonymized.json")
	w, err := writer.New(writer.Config{
		CapturedFile:   filepath.Join(dir, "captured.json"),
		AnonymizedFile: anonymizedFile,
		MappingFile:    mappingFile,
	}, logger)
	require.NoError(t, err)
	t.Cleanup(w.Close)

	err = w.WriteSpan(&model.Span{
		TraceID:       model.NewTraceID(1, 2),
		SpanID:        model.NewSpanID(1),
		OperationName: operationName,
		Process: &model.Process{
			ServiceName: serviceName,
		},
		StartTime: time.Unix(300, 0),
		Duration:  time.Second,
	})
	require.NoError(t, err)
	w.Close()

	logs := readStderr()
	anonymized, err := os.ReadFile(anonymizedFile)
	require.NoError(t, err)
	mappingBytes, err := os.ReadFile(mappingFile)
	require.NoError(t, err)
	info, err := os.Stat(mappingFile)
	require.NoError(t, err)

	var mapping struct {
		Services   map[string]string
		Operations map[string]string
	}
	require.NoError(t, json.Unmarshal(mappingBytes, &mapping))
	serviceHash := mapping.Services[serviceName]
	operationHash := mapping.Operations["["+serviceName+"]:"+operationName]
	require.NotEmpty(t, serviceHash)
	require.NotEmpty(t, operationHash)
	require.NotContains(t, string(anonymized), serviceName)
	require.NotContains(t, string(anonymized), operationName)
	require.Contains(t, string(anonymized), serviceHash)
	require.Contains(t, string(anonymized), operationHash)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// The mapping file is the only copy of the reverse map.
	require.Contains(t, logs, mappingFile)
	require.NotContains(t, logs, serviceName)
	require.NotContains(t, logs, operationName)
	require.NotContains(t, logs, serviceHash)
}

// redirectProcessStderr points file descriptor 2 at a pipe. The logger in
// main is built with zap.NewDevelopment during package init, and that logger
// already holds os.Stderr, so replacing the os.Stderr variable would miss it.
func redirectProcessStderr(t *testing.T) func() string {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)
	saved, err := unix.Dup(int(os.Stderr.Fd()))
	require.NoError(t, err)
	require.NoError(t, unix.Dup2(int(w.Fd()), int(os.Stderr.Fd())))
	require.NoError(t, w.Close())

	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		_ = unix.Dup2(saved, int(os.Stderr.Fd()))
		_ = unix.Close(saved)
	}
	t.Cleanup(func() {
		restore()
		_ = r.Close()
	})

	return func() string {
		t.Helper()
		// Sync flushes zap's writer. A pipe has nothing to sync; ignore that error.
		_ = logger.Sync()
		restore()
		out, err := io.ReadAll(r)
		require.NoError(t, err)
		return string(out)
	}
}
