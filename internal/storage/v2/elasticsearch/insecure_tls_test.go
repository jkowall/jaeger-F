// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package elasticsearch

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/config/configtls"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/internal/metrics"
	escfg "github.com/jaegertracing/jaeger/internal/storage/elasticsearch/config"
)

// placeholderUser and placeholderPassword are obvious stand-ins. They are not
// credentials for any real system.
const (
	placeholderUser     = "placeholder-user"
	placeholderPassword = "placeholder-password"
	esVersionBody       = `{"version":{"number":"8.0.0"},"tagline":"You Know, for Search"}`
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
		p.mu.Lock()
		p.hits = append(p.hits, peerHit{
			method:        r.Method,
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization") != "",
		})
		p.mu.Unlock()
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte(esVersionBody))
			return
		}
		_, _ = w.Write([]byte(`{"acknowledged":true}`))
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

func (p *untrustedPeer) requireUntouched(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.hits) == 0 {
		return
	}
	h := p.hits[0]
	t.Errorf("untrusted peer received %d request(s); first was %s %s (authorization header present: %t)",
		len(p.hits), h.method, h.path, h.authorization)
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

func storageConfig(serverURL string, tlsCfg configtls.ClientConfig) escfg.Configuration {
	replicas := int64(1)
	return escfg.Configuration{
		Servers:              []string{serverURL},
		CreateIndexTemplates: true,
		TLS:                  tlsCfg,
		Authentication: escfg.Authentication{
			BasicAuthentication: configoptional.Some(escfg.BasicAuthentication{
				Username: placeholderUser,
				Password: placeholderPassword,
			}),
		},
		Indices: escfg.Indices{
			Spans:    escfg.SpanIndexOptions{IndexOptions: escfg.IndexOptions{Replicas: &replicas}},
			Services: escfg.IndexOptions{Replicas: &replicas},
		},
	}
}

// TestFactoryWithTLSDisabledDoesNotSendCredentialsOnHTTPS covers the jaeger
// storage factory. configtls.ClientConfig.Insecure is the value es-index-cleaner
// and es-rollover store when --es.tls.enabled is left false, and it is also the
// collector setting tls.insecure: true. An https URL must not be dialed with
// certificate verification disabled.
func TestFactoryWithTLSDisabledDoesNotSendCredentialsOnHTTPS(t *testing.T) {
	peer := newUntrustedPeer(t)
	cfg := storageConfig(peer.server.URL, configtls.ClientConfig{Insecure: true})
	f, err := NewFactoryBase(context.Background(), cfg, metrics.NullFactory, zap.NewNop(), nooptrace.NewTracerProvider(), nil)
	if f != nil {
		t.Cleanup(func() { _ = f.Close() })
	}
	if err == nil {
		err = f.Purge(context.Background())
	}
	assert.Error(t, err)
	peer.requireUntouched(t)
}

// TestFactoryWithTLSEnabledRejectsUntrustedHTTPSCertificate locks the collector
// default (Insecure false): https uses the system trust store.
func TestFactoryWithTLSEnabledRejectsUntrustedHTTPSCertificate(t *testing.T) {
	peer := newUntrustedPeer(t)
	cfg := storageConfig(peer.server.URL, configtls.ClientConfig{})
	f, err := NewFactoryBase(context.Background(), cfg, metrics.NullFactory, zap.NewNop(), nooptrace.NewTracerProvider(), nil)
	if f != nil {
		t.Cleanup(func() { _ = f.Close() })
	}
	assert.Error(t, err)
	peer.requireUntouched(t)
}
