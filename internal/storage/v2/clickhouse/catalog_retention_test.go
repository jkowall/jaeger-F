// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/open-telemetry/opentelemetry-collector-contrib/extension/basicauthextension"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/jaegerreceiver"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/zipkinreceiver"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/configopaque"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/otlpreceiver"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger-idl/proto-gen/api_v2"
	"github.com/jaegertracing/jaeger/internal/jptrace/sanitizer"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
	"github.com/jaegertracing/jaeger/internal/telemetry"
	"github.com/jaegertracing/jaeger/internal/telemetry/otelsemconv"
)

// TestNameCatalogsExpireWithSpans submits spans the way a collector does and
// then forces ClickHouse to apply table retention. Service names, operation
// names, and attribute keys copied from a span must not outlive that span.
// A name still referenced by a span inside the retention window must stay.
//
// The collector listens without authentication. Each subtest posts OTLP JSON,
// Zipkin JSON, and a Jaeger PostSpans request at the receivers linked into
// the jaeger binary, then writes the batch through the same sanitizer the
// storage exporter uses. Both attribute layouts are covered. The materialized
// views that fill services, operations, and attribute_metadata (span,
// resource, scope, event, and link keys) all run inside ClickHouse.
func TestNameCatalogsExpireWithSpans(t *testing.T) {
	conn := openClickHouse(t, "")
	require.NoError(t, conn.Close())

	for _, layout := range []string{"nested", "map"} {
		t.Run(layout, func(t *testing.T) {
			assertCatalogsExpireWithSpans(t, layout)
		})
	}
}

