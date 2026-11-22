CREATE TABLE
    IF NOT EXISTS services{{ .LocalTableSuffix }} (
        name String,
        last_seen SimpleAggregateFunction(max, DateTime64(9))
    ) ENGINE = {{ .AggregatingMergeTree }}
ORDER BY
    (name)
{{- if gt .TTLSeconds 0 }}
TTL last_seen + INTERVAL {{ .TTLSeconds }} SECOND DELETE
{{- end }}
