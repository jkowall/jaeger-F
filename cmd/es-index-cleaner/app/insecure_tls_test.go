// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/internal/config"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/esclient"
)

// placeholderUser and placeholderPassword are obvious stand-ins. They are not
// credentials for any real system.
const (
	placeholderUser     = "placeholder-user"
	placeholderPassword = "placeholder-password"
)

// esVersionBody is what NewClient's startup probe accepts as Elasticsearch 8.
const esVersionBody = `{"version":{"number":"8.0.0"},"tagline":"You Know, for Search"}`

// esCleanerIndicesBody is one daily index old enough for the cleaner's date filter.
const esCleanerIndicesBody = `{"jaeger-span-2000-01-01":{"aliases":{},"settings":{"index.creation_date":"1"}}}`

// peerHit is one HTTP request that completed a TLS handshake and reached the handler.
// The Authorization header value is not stored.
type peerHit struct {
	method        string
	path          string
	authorization bool
}

// untrustedPeer is an HTTPS server whose certificate is self-signed for a name
// other than the address the client dials.
type untrustedPeer struct {
	server *httptest.Server
	mu     sync.Mutex
	hits   []peerHit
}

func newUntrustedPeer(t *testing.T, indicesBody string) *untrustedPeer {
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
			body = []byte(indicesBody)
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

// requireUntouched fails if the handshake produced any HTTP request. A refusal
// that still delivers the request, including the startup version probe, is a failure.
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

func loadCleanerConfig(t *testing.T, args []string) *Config {
	t.Helper()
	cfg := &Config{}
	v, cmd := config.Viperize(cfg.AddFlags)
	require.NoError(t, cmd.ParseFlags(args))
	require.NoError(t, cfg.InitFromViper(v))
	return cfg
}

func placeholderAuthArgs(extra ...string) []string {
	args := []string{
		"--es.username=" + placeholderUser,
		"--es.password=" + placeholderPassword,
		"--timeout=5",
	}
	return append(args, extra...)
}

// runCleaner performs the same calls as cmd/es-index-cleaner/main.go after flag
// parsing: build the client, list Jaeger indices, and delete those the filter selects.
func runCleaner(t *testing.T, endpoint string, cfg *Config) error {
	t.Helper()
	ctx := context.Background()
	client, err := NewESClient(ctx, endpoint, cfg, zap.NewNop())
	if client != nil {
		t.Cleanup(func() { _ = client.Close() })
	}
	if err != nil {
		return err
	}
	indicesClient := esclient.IndicesClient{
		Client:                 client,
		MasterTimeoutSeconds:   cfg.MasterNodeTimeoutSeconds,
		IgnoreUnavailableIndex: true,
	}
	indices, err := indicesClient.GetJaegerIndices(ctx, cfg.IndexPrefix)
	if err != nil {
		return err
	}
	deleteBefore := CalculateDeletionCutoff(time.Now().UTC(), 7, false)
	filtered := (&IndexFilter{
		IndexPrefix:          cfg.IndexPrefix,
		IndexDateSeparator:   cfg.IndexDateSeparator,
		DeleteBeforeThisDate: deleteBefore,
	}).Filter(indices)
	if len(filtered) == 0 {
		return nil
	}
	return indicesClient.DeleteIndices(ctx, filtered)
}

// TestDefaultTLSDisabledDoesNotSendCredentialsOrDeleteOnHTTPS is the vulnerable
// configuration: --es.tls.enabled is left at its default (false) and the server
// URL is https. The peer certificate is not in the trust store and names a
// different host. The cleaner must fail before that peer sees a request.
func TestDefaultTLSDisabledDoesNotSendCredentialsOrDeleteOnHTTPS(t *testing.T) {
	peer := newUntrustedPeer(t, esCleanerIndicesBody)
	cfg := loadCleanerConfig(t, placeholderAuthArgs())
	err := runCleaner(t, peer.server.URL, cfg)
	assert.Error(t, err)
	peer.requireUntouched(t)
}

// TestHTTPWithTLSDisabledStillDeletesIndices locks the plaintext behavior of
// TLS-disabled: an http URL is not a certificate check, and deletion still runs.
func TestHTTPWithTLSDisabledStillDeletesIndices(t *testing.T) {
	var peer untrustedPeer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer.record(r)
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		body := []byte(`{"acknowledged":true}`)
		switch {
		case r.URL.Path == "/":
			body = []byte(esVersionBody)
		case r.Method == http.MethodGet && r.URL.Path == "/jaeger-*":
			body = []byte(esCleanerIndicesBody)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	cfg := loadCleanerConfig(t, placeholderAuthArgs())
	require.NoError(t, runCleaner(t, server.URL, cfg))
	assert.True(t, peer.saw(http.MethodDelete, "jaeger-span-2000-01-01"), "plaintext cleaner still deletes the selected index")
}

// TestTLSEnabledRejectsUntrustedHTTPSCertificate locks certificate verification
// when --es.tls.enabled is set and --es.tls.skip-host-verify is not.
func TestTLSEnabledRejectsUntrustedHTTPSCertificate(t *testing.T) {
	peer := newUntrustedPeer(t, esCleanerIndicesBody)
	cfg := loadCleanerConfig(t, placeholderAuthArgs("--es.tls.enabled=true"))
	err := runCleaner(t, peer.server.URL, cfg)
	assert.Error(t, err)
	peer.requireUntouched(t)
}

// TestSkipHostVerifyStillAllowsUntrustedHTTPSCertificate locks the explicit
// opt-in. Skip-host-verify is honored only together with TLS enabled.
func TestSkipHostVerifyStillAllowsUntrustedHTTPSCertificate(t *testing.T) {
	peer := newUntrustedPeer(t, esCleanerIndicesBody)
	cfg := loadCleanerConfig(t, placeholderAuthArgs(
		"--es.tls.enabled=true",
		"--es.tls.skip-host-verify=true",
	))
	require.NoError(t, runCleaner(t, peer.server.URL, cfg))
	assert.True(t, peer.saw(http.MethodDelete, "jaeger-span-2000-01-01"))
	for _, hit := range peer.snapshot() {
		if hit.method == http.MethodDelete {
			assert.True(t, hit.authorization, "explicit skip-host-verify still sends the configured Authorization header")
		}
	}
}
