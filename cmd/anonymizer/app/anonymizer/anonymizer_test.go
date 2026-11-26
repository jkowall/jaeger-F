// Copyright (c) 2020 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package anonymizer

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger-idl/model/v1"
)

var tags = []model.KeyValue{
	model.Bool("error", true),
	model.String("http.method", http.MethodPost),
	model.Bool("foobar", true),
}

var traceID = model.NewTraceID(1, 2)

var span1 = &model.Span{
	TraceID: traceID,
	SpanID:  model.NewSpanID(1),
	Process: &model.Process{
		ServiceName: "serviceName",
		Tags:        tags,
	},
	OperationName: "operationName",
	Tags:          tags,
	Logs: []model.Log{
		{
			Timestamp: time.Now(),
			Fields: []model.KeyValue{
				model.String("logKey", "logValue"),
			},
		},
	},
	Duration:  time.Second * 5,
	StartTime: time.Unix(300, 0),
}

var span2 = &model.Span{
	TraceID: traceID,
	SpanID:  model.NewSpanID(1),
	Process: &model.Process{
		ServiceName: "serviceName",
		Tags:        tags,
	},
	OperationName: "operationName",
	Tags:          tags,
	Logs: []model.Log{
		{
			Timestamp: time.Now(),
			Fields: []model.KeyValue{
				model.String("logKey", "logValue"),
			},
		},
	},
	Duration:  time.Second * 5,
	StartTime: time.Unix(300, 0),
}

func TestNew(t *testing.T) {
	nopLogger := zap.NewNop()
	tempDir := t.TempDir()

	file, err := os.CreateTemp(tempDir, "mapping.json")
	require.NoError(t, err)
	defer file.Close()

	_, err = file.WriteString(`
{
    "services": {
		"api": "hashed_api"
	},
	"operations": {
		"[api]:delete": "hashed_api_delete"
	}
}
`)
	require.NoError(t, err)

	anonymizer := New(file.Name(), Options{}, nopLogger)
	defer anonymizer.Stop()
	assert.NotNil(t, anonymizer)
}