func assertCatalogsExpireWithSpans(t *testing.T, layout string) {
	t.Helper()
	db := "catalog_ret_" + layout
	admin := openClickHouse(t, "")
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	ctx := context.Background()
	require.NoError(t, admin.Exec(ctx, "DROP DATABASE IF EXISTS "+db+" SYNC"))
	require.NoError(t, admin.Exec(ctx, "CREATE DATABASE "+db))
	t.Cleanup(func() {
		_ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" SYNC")
	})

	cfg := DefaultConfiguration()
	cfg.Addresses = []string{clickHouseAddr()}
	cfg.Database = db
	cfg.Auth = clickHouseAuth()
	cfg.CreateSchema = true
	// Two seconds, and every span starts two seconds in the future, so the row
	// is inside the window at insert even if the write is briefly delayed.
	// start_time is part of the spans sort key, so it cannot be updated later.
	cfg.TTL = 2 * time.Second
	cfg.AttributeLayout = layout
	cfg.TableEngine = TableEngine{MergeTree: configoptional.Some(MergeTreeEngine{})}
	factory, err := NewFactory(ctx, cfg, telemetry.NoopSettings())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, factory.Close()) })
	writer, err := factory.CreateTraceWriter()
	require.NoError(t, err)

	query := openClickHouse(t, db)
	t.Cleanup(func() { require.NoError(t, query.Close()) })

	prefix := "exp-" + layout
	liveService := "live-" + layout + "-svc"
	liveOp := "live-" + layout + "-op"
	liveAttr := "live-" + layout + "-attr"
	wave1 := prefix + "-svc"
	wave1OpA := prefix + "-op-a"
	wave1OpB := prefix + "-op-b"
	wave2 := prefix + "-svc-2"
	wave2Op := prefix + "-op-2"
	wave2Attr := prefix + "-attr-2"
	zipkinService := prefix + "-zipkin-svc"
	zipkinOp := prefix + "-zipkin-op"
	zipkinAttr := prefix + "-zipkin-attr"
	jaegerService := prefix + "-jaeger-svc"
	jaegerOp := prefix + "-jaeger-op"
	jaegerAttr := prefix + "-jaeger-attr"

	httpClient := &http.Client{}
	t.Cleanup(httpClient.CloseIdleConnections)
	otlpURL := startOTLPReceiver(t, writer)
	zipkinURL := startZipkinReceiver(t, writer)
	jaegerAddr := startJaegerReceiver(t, writer)

	// Spans are inside the retention window when they are accepted. ClickHouse
	// drops a part whose rows are already expired at insert, so a backdated
	// span would not show that names copied from stored spans remain.
	// Separate requests: one insert block collapses to a single operations row
	// because that table's sort key is (service_name, span_kind), not the
	// operation name. Each request still has to be retained on its own.
	postOTLP(t, httpClient, otlpURL, otlpBatch(spanStart(), wave1, []string{wave1OpA}, prefix, true))
	postOTLP(t, httpClient, otlpURL, otlpBatch(spanStart(), wave1, []string{wave1OpB}, "", false))
	postZipkin(t, httpClient, zipkinURL, zipkinService, zipkinOp, zipkinAttr, spanStart())
	postJaeger(t, jaegerAddr, jaegerService, jaegerOp, jaegerAttr, spanStart())

	requireStored(t, query, wave1, wave1OpA)
	requireStored(t, query, wave1, wave1OpB)
	requireStored(t, query, zipkinService, zipkinOp)
	requireStored(t, query, jaegerService, jaegerOp)
	for _, key := range attributeKeys(prefix) {
		require.NotZero(t, waitCount(t, query, "SELECT count() FROM attribute_metadata WHERE attribute_key = ?", key),
			"attribute key %s was not copied into attribute_metadata", key)
	}
	require.NotZero(t, waitCount(t, query, "SELECT count() FROM attribute_metadata WHERE attribute_key = ?", zipkinAttr))
	require.NotZero(t, waitCount(t, query, "SELECT count() FROM attribute_metadata WHERE attribute_key = ?", jaegerAttr))

	expiredTraceIDs := traceIDs(t, query, wave1)
	expiredTraceIDs = append(expiredTraceIDs, traceIDs(t, query, zipkinService)...)
	expiredTraceIDs = append(expiredTraceIDs, traceIDs(t, query, jaegerService)...)
	require.NotEmpty(t, expiredTraceIDs)

	expireSpans(t, query)
	require.Zero(t, countWhere(t, query, "SELECT count() FROM spans WHERE service_name = ?", wave1),
		"span TTL did not delete the expired spans; the catalog check would not be meaningful")
	require.Zero(t, countWhere(t, query, "SELECT count() FROM spans WHERE service_name = ?", zipkinService))
	require.Zero(t, countWhere(t, query, "SELECT count() FROM spans WHERE service_name = ?", jaegerService))
	for _, id := range expiredTraceIDs {
		require.Zero(t, countWhere(t, query, "SELECT count() FROM trace_id_timestamps WHERE trace_id = ?", id),
			"trace_id_timestamps TTL did not delete %s", id)
	}

	// A second wave arrives after the first spans have already been deleted.
	postOTLP(t, httpClient, otlpURL, otlpBatch(spanStart(), wave2, []string{wave2Op}, wave2Attr, false))
	requireStored(t, query, wave2, wave2Op)
	require.NotZero(t, waitCount(t, query, "SELECT count() FROM attribute_metadata WHERE attribute_key = ?", wave2Attr))
	wave2TraceIDs := traceIDs(t, query, wave2)
	expireSpans(t, query)
	require.Zero(t, countWhere(t, query, "SELECT count() FROM spans WHERE service_name = ?", wave2))
	for _, id := range wave2TraceIDs {
		require.Zero(t, countWhere(t, query, "SELECT count() FROM trace_id_timestamps WHERE trace_id = ?", id))
	}

	postOTLP(t, httpClient, otlpURL, otlpBatch(spanStart(), liveService, []string{liveOp}, liveAttr, false))
	requireStored(t, query, liveService, liveOp)
	require.NotZero(t, waitCount(t, query, "SELECT count() FROM attribute_metadata WHERE attribute_key = ?", liveAttr))
	liveTraceIDs := traceIDs(t, query, liveService)
	require.NotEmpty(t, liveTraceIDs)
	optimize(t, query, "spans", "services", "operations", "attribute_metadata", "trace_id_timestamps")
	require.NotZero(t, countWhere(t, query, "SELECT count() FROM spans WHERE service_name = ?", liveService),
		"span TTL deleted a span that is still inside the retention window")

	var survived []string
	for _, name := range []string{wave1, wave2, zipkinService, jaegerService} {
		if n := countWhere(t, query, "SELECT count() FROM services WHERE name = ?", name); n != 0 {
			survived = append(survived, fmt.Sprintf("services.name=%s (%d rows)", name, n))
		}
	}
	for _, spec := range []struct{ service, op string }{
		{wave1, wave1OpA},
		{wave1, wave1OpB},
		{wave2, wave2Op},
		{zipkinService, zipkinOp},
		{jaegerService, jaegerOp},
	} {
		n := countWhere(t, query,
			"SELECT count() FROM operations WHERE service_name = ? AND name = ?", spec.service, spec.op)
		if n != 0 {
			survived = append(survived, fmt.Sprintf("operations %s %s (%d rows)", spec.service, spec.op, n))
		}
	}
	goneKeys := append(attributeKeys(prefix), zipkinAttr, jaegerAttr, wave2Attr)
	for _, key := range goneKeys {
		if n := countWhere(t, query, "SELECT count() FROM attribute_metadata WHERE attribute_key = ?", key); n != 0 {
			survived = append(survived, fmt.Sprintf("attribute_metadata %s (%d rows)", key, n))
		}
	}
	if len(survived) > 0 {
		t.Fatalf("catalog rows outlived the spans that introduced them (%d bytes still in services, operations, and attribute_metadata): %s",
			catalogBytes(t, query), strings.Join(survived, "; "))
	}

	require.NotZero(t, countWhere(t, query, "SELECT count() FROM services WHERE name = ?", liveService),
		"retention removed a service name that a live span still references")
	require.NotZero(t, countWhere(t, query,
		"SELECT count() FROM operations WHERE service_name = ? AND name = ?", liveService, liveOp))
	require.NotZero(t, countWhere(t, query, "SELECT count() FROM attribute_metadata WHERE attribute_key = ?", liveAttr))
	for _, id := range liveTraceIDs {
		require.NotZero(t, countWhere(t, query, "SELECT count() FROM trace_id_timestamps WHERE trace_id = ?", id))
	}
}

