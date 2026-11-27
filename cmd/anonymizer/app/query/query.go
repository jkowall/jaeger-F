// Copyright (c) 2020 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package query

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"go.opentelemetry.io/collector/config/configtls"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger-idl/proto-gen/api_v2"
	_ "github.com/jaegertracing/jaeger/internal/gogocodec" // force gogo codec registration
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/spanstore"
)

// ErrPlaintextNonLoopback is returned when a plaintext connection would reach a non-loopback peer.
var ErrPlaintextNonLoopback = errors.New("refusing plaintext gRPC connection to a non-loopback jaeger-query address; enable TLS to reach a remote query service")

// Query represents a jaeger-query's query for trace-id
type Query struct {
	client api_v2.QueryServiceClient
	conn   *grpc.ClientConn
}

// New creates a Query object that talks plaintext gRPC to a jaeger-query
// service listening on a loopback address.
func New(addr string) (*Query, error) {
	return NewWithTLS(addr, configtls.ClientConfig{Insecure: true})
}

// NewWithTLS creates a Query object using the given TLS client configuration.
// When tlsCfg.Insecure is set the connection is plaintext and is only
// permitted to a loopback peer.
func NewWithTLS(addr string, tlsCfg configtls.ClientConfig) (*Query, error) {
	var dialOpts []grpc.DialOption
	if tlsCfg.Insecure {
		dialOpts = append(dialOpts,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(dialLoopbackOnly),
			grpc.WithNoProxy(),
		)
	} else {
		cfg, err := tlsCfg.LoadTLSConfig(context.Background())
		if err != nil {
			return nil, fmt.Errorf("failed to load TLS config for the jaeger-query service: %w", err)
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	}

	conn, err := grpc.NewClient(addr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect with the jaeger-query service: %w", err)
	}

	return &Query{
		client: api_v2.NewQueryServiceClient(conn),
		conn:   conn,
	}, nil
}

// dialLoopbackOnly checks the peer of the established TCP connection, not the
// dial target, so a hostname cannot resolve to loopback for a check and to a
// remote address for the dial. gRPC sends nothing before this returns.
func dialLoopbackOnly(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if err := requireLoopbackPeer(conn); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	return conn, nil
}

func requireLoopbackPeer(conn net.Conn) error {
	if tcpAddr, ok := conn.RemoteAddr().(*net.TCPAddr); !ok || !tcpAddr.IP.IsLoopback() {
		return fmt.Errorf("%w: %s", ErrPlaintextNonLoopback, conn.RemoteAddr())
	}
	return nil
}

// unwrapNotFoundErr is a conversion function
func unwrapNotFoundErr(err error) error {
	if s, _ := status.FromError(err); s != nil {
		if strings.Contains(s.Message(), spanstore.ErrTraceNotFound.Error()) {
			return spanstore.ErrTraceNotFound
		}
	}
	return err
}

// QueryTrace queries for a trace and returns all spans inside it
func (q *Query) QueryTrace(traceID string, startTime time.Time, endTime time.Time) ([]model.Span, error) {
	mTraceID, err := model.TraceIDFromString(traceID)
	if err != nil {
		return nil, fmt.Errorf("failed to convert the provided trace id: %w", err)
	}

	request := api_v2.GetTraceRequest{
		TraceID:   mTraceID,
		StartTime: startTime,
		EndTime:   endTime,
	}

	stream, err := q.client.GetTrace(context.Background(), &request)
	if err != nil {
		return nil, unwrapNotFoundErr(err)
	}

	var spans []model.Span
	for received, err := stream.Recv(); !errors.Is(err, io.EOF); received, err = stream.Recv() {
		if err != nil {
			return nil, unwrapNotFoundErr(err)
		}
		spans = append(spans, received.Spans...)
	}

	return spans, nil
}

// Close closes the grpc client connection
func (q *Query) Close() error {
	return q.conn.Close()
}
