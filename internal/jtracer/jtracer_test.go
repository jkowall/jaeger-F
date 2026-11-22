// Copyright (c) 2023 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package jtracer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/jaegertracing/jaeger/internal/testutils"
)

func TestNewProvider(t *testing.T) {
	p, c, err := NewProvider(t.Context(), "serviceName")
	require.NoError(t, err)
	require.NotNil(t, p, "Expected OTEL not to be nil")
	require.NotNil(t, c, "Expected closer not to be nil")
	c(t.Context())
}

func TestNewHelperProviderError(t *testing.T) {
	fakeErr := errors.New("fakeProviderError")
	_, _, err := newProviderHelper(
		t.Context(),
		"svc",
		func(_ context.Context, _ /* svc */ string) (*sdktrace.TracerProvider, func(), error) {
			return nil, nil, fakeErr
		},
	)
	require.Error(t, err)
	require.EqualError(t, err, fakeErr.Error())
}

func TestInitHelperExporterError(t *testing.T) {
	fakeErr := errors.New("fakeExporterError")
	_, _, err := initHelper(
		context.Background(),
		"svc",
		func(_ context.Context) (sdktrace.SpanExporter, error) {
			return nil, fakeErr
		},
		func(_ context.Context, _ /* svc */ string) (*resource.Resource, error) {
			return nil, nil
		},
	)
	require.Error(t, err)
	require.EqualError(t, err, fakeErr.Error())
}

func TestInitHelperResourceError(t *testing.T) {
	fakeErr := errors.New("fakeResourceError")
	tp, _, err := initHelper(
		context.Background(),
		"svc",
		otelExporter,
		func(_ context.Context, _ /* svc */ string) (*resource.Resource, error) {
			return nil, fakeErr
		},
	)
	require.Error(t, err)
	require.Nil(t, tp)
	require.EqualError(t, err, fakeErr.Error())
}

func clearOTLPTargetEnv(t *testing.T) {
	t.Helper()
	for _, key := range otlpTargetEnv {
		t.Setenv(key, "")
	}
}

func TestExporterOptionsLoopbackDefault(t *testing.T) {
	clearOTLPTargetEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "  ")
	require.True(t, usesLoopbackDefault())
	require.Len(t, GRPCOptions(), 2)
	require.Len(t, HTTPOptions(), 2)
}

func TestExporterOptionsDeferToConfiguredTarget(t *testing.T) {
	for _, key := range otlpTargetEnv {
		t.Run(key, func(t *testing.T) {
			clearOTLPTargetEnv(t)
			t.Setenv(key, "configured")
			require.False(t, usesLoopbackDefault())
			require.Empty(t, GRPCOptions())
			require.Empty(t, HTTPOptions())
		})
	}
}

func TestMain(m *testing.M) {
	testutils.VerifyGoLeaks(m)
}