var traceSeq uint64

func attributeKeys(prefix string) []string {
	var keys []string
	for _, level := range []string{"resource", "scope", "span", "event", "link"} {
		for _, kind := range []string{"bool", "double", "int", "str", "bytes", "map", "slice"} {
			keys = append(keys, prefix+"."+level+"."+kind)
		}
	}
	return keys
}

func otlpBatch(start time.Time, service string, operations []string, attrPrefix string, full bool) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr(string(otelsemconv.ServiceNameKey), service)
	if full {
		putAttributeTypes(rs.Resource().Attributes(), attrPrefix+".resource")
	}
	ss := rs.ScopeSpans().AppendEmpty()
	ss.Scope().SetName("catalog-retention")
	if full {
		putAttributeTypes(ss.Scope().Attributes(), attrPrefix+".scope")
	}
	for _, op := range operations {
		span := ss.Spans().AppendEmpty()
		span.SetName(op)
		span.SetKind(ptrace.SpanKindServer)
		var traceID pcommon.TraceID
		var spanID pcommon.SpanID
		nonce := atomic.AddUint64(&traceSeq, 1)
		binary.LittleEndian.PutUint64(traceID[:8], nonce)
		binary.LittleEndian.PutUint64(traceID[8:], nonce^0x9e3779b97f4a7c15)
		binary.LittleEndian.PutUint64(spanID[:], nonce^0x6a09e667f3bcc909)
		span.SetTraceID(traceID)
		span.SetSpanID(spanID)
		span.SetStartTimestamp(pcommon.NewTimestampFromTime(start))
		span.SetEndTimestamp(pcommon.NewTimestampFromTime(start.Add(time.Millisecond)))
		if full {
			putAttributeTypes(span.Attributes(), attrPrefix+".span")
			event := span.Events().AppendEmpty()
			event.SetName("catalog-event")
			event.SetTimestamp(span.StartTimestamp())
			putAttributeTypes(event.Attributes(), attrPrefix+".event")
			link := span.Links().AppendEmpty()
			link.SetTraceID(traceID)
			link.SetSpanID(spanID)
			putAttributeTypes(link.Attributes(), attrPrefix+".link")
		} else if attrPrefix != "" {
			span.Attributes().PutStr(attrPrefix, "v")
		}
	}
	return td
}

func putAttributeTypes(attrs pcommon.Map, prefix string) {
	attrs.PutBool(prefix+".bool", true)
	attrs.PutDouble(prefix+".double", 1.5)
	attrs.PutInt(prefix+".int", 7)
	attrs.PutStr(prefix+".str", "v")
	attrs.PutEmptyBytes(prefix + ".bytes").FromRaw([]byte{1, 2, 3})
	attrs.PutEmptyMap(prefix+".map").PutStr("k", "v")
	attrs.PutEmptySlice(prefix + ".slice").AppendEmpty().SetStr("item")
}

