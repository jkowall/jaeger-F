CREATE TABLE
    IF NOT EXISTS attribute_metadata{{ .LocalTableSuffix }} (
        attribute_key String,
        type String,  -- 'bool', 'double', 'int', 'str', 'bytes', 'map', 'slice'
        level String,  -- 'resource', 'scope', 'span'
        last_seen SimpleAggregateFunction(max, DateTime64(9))
    ) ENGINE = {{ .AggregatingMergeTree }}
ORDER BY (attribute_key, type, level)
{{- if gt .TTLSeconds 0 }}
TTL last_seen + INTERVAL {{ .TTLSeconds }} SECOND DELETE
{{- end }}
