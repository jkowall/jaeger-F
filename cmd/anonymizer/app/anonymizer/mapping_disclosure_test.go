// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package anonymizer

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger-idl/model/v1"
)

// TestSaveMappingOmitsIdentifierMapFromLog covers SaveMapping, the function
// that both writer.Close and the periodic goroutine started by New call.
// The logger is built the same way as the command: zap.NewDevelopment, which
// writes info lines to standard error. The log may include the mapping file
// path. It must not include the JSON body or the original names.
func TestSaveMappingOmitsIdentifierMapFromLog(t *testing.T) {
	const serviceName = "ledgercheckoutservice"
	const operationName = "postjournalentry"

	logger, readStderr := developmentLoggerOnPipe(t)
	mappingFile := filepath.Join(t.TempDir(), "mapping.json")
	a := New(mappingFile, Options{}, logger)
	t.Cleanup(a.Stop)

	_ = a.AnonymizeSpan(&model.Span{
		TraceID:       model.NewTraceID(1, 2),
		SpanID:        model.NewSpanID(1),
		OperationName: operationName,
		Process: &model.Process{
			ServiceName: serviceName,
		},
		StartTime: time.Unix(300, 0),
		Duration:  time.Second,
	})
	a.SaveMapping()
	a.Stop()

	logs := readStderr()
	info, err := os.Stat(mappingFile)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	body, err := os.ReadFile(mappingFile)
	require.NoError(t, err)
	require.Contains(t, string(body), serviceName)
	require.Contains(t, string(body), operationName)

	require.Contains(t, logs, mappingFile)
	require.NotContains(t, logs, serviceName)
	require.NotContains(t, logs, operationName)
	require.NotContains(t, logs, string(body))
}

// developmentLoggerOnPipe builds zap.NewDevelopment while os.Stderr is a pipe.
// NewDevelopment opens the "stderr" sink during Build, so the swap has to
// happen first.
func developmentLoggerOnPipe(t *testing.T) (*zap.Logger, func() string) {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stderr
	os.Stderr = w
	t.Cleanup(func() {
		os.Stderr = orig
		_ = r.Close()
	})

	logger, err := zap.NewDevelopment()
	require.NoError(t, err)

	return logger, func() string {
		t.Helper()
		_ = logger.Sync()
		os.Stderr = orig
		require.NoError(t, w.Close())
		out, err := io.ReadAll(r)
		require.NoError(t, err)
		return string(out)
	}
}