func postOTLP(t *testing.T, client *http.Client, url string, td ptrace.Traces) {
	t.Helper()
	body, err := ptraceotlp.NewExportRequestFromTraces(td).MarshalJSON()
	require.NoError(t, err)
	parsed := ptraceotlp.NewExportRequest()
	require.NoError(t, parsed.UnmarshalJSON(body))
	require.NotZero(t, parsed.Traces().SpanCount(), "otlp json round trip dropped spans: %s", body)
	req, err := http.NewRequest(http.MethodPost, url+"/v1/traces", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "otlp response: %s", payload)
}

func postZipkin(t *testing.T, client *http.Client, url, service, operation, attr string, start time.Time) {
	t.Helper()
	body := fmt.Sprintf(`[{
		"traceId": "%032x",
		"id": "%016x",
		"name": %q,
		"timestamp": %d,
		"duration": 1000,
		"localEndpoint": {"serviceName": %q},
		"tags": {%q: "v"}
	}]`, start.UnixNano(), start.UnixNano()&0xffff, operation, start.UnixMicro(), service, attr)
	req, err := http.NewRequest(http.MethodPost, url+"/api/v2/spans", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusAccepted, resp.StatusCode, "zipkin response: %s", payload)
}

func postJaeger(t *testing.T, addr, service, operation, attr string, start time.Time) {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()
	client := api_v2.NewCollectorServiceClient(conn)
	_, err = client.PostSpans(context.Background(), &api_v2.PostSpansRequest{
		Batch: model.Batch{
			Process: &model.Process{ServiceName: service},
			Spans: []*model.Span{{
				TraceID:       model.NewTraceID(uint64(start.UnixNano()), uint64(len(service))),
				SpanID:        model.NewSpanID(uint64(len(operation) + 1)),
				OperationName: operation,
				StartTime:     start,
				Duration:      time.Millisecond,
				Tags:          []model.KeyValue{model.String(attr, "v")},
			}},
		},
	})
	require.NoError(t, err)
}

type traceWriterConsumer struct {
	w tracestore.Writer
}

func (traceWriterConsumer) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: true}
}

func (c traceWriterConsumer) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	return c.w.WriteTraces(ctx, sanitizer.Sanitize(td))
}

func startOTLPReceiver(t *testing.T, w tracestore.Writer) string {
	t.Helper()
	factory := otlpreceiver.NewFactory()
	cfg := factory.CreateDefaultConfig().(*otlpreceiver.Config)
	cfg.Protocols.GRPC = configoptional.None[configgrpc.ServerConfig]()
	endpoint := freeEndpoint(t)
	cfg.Protocols.HTTP.GetOrInsertDefault().ServerConfig.NetAddr.Endpoint = endpoint
	return "http://" + startReceiver(t, factory, cfg, endpoint, w)
}

func startZipkinReceiver(t *testing.T, w tracestore.Writer) string {
	t.Helper()
	factory := zipkinreceiver.NewFactory()
	cfg := factory.CreateDefaultConfig().(*zipkinreceiver.Config)
	endpoint := freeEndpoint(t)
	cfg.ServerConfig.NetAddr.Endpoint = endpoint
	return "http://" + startReceiver(t, factory, cfg, endpoint, w)
}

func startJaegerReceiver(t *testing.T, w tracestore.Writer) string {
	t.Helper()
	factory := jaegerreceiver.NewFactory()
	cfg := factory.CreateDefaultConfig().(*jaegerreceiver.Config)
	cfg.Protocols.ThriftHTTP = configoptional.None[confighttp.ServerConfig]()
	cfg.Protocols.ThriftBinaryUDP = configoptional.None[jaegerreceiver.ProtocolUDP]()
	cfg.Protocols.ThriftCompactUDP = configoptional.None[jaegerreceiver.ProtocolUDP]()
	endpoint := freeEndpoint(t)
	cfg.Protocols.GRPC.GetOrInsertDefault().NetAddr.Endpoint = endpoint
	startReceiver(t, factory, cfg, endpoint, w)
	return endpoint
}

func startReceiver(t *testing.T, factory receiver.Factory, cfg component.Config, endpoint string, w tracestore.Writer) string {
	t.Helper()
	set := receiver.Settings{
		ID: component.NewID(factory.Type()),
		TelemetrySettings: component.TelemetrySettings{
			Logger:         zap.NewNop(),
			TracerProvider: nooptrace.NewTracerProvider(),
			MeterProvider:  noopmetric.NewMeterProvider(),
		},
		BuildInfo: component.NewDefaultBuildInfo(),
	}
	rec, err := factory.CreateTraces(context.Background(), set, cfg, traceWriterConsumer{w: w})
	require.NoError(t, err)
	require.NoError(t, rec.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() {
		require.NoError(t, rec.Shutdown(context.Background()))
	})
	return endpoint
}

func freeEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	endpoint := ln.Addr().String()
	require.NoError(t, ln.Close())
	return endpoint
}

func requireStored(t *testing.T, conn driver.Conn, service, operation string) {
	t.Helper()
	if n := waitCount(t, conn, "SELECT count() FROM spans WHERE service_name = ?", service); n == 0 {
		var db string
		var spans, services uint64
		_ = conn.QueryRow(context.Background(), "SELECT currentDatabase()").Scan(&db)
		_ = conn.QueryRow(context.Background(), "SELECT count() FROM spans").Scan(&spans)
		_ = conn.QueryRow(context.Background(), "SELECT count() FROM services").Scan(&services)
		t.Fatalf("span for %s was not stored (db=%s spans=%d services=%d)", service, db, spans, services)
	}
	require.NotZero(t, waitCount(t, conn, "SELECT count() FROM services WHERE name = ?", service),
		"service %s was not copied into services", service)
	require.NotZero(t, waitCount(t, conn,
		"SELECT count() FROM operations WHERE service_name = ? AND name = ?", service, operation),
		"operation %s %s was not copied into operations", service, operation)
}

// waitCount polls until the query returns a positive count or two seconds pass.
// ClickHouse makes an acknowledged insert visible immediately, but a receiver
// goroutine can still be handing the batch to the writer when the test looks.
func waitCount(t *testing.T, conn driver.Conn, query string, args ...any) uint64 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var n uint64
	for {
		n = countWhere(t, conn, query, args...)
		if n > 0 || !time.Now().Before(deadline) {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func traceIDs(t *testing.T, conn driver.Conn, service string) []string {
	t.Helper()
	rows, err := conn.Query(context.Background(), "SELECT trace_id FROM spans WHERE service_name = ?", service)
	require.NoError(t, err)
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

// spanStart is far enough ahead of time.Now that the one-part TTL check at
// insert still keeps the row. expireSpans waits until that lead, plus the
// configured retention, has passed.
func spanStart() time.Time {
	return time.Now().Add(2 * time.Second).UTC()
}

func expireSpans(t *testing.T, conn driver.Conn) {
	t.Helper()
	time.Sleep(5 * time.Second)
	optimize(t, conn, "spans", "trace_id_timestamps")
}

func optimize(t *testing.T, conn driver.Conn, tables ...string) {
	t.Helper()
	for _, table := range tables {
		require.NoError(t, conn.Exec(context.Background(), "OPTIMIZE TABLE "+table+" FINAL"))
	}
}

func countWhere(t *testing.T, conn driver.Conn, query string, args ...any) uint64 {
	t.Helper()
	var n uint64
	require.NoError(t, conn.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func catalogBytes(t *testing.T, conn driver.Conn) uint64 {
	t.Helper()
	return countWhere(t, conn, `
		SELECT sum(bytes_on_disk)
		FROM system.parts
		WHERE active AND database = currentDatabase()
			AND table IN ('services', 'operations', 'attribute_metadata')`)
}

func openClickHouse(t *testing.T, database string) driver.Conn {
	t.Helper()
	configured := clickHouseAuth()
	auth := configured.Basic.Get()
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{clickHouseAddr()},
		Auth: clickhouse.Auth{
			Database: database,
			Username: auth.Username,
			Password: string(auth.Password),
		},
		DialTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = conn.Ping(ctx)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("ClickHouse at %s is not accepting connections (%v). Start the server pinned in docker-compose/clickhouse/docker-compose-lts-march.yml and rerun this test.", clickHouseAddr(), err)
	}
	return conn
}

func clickHouseAddr() string {
	if addr := os.Getenv("CLICKHOUSE_ADDR"); addr != "" {
		return addr
	}
	return "127.0.0.1:9000"
}

func clickHouseAuth() Authentication {
	return Authentication{
		Basic: configoptional.Some(basicauthextension.ClientAuthSettings{
			Username: "default",
			Password: configopaque.String(clickHousePassword()),
		}),
	}
}

func clickHousePassword() string {
	if password := os.Getenv("CLICKHOUSE_PASSWORD"); password != "" {
		return password
	}
	// Fixture shared with docker-compose/clickhouse and the storage integration tests.
	return "password"
}