func TestAnonymizer_SaveMapping(t *testing.T) {
	nopLogger := zap.NewNop()
	mapping := mapping{
		Services:   make(map[string]string),
		Operations: make(map[string]string),
	}

	tests := []struct {
		name        string
		mappingFile string
	}{
		{
			name:        "fail to write mapping file",
			mappingFile: "",
		},
		{
			name:        "save mapping file successfully",
			mappingFile: filepath.Join(t.TempDir(), "mapping.json"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(_ *testing.T) {
			anonymizer := Anonymizer{
				logger:      nopLogger,
				mapping:     mapping,
				mappingFile: tt.mappingFile,
			}
			anonymizer.SaveMapping()
		})
	}
}

func TestAnonymizer_FilterStandardTags(t *testing.T) {
	expected := []model.KeyValue{
		model.Bool("error", true),
		model.String("http.method", http.MethodPost),
	}
	actual := filterStandardTags(tags)
	assert.Equal(t, expected, actual)
}

func TestAnonymizer_FilterCustomTags(t *testing.T) {
	expected := []model.KeyValue{
		model.Bool("foobar", true),
	}
	actual := filterCustomTags(tags)
	assert.Equal(t, expected, actual)
}

func TestNew_GeneratesHashKey(t *testing.T) {
	mappingFile := filepath.Join(t.TempDir(), "mapping.json")
	first := New(mappingFile, Options{}, zap.NewNop())
	defer first.Stop()
	second := New(filepath.Join(t.TempDir(), "mapping.json"), Options{}, zap.NewNop())
	defer second.Stop()

	key, err := hex.DecodeString(first.mapping.HashKey)
	require.NoError(t, err)
	assert.Len(t, key, hashKeySize)
	assert.NotEqual(t, first.mapping.HashKey, second.mapping.HashKey)
	assert.NotEqual(t, first.mapServiceName("foobar"), second.mapServiceName("foobar"))

	first.SaveMapping()
	dat, err := os.ReadFile(mappingFile)
	require.NoError(t, err)
	var saved mapping
	require.NoError(t, json.Unmarshal(dat, &saved))
	assert.Equal(t, first.mapping.HashKey, saved.HashKey)
}

func TestNew_ReusesHashKeyFromMappingFile(t *testing.T) {
	mappingFile := filepath.Join(t.TempDir(), "mapping.json")
	first := New(mappingFile, Options{}, zap.NewNop())
	first.Stop()
	first.SaveMapping()

	second := New(mappingFile, Options{}, zap.NewNop())
	defer second.Stop()
	assert.Equal(t, first.mapping.HashKey, second.mapping.HashKey)
	assert.Equal(t, first.hash("foobar"), second.hash("foobar"))
}

func TestAnonymizer_Hash(t *testing.T) {
	anonymizer := &Anonymizer{mapping: mapping{HashKey: "key"}}
	other := &Anonymizer{mapping: mapping{HashKey: "other-key"}}
	actual := anonymizer.hash("foobar")
	assert.Equal(t, "37508e74cc6edbed", actual)
	assert.Equal(t, actual, anonymizer.hash("foobar"))
	assert.NotEqual(t, actual, anonymizer.hash("foobaz"))
	assert.NotEqual(t, actual, other.hash("foobar"))
}

func TestAnonymizer_AnonymizeSpan_AllTrue(t *testing.T) {
	anonymizer := &Anonymizer{
		mapping: mapping{
			Services:   make(map[string]string),
			Operations: make(map[string]string),
		},
		options: Options{
			HashStandardTags: true,
			HashCustomTags:   true,
			HashProcess:      true,
			HashLogs:         true,
		},
	}
	_ = anonymizer.AnonymizeSpan(span1)
	assert.Len(t, span1.Tags, 3)
	assert.Len(t, span1.Logs, 1)
	assert.Len(t, span1.Process.Tags, 3)
}

func TestAnonymizer_AnonymizeSpan_AllFalse(t *testing.T) {
	anonymizer := &Anonymizer{
		mapping: mapping{
			Services:   make(map[string]string),
			Operations: make(map[string]string),
		},
		options: Options{
			HashStandardTags: false,
			HashCustomTags:   false,
			HashProcess:      false,
			HashLogs:         false,
		},
	}
	_ = anonymizer.AnonymizeSpan(span2)
	assert.Len(t, span2.Tags, 2)
	assert.Empty(t, span2.Logs)
	assert.Empty(t, span2.Process.Tags)
}

func TestAnonymizer_MapString_Present(t *testing.T) {
	v := "foobar"
	m := map[string]string{
		"foobar": "hashed_foobar",
	}
	anonymizer := &Anonymizer{}
	actual := anonymizer.mapString(v, m)
	assert.Equal(t, "hashed_foobar", actual)
}

func TestAnonymizer_MapString_Absent(t *testing.T) {
	v := "foobar"
	m := map[string]string{}
	anonymizer := &Anonymizer{mapping: mapping{HashKey: "key"}}
	actual := anonymizer.mapString(v, m)
	assert.Equal(t, anonymizer.hash(v), actual)
	assert.Equal(t, actual, m[v])
}

func TestAnonymizer_MapServiceName(t *testing.T) {
	anonymizer := &Anonymizer{
		mapping: mapping{
			Services: map[string]string{
				"api": "hashed_api",
			},
		},
	}
	actual := anonymizer.mapServiceName("api")
	assert.Equal(t, "hashed_api", actual)
}

func TestAnonymizer_MapOperationName(t *testing.T) {
	anonymizer := &Anonymizer{
		mapping: mapping{
			Services: map[string]string{
				"api": "hashed_api",
			},
			Operations: map[string]string{
				"[api]:delete": "hashed_api_delete",
			},
		},
	}
	actual := anonymizer.mapOperationName("api", "delete")
	assert.Equal(t, "hashed_api_delete", actual)
}
