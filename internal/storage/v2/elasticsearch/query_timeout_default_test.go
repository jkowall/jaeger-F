// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package elasticsearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	es "github.com/jaegertracing/jaeger/internal/storage/elasticsearch"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/esclient"
)

// maxDefaultQueryTimeout is the longest a single Elasticsearch/OpenSearch
// search may run when the operator has not set query_timeout. A default of
// zero leaves every page of a trace read without a deadline.
const maxDefaultQueryTimeout = 30 * time.Second

func TestDefaultQueryTimeoutBoundsSearch(t *testing.T) {
	cfg := DefaultConfig()
	require.Greater(t, cfg.QueryTimeout, time.Duration(0),
		"the default Elasticsearch query timeout is zero, so a search runs with no deadline")
	require.LessOrEqual(t, cfg.QueryTimeout, maxDefaultQueryTimeout,
		"the default query timeout must be a real deadline, not a multi-minute hang")
	require.Equal(t, 10_000, cfg.MaxDocCount,
		"max_doc_count is the maximum number of results per query and the cap a single trace read may load")

	// The handler returns only when the client cancels. Client.request applies
	// QueryTimeout as the request deadline; a zero timeout never cancels.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		http.Error(w, "cancelled", http.StatusGatewayTimeout)
	}))
	t.Cleanup(srv.Close)

	cfg.Servers = []string{srv.URL}
	cfg.Version = uint(es.ElasticV7)
	cfg.HTTPCompression = false
	client, err := esclient.NewClient(context.Background(), &cfg, zap.NewNop(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	start := time.Now()
	_, err = (esclient.SearchClient{Client: client}).Search(context.Background(), []string{"jaeger-span"}, esclient.SearchRequest{Size: 1})
	elapsed := time.Since(start)
	require.Error(t, err, "a search past the default query timeout must fail")
	require.Less(t, elapsed, cfg.QueryTimeout+2*time.Second,
		"search returned after %s; the default query timeout is %s", elapsed, cfg.QueryTimeout)
}
