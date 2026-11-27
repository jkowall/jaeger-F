// Copyright (c) 2022 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/viper"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/config/configtls"
	"go.opentelemetry.io/collector/featuregate"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/internal/metrics"
	"github.com/jaegertracing/jaeger/ports"
)

// ServiceConfig holds the settings of the service itself, as opposed to the storage
// it serves: the admin server, logging and the metrics backend.
type ServiceConfig struct {
	Admin   AdminServerConfig `mapstructure:"admin"`
	Logging LoggingConfig     `mapstructure:"logging"`
	Metrics MetricsConfig     `mapstructure:"metrics"`
}

// AdminServerConfig is the admin section of the configuration file. It names only what
// the admin server honors, the endpoint, TLS and profiling, and has no Unmarshal hook of
// its own, so a misspelled key is rejected like anywhere else in the file.
// confighttp.ServerConfig would not do: its hook ignores keys it does not know, and it
// carries authentication and CORS settings the admin server has no host to serve them with.
type AdminServerConfig struct {
	// Endpoint is the host:port the admin server listens on.
	Endpoint string `mapstructure:"endpoint"`
	// TLS, when set, serves the admin endpoints over HTTPS.
	TLS configoptional.Optional[configtls.ServerConfig] `mapstructure:"tls"`
	// Profiling, when true, mounts the Go profiling handlers under /debug/pprof/ and expvar
	// on /debug/vars. They are off by default.
	Profiling bool `mapstructure:"profiling"`
}

// Validate refuses profiling on a listener that any host could call without credentials.
// The admin server has no authenticator, so profiling needs either a loopback endpoint or
// TLS that requires a client certificate.
func (c AdminServerConfig) Validate() error {
	if !c.Profiling || isLoopbackEndpoint(c.Endpoint) || c.requiresClientCert() {
		return nil
	}
	return fmt.Errorf(
		"admin.profiling requires a loopback admin.endpoint (such as localhost:%d) or admin.tls with client_ca_file; endpoint %q is reachable from other hosts",
		ports.RemoteStorageAdminHTTP, c.Endpoint,
	)
}

func (c AdminServerConfig) requiresClientCert() bool {
	return c.TLS.HasValue() && c.TLS.Get().ClientCAFile != ""
}

func isLoopbackEndpoint(endpoint string) bool {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DefaultServiceConfig returns the settings the service runs with when its configuration
// file does not name them: an admin server on adminPort, info-level JSON logs, and
// Prometheus metrics on /metrics.
func DefaultServiceConfig(adminPort int) ServiceConfig {
	return ServiceConfig{
		Admin: AdminServerConfig{Endpoint: ports.PortToHostPort(adminPort)},
		Logging: LoggingConfig{
			Level:    "info",
			Encoding: "json",
		},
		Metrics: DefaultMetricsConfig(),
	}
}

// Service represents an abstract Jaeger backend component with some basic shared functionality.
type Service struct {
	// Admin is the admin server that hosts the health check and metrics endpoints.
	Admin *AdminServer

	// Logger is initialized by Start from the logging configuration.
	Logger *zap.Logger

	// MetricsFactory is the root factory without a namespace.
	MetricsFactory metrics.Factory

	signalsChannel chan os.Signal
}

// NewService creates a new Service.
func NewService() *Service {
	signalsChannel := make(chan os.Signal, 1)
	signal.Notify(signalsChannel, os.Interrupt, syscall.SIGTERM)

	return &Service{
		Admin:          NewAdminServer(),
		signalsChannel: signalsChannel,
	}
}

const configFile = "config-file"

// AddConfigFileFlag registers the --config-file flag.
func AddConfigFileFlag(flagSet *flag.FlagSet) {
	flagSet.String(configFile, "", "Path to the YAML configuration file (default none).")
}

// ConfigFile returns the path given with --config-file, or an empty string when none was.
func ConfigFile(v *viper.Viper) string {
	return v.GetString(configFile)
}

// AddFlags registers the CLI flags: the configuration file, which carries every other
// setting, and the feature gates.
func (*Service) AddFlags(flagSet *flag.FlagSet) {
	AddConfigFileFlag(flagSet)
	featuregate.GlobalRegistry().RegisterFlags(flagSet)
}

// Start bootstraps the service from its configuration and starts the admin server.
func (s *Service) Start(cfg ServiceConfig) error {
	newProdConfig := zap.NewProductionConfig()
	newProdConfig.Sampling = nil
	logger, err := cfg.Logging.NewLogger(newProdConfig)
	if err != nil {
		return fmt.Errorf("cannot create logger: %w", err)
	}
	s.Logger = logger

	metricsBuilder := cfg.Metrics
	metricsFactory, err := metricsBuilder.CreateMetricsFactory("")
	if err != nil {
		return fmt.Errorf("cannot create metrics factory: %w", err)
	}
	s.MetricsFactory = metricsFactory

	if err := cfg.Admin.Validate(); err != nil {
		return fmt.Errorf("invalid admin configuration: %w", err)
	}
	s.Admin.configure(cfg.Admin, s.Logger)
	if h := metricsBuilder.Handler(); h != nil {
		route := metricsBuilder.HTTPRoute
		s.Logger.Info("Mounting metrics handler on admin server", zap.String("route", route))
		s.Admin.Handle(route, h)
	}

	if err := s.Admin.Serve(); err != nil {
		return fmt.Errorf("cannot start the admin server: %w", err)
	}

	return nil
}

// RunAndThen sets the health check to Ready and blocks until SIGTERM is received.
// It then runs the shutdown function and exits.
func (s *Service) RunAndThen(shutdown func()) error {
	s.Admin.Host().Ready()

	<-s.signalsChannel

	s.Logger.Info("Shutting down")
	s.Admin.Host().SetUnavailable()

	if shutdown != nil {
		shutdown()
	}

	err := s.Admin.Close()
	if err == nil {
		s.Logger.Info("Shutdown complete")
	}
	return err
}
