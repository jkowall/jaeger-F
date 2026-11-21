// Copyright (c) 2022 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"flag"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/config/configtls"
	"go.opentelemetry.io/collector/confmap"

	"github.com/jaegertracing/jaeger/internal/config"
)

func TestAddFlags(*testing.T) {
	s := NewService()
	s.AddFlags(new(flag.FlagSet))
}

func TestConfigFile(t *testing.T) {
	v, cmd := config.Viperize(AddConfigFileFlag)
	assert.Empty(t, ConfigFile(v))
	require.NoError(t, cmd.ParseFlags([]string{"--config-file=/tmp/config.yaml"}))
	assert.Equal(t, "/tmp/config.yaml", ConfigFile(v))
}

func TestDefaultServiceConfigUnmarshal(t *testing.T) {
	cfg := DefaultServiceConfig(0)
	conf := confmap.NewFromStringMap(map[string]any{
		"admin":   map[string]any{"endpoint": ":18000"},
		"logging": map[string]any{"level": "debug", "encoding": "console"},
		"metrics": map[string]any{"backend": "none"},
	})
	require.NoError(t, conf.Unmarshal(&cfg))
	assert.Equal(t, ":18000", cfg.Admin.Endpoint)
	assert.Equal(t, LoggingConfig{Level: "debug", Encoding: "console"}, cfg.Logging)
	assert.Equal(t, "none", cfg.Metrics.Backend)
	assert.Equal(t, "/metrics", cfg.Metrics.HTTPRoute, "unnamed settings keep their default")
}

func TestStartErrors(t *testing.T) {
	scenarios := []struct {
		name   string
		modify func(*ServiceConfig)
		expErr string
	}{
		{
			name:   "bad log level",
			modify: func(c *ServiceConfig) { c.Logging.Level = "invalid-log-level" },
			expErr: "cannot create logger",
		},
		{
			name:   "bad metrics backend",
			modify: func(c *ServiceConfig) { c.Metrics.Backend = "invalid-metrics-backend" },
			expErr: "cannot create metrics factory",
		},
		{
			name: "bad admin TLS",
			modify: func(c *ServiceConfig) {
				c.Admin.TLS = configoptional.Some(configtls.ServerConfig{
					Config: configtls.Config{CertFile: "invalid-cert"},
				})
			},
			expErr: "cannot start the admin server",
		},
		{
			name:   "bad host:port",
			modify: func(c *ServiceConfig) { c.Admin.Endpoint = "invalid" },
			expErr: "cannot start the admin server",
		},
		{
			name:   "profiling on a wildcard endpoint",
			modify: func(c *ServiceConfig) { c.Admin.Profiling = true },
			expErr: "invalid admin configuration",
		},
		{
			name:   "clean start",
			modify: func(*ServiceConfig) {},
		},
	}
	for _, test := range scenarios {
		t.Run(test.name, func(t *testing.T) {
			s := NewService()
			cfg := DefaultServiceConfig(0)
			test.modify(&cfg)
			err := s.Start(cfg)
			if test.expErr != "" {
				require.ErrorContains(t, err, test.expErr)
				return
			}
			require.NoError(t, err)

			var stopped atomic.Bool
			shutdown := func() {
				stopped.Store(true)
			}
			go s.RunAndThen(shutdown)

			// Give time for RunAndThen to start
			time.Sleep(100 * time.Millisecond)

			s.signalsChannel <- os.Interrupt
			waitForEqual(t, true, func() any { return stopped.Load() })
		})
	}
}

func TestAdminServerConfigValidate(t *testing.T) {
	mTLS := configoptional.Some(configtls.ServerConfig{ClientCAFile: "ca.pem"})
	serverOnlyTLS := configoptional.Some(configtls.ServerConfig{})
	tests := []struct {
		name   string
		cfg    AdminServerConfig
		expErr bool
	}{
		{name: "profiling off on wildcard", cfg: AdminServerConfig{Endpoint: ":17270"}},
		{name: "localhost", cfg: AdminServerConfig{Endpoint: "localhost:17270", Profiling: true}},
		{name: "localhost upper case", cfg: AdminServerConfig{Endpoint: "LOCALHOST:17270", Profiling: true}},
		{name: "IPv4 loopback", cfg: AdminServerConfig{Endpoint: "127.0.0.1:17270", Profiling: true}},
		{name: "IPv6 loopback", cfg: AdminServerConfig{Endpoint: "[::1]:17270", Profiling: true}},
		{name: "wildcard with client certificates", cfg: AdminServerConfig{Endpoint: ":17270", Profiling: true, TLS: mTLS}},
		{name: "empty host", cfg: AdminServerConfig{Endpoint: ":17270", Profiling: true}, expErr: true},
		{name: "IPv4 wildcard", cfg: AdminServerConfig{Endpoint: "0.0.0.0:17270", Profiling: true}, expErr: true},
		{name: "IPv6 wildcard", cfg: AdminServerConfig{Endpoint: "[::]:17270", Profiling: true}, expErr: true},
		{name: "other host name", cfg: AdminServerConfig{Endpoint: "localhost.example.com:17270", Profiling: true}, expErr: true},
		{name: "unparsable endpoint", cfg: AdminServerConfig{Endpoint: "invalid", Profiling: true}, expErr: true},
		{name: "wildcard with server-only TLS", cfg: AdminServerConfig{Endpoint: ":17270", Profiling: true, TLS: serverOnlyTLS}, expErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.cfg.Validate()
			if test.expErr {
				require.ErrorContains(t, err, "admin.profiling requires a loopback admin.endpoint")
				return
			}
			require.NoError(t, err)
		})
	}
}

func waitForEqual(t *testing.T, expected any, getter func() any) {
	for range 1000 {
		value := getter()
		if reflect.DeepEqual(value, expected) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.Equal(t, expected, getter())
}
