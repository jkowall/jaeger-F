// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"flag"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/cmd/es-rollover/app"
	initialize "github.com/jaegertracing/jaeger/cmd/es-rollover/app/init"
	"github.com/jaegertracing/jaeger/cmd/es-rollover/app/lookback"
	"github.com/jaegertracing/jaeger/cmd/es-rollover/app/rollover"
	"github.com/jaegertracing/jaeger/internal/config"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/esclient"
)

// placeholderUser and placeholderPassword are obvious stand-ins. They are not
// credentials for any real system.
const (
	placeholderUser     = "placeholder-user"
	placeholderPassword = "placeholder-password"
	esVersionBody       = `{"version":{"number":"8.0.0"},"tagline":"You Know, for Search"}`
	// One rolled index on the read alias, created in 1970, so lookback selects it.
	esLookbackIndicesBody = `{"jaeger-span-000001":{"aliases":{"jaeger-span-read":{}},"settings":{"index.creation_date":"1"}}}`
)

type peerHit struct {
	method        string
	path          string
	authorization bool
}

type untrustedPeer struct {
	server *httptest.Server
	mu     sync.Mutex
	hits   []peerHit
}

func newUntrustedPeer(t *testing.T) *untrustedPeer {
	t.Helper()
	p := &untrustedPeer{}
	p.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.record(r)
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		body := []byte(`{"acknowledged":true}`)
		switch {
		case r.URL.Path == "/":
			body = []byte(esVersionBody)
		case r.Method == http.MethodGet && r.URL.Path == "/jaeger-*":
			body = []byte(esLookbackIndicesBody)
		}
		_, _ = w.Write(body)
	}))
	p.server.TLS = &tls.Config{Certificates: []tls.Certificate{attackerCertificate(t)}}
	p.server.StartTLS()
	t.Cleanup(p.server.Close)
	leaf := p.server.Certificate()
	require.NotNil(t, leaf)
	require.Equal(t, "untrusted.attacker.example", leaf.Subject.CommonName)
	require.Empty(t, leaf.IPAddresses)
	return p
}

func (p *untrustedPeer) record(r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hits = append(p.hits, peerHit{
		method:        r.Method,
		path:          r.URL.Path,
		authorization: r.Header.Get("Authorization") != "",
	})
}

func (p *untrustedPeer) snapshot() []peerHit {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]peerHit, len(p.hits))
	copy(out, p.hits)
	return out
}

func (p *untrustedPeer) requireUntouched(t *testing.T) {
	t.Helper()
	hits := p.snapshot()
	if len(hits) == 0 {
		return
	}
	t.Errorf("untrusted peer received %d request(s); first was %s %s (authorization header present: %t)",
		len(hits), hits[0].method, hits[0].path, hits[0].authorization)
}

func (p *untrustedPeer) saw(method, pathPart string) bool {
	for _, hit := range p.snapshot() {
		if hit.method == method && strings.Contains(hit.path, pathPart) {
			return true
		}
	}
	return false
}

func attackerCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "untrusted.attacker.example"},
		DNSNames:     []string{"untrusted.attacker.example"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func authArgs(extra ...string) []string {
	args := []string{
		"--es.username=" + placeholderUser,
		"--es.password=" + placeholderPassword,
		"--timeout=5",
	}
	return append(args, extra...)
}

func parsedConfig(t *testing.T, args []string, inits ...func(*flag.FlagSet)) (*viper.Viper, *app.Config) {
	t.Helper()
	cfg := &app.Config{}
	flags := append([]func(*flag.FlagSet){app.AddFlags}, inits...)
	v, cmd := config.Viperize(flags...)
	require.NoError(t, cmd.ParseFlags(args))
	require.NoError(t, cfg.InitFromViper(v))
	return v, cfg
}

func TestRolloverDefaultTLSDisabledDoesNotSendCredentialsOnHTTPS(t *testing.T) {
	peer := newUntrustedPeer(t)
	v, _ := parsedConfig(t, authArgs(), (&rollover.Config{}).AddFlags)
	rolloverCfg := &rollover.Config{}
	err := app.ExecuteAction(app.ActionExecuteOptions{
		Args:   []string{peer.server.URL},
		Viper:  v,
		Logger: zap.NewNop(),
	}, func(c *esclient.Client, cfg app.Config) app.Action {
		rolloverCfg.Config = cfg
		rolloverCfg.InitFromViper(v)
		return &rollover.Action{
			IndicesClient: &esclient.IndicesClient{Client: c, MasterTimeoutSeconds: cfg.Timeout},
			Config:        *rolloverCfg,
		}
	})
	assert.Error(t, err)
	peer.requireUntouched(t)
	assert.False(t, peer.saw(http.MethodPost, "/_rollover"))
}

func TestInitDefaultTLSDisabledDoesNotSendCredentialsOnHTTPS(t *testing.T) {
	peer := newUntrustedPeer(t)
	initCfg := &initialize.Config{}
	v, _ := parsedConfig(t, authArgs(), initCfg.AddFlags)
	err := app.ExecuteAction(app.ActionExecuteOptions{
		Args:   []string{peer.server.URL},
		Viper:  v,
		Logger: zap.NewNop(),
	}, func(c *esclient.Client, cfg app.Config) app.Action {
		initCfg.Config = cfg
		initCfg.InitFromViper(v)
		return &initialize.Action{
			IndicesClient: &esclient.IndicesClient{
				Client:               c,
				MasterTimeoutSeconds: cfg.Timeout,
				Indices:              initCfg.Indices,
			},
			ILMClient: &esclient.ILMClient{Client: c, Logger: zap.NewNop()},
			Config:    *initCfg,
		}
	})
	assert.Error(t, err)
	peer.requireUntouched(t)
	assert.False(t, peer.saw(http.MethodPut, "/_index_template"))
	assert.False(t, peer.saw(http.MethodPut, "jaeger-span-000001"))
}

func TestLookbackDefaultTLSDisabledDoesNotSendCredentialsOnHTTPS(t *testing.T) {
	peer := newUntrustedPeer(t)
	lookbackCfg := &lookback.Config{}
	v, _ := parsedConfig(t, authArgs(), lookbackCfg.AddFlags)
	err := app.ExecuteAction(app.ActionExecuteOptions{
		Args:   []string{peer.server.URL},
		Viper:  v,
		Logger: zap.NewNop(),
	}, func(c *esclient.Client, cfg app.Config) app.Action {
		lookbackCfg.Config = cfg
		lookbackCfg.InitFromViper(v)
		return &lookback.Action{
			IndicesClient: &esclient.IndicesClient{Client: c, MasterTimeoutSeconds: cfg.Timeout},
			Config:        *lookbackCfg,
			Logger:        zap.NewNop(),
		}
	})
	assert.Error(t, err)
	peer.requireUntouched(t)
	assert.False(t, peer.saw(http.MethodPost, "/_aliases"))
}

// TestRolloverSkipHostVerifyStillRollsOver locks the explicit opt-in so a fix
// does not ignore --es.tls.skip-host-verify.
func TestRolloverSkipHostVerifyStillRollsOver(t *testing.T) {
	peer := newUntrustedPeer(t)
	v, _ := parsedConfig(t, authArgs("--es.tls.enabled=true", "--es.tls.skip-host-verify=true"), (&rollover.Config{}).AddFlags)
	rolloverCfg := &rollover.Config{}
	err := app.ExecuteAction(app.ActionExecuteOptions{
		Args:   []string{peer.server.URL},
		Viper:  v,
		Logger: zap.NewNop(),
	}, func(c *esclient.Client, cfg app.Config) app.Action {
		rolloverCfg.Config = cfg
		rolloverCfg.InitFromViper(v)
		return &rollover.Action{
			IndicesClient: &esclient.IndicesClient{Client: c, MasterTimeoutSeconds: cfg.Timeout},
			Config:        *rolloverCfg,
		}
	})
	require.NoError(t, err)
	assert.True(t, peer.saw(http.MethodPost, "/_rollover"), "explicit skip-host-verify still performs rollover")
}
