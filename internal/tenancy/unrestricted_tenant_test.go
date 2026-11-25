// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package tenancy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestEnabledWithoutAllowListRejectsTenantBeforeHandler fails while an enabled
// tenancy manager with an empty allow-list treats every non-empty header as
// valid. The handler must not run: a tenant store created before the refusal
// is the exhaustion this guards against.
func TestEnabledWithoutAllowListRejectsTenantBeforeHandler(t *testing.T) {
	for _, tenants := range [][]string{nil, {}} {
		mgr := NewManager(&Options{Enabled: true, Header: "x-scope-orgid", Tenants: tenants})
		require.False(t, mgr.Valid("attacker"), "empty allow-list accepted a tenant")
		require.False(t, mgr.Valid(" "), "blank tenant header was accepted")

		t.Run("http", func(t *testing.T) {
			reached := false
			handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				reached = true
			})
			req := httptest.NewRequest(http.MethodGet, "/api/v3/services", http.NoBody)
			req.Header.Set(mgr.Header, "attacker")
			rec := httptest.NewRecorder()
			ExtractTenantHTTPHandler(mgr, handler).ServeHTTP(rec, req)
			require.Equal(t, http.StatusUnauthorized, rec.Code)
			require.False(t, reached, "HTTP handler ran for a tenant that was not allow-listed")
		})

		ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{mgr.Header: []string{"attacker"}})
		clientCtx := client.NewContext(context.Background(), client.Info{
			Metadata: client.NewMetadata(map[string][]string{mgr.Header: {"attacker"}}),
		})
		for _, test := range []struct {
			name string
			ctx  context.Context
		}{
			{name: "grpc metadata", ctx: ctx},
			{name: "client metadata", ctx: clientCtx},
		} {
			t.Run(test.name, func(t *testing.T) {
				unaryReached := false
				_, err := NewGuardingUnaryInterceptor(mgr)(test.ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) {
					unaryReached = true
					return nil, nil
				})
				require.Equal(t, codes.PermissionDenied, status.Code(err))
				require.False(t, unaryReached, "unary handler ran for a tenant that was not allow-listed")

				streamReached := false
				err = NewGuardingStreamInterceptor(mgr)(nil, &tenantedServerStream{context: test.ctx}, &grpc.StreamServerInfo{}, func(any, grpc.ServerStream) error {
					streamReached = true
					return nil
				})
				require.Equal(t, codes.PermissionDenied, status.Code(err))
				require.False(t, streamReached, "stream handler ran for a tenant that was not allow-listed")
			})
		}
	}
}
