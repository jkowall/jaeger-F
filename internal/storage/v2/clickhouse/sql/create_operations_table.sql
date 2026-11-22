CREATE TABLE IF NOT EXISTS
    operations{{ .LocalTableSuffix }} (
        service_name String,
        name String,
        span_kind String,
        last_seen SimpleAggregateFunction(max, DateTime64(9))
    ) ENGINE = {{ .AggregatingMergeTree }}
ORDER BY
    (service_name, span_kind)
{{- if gt .TTLSeconds 0 }}
TTL last_seen + INTERVAL {{ .TTLSeconds }} SECOND DELETE
{{- end }}
