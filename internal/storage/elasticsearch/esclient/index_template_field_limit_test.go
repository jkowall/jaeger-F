// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package esclient

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	es "github.com/jaegertracing/jaeger/internal/storage/elasticsearch"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/config"
)

// elasticsearchDefaultTotalFieldsLimit is index.mapping.total_fields.limit in
// Elasticsearch 7.17.22, 8.19.0, and 9.5.3 (MapperService.INDEX_MAPPING_TOTAL_FIELDS_LIMIT_SETTING).
// An index template that omits the setting inherits whatever the cluster default
// is, including a default an operator has raised. The span template has to pin
// its own limit at or below this value so a raised cluster default cannot give
// attacker-chosen attribute keys a larger mapping.
const elasticsearchDefaultTotalFieldsLimit = 1000

func spanIndices(numericAttributes bool) config.Indices {
	reps := int64(1)
	return config.Indices{
		Spans: config.SpanIndexOptions{
			IndexOptions:      config.IndexOptions{Shards: 5, Replicas: &reps},
			NumericAttributes: numericAttributes,
		},
		Services:     config.IndexOptions{Replicas: &reps},
		Dependencies: config.IndexOptions{Replicas: &reps},
		Sampling:     config.IndexOptions{Replicas: &reps},
	}
}

// TestSpanTemplatePinsTotalFieldsLimitWhenUnset renders the span index template
// with no indices.spans.total_fields_limit configured. Both the rotation template
// and the data-stream settings component must still declare
// index.mapping.total_fields.limit, at or below Elasticsearch's own default.
func TestSpanTemplatePinsTotalFieldsLimitWhenUnset(t *testing.T) {
	for _, numeric := range []bool{false, true} {
		indices := spanIndices(numeric)
		t.Run(fmt.Sprintf("numeric_attributes=%t", numeric), func(t *testing.T) {
			for _, version := range es.AllVersions {
				t.Run(version.String(), func(t *testing.T) {
					rendered, err := RenderIndexTemplate(SpanMapping, indices, false, "", version)
					require.NoError(t, err)
					assertPinnedTotalFieldsLimit(t, []byte(rendered))
				})
			}
			t.Run("data-stream", func(t *testing.T) {
				templates, err := renderSpanDataStreamTemplates(indices)
				require.NoError(t, err)
				var settings []byte
				for _, template := range templates {
					if template.api != componentTemplateAPI || template.body == "" {
						continue
					}
					var doc map[string]any
					require.NoError(t, json.Unmarshal([]byte(template.body), &doc))
					inner, _ := doc["template"].(map[string]any)
					if _, ok := inner["settings"]; ok {
						settings = []byte(template.body)
					}
				}
				require.NotEmpty(t, settings, "data stream settings component was not rendered")
				assertPinnedTotalFieldsLimit(t, settings)
			})
		})
	}
}

func assertPinnedTotalFieldsLimit(t *testing.T, body []byte) {
	t.Helper()
	var doc any
	require.NoError(t, json.Unmarshal(body, &doc))
	limit, ok := findJSONNumber(doc, "index.mapping.total_fields.limit")
	require.True(t, ok, "span index template omits index.mapping.total_fields.limit: %s", body)
	assert.GreaterOrEqual(t, limit, 1.0)
	assert.LessOrEqual(t, limit, float64(elasticsearchDefaultTotalFieldsLimit))
}

func findJSONNumber(v any, key string) (float64, bool) {
	switch node := v.(type) {
	case map[string]any:
		if raw, ok := node[key]; ok {
			number, isNumber := raw.(float64)
			return number, isNumber
		}
		for _, child := range node {
			if number, ok := findJSONNumber(child, key); ok {
				return number, true
			}
		}
	case []any:
		for _, child := range node {
			if number, ok := findJSONNumber(child, key); ok {
				return number, true
			}
		}
	}
	return 0, false
}
