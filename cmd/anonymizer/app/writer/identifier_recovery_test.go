// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package writer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger/cmd/anonymizer/app/anonymizer"
	"github.com/jaegertracing/jaeger/cmd/anonymizer/app/uiconv"
)

const (
	lowEntropyService   = "frontend"
	lowEntropyOperation = "get"
)

// serviceDictionary is a stand-in for the short, human-readable names a
// recipient can try. It is not read from the mapping file.
func serviceDictionary() []string {
	return []string{
		"api", "auth", "checkout", "frontend", "gateway", "inventory",
		"mysql", "nginx", "order", "payment", "redis", "user",
	}
}

func operationDictionary() []string {
	return []string{
		"delete", "fetch", "get", "health", "list", "post", "query",
		"read", "search", "update", "write",
	}
}

// operationPreimages are the strings mapOperationName can be given.
// The live code hashes "[" + service + "]:" + operation, using the original
// service name. Bare operation names are included so a hash of only the
// operation is caught as well.
func operationPreimages() []string {
	preimages := make([]string, 0, len(operationDictionary())*(len(serviceDictionary())+1))
	for _, operation := range operationDictionary() {
		preimages = append(preimages, operation)
		for _, service := range serviceDictionary() {
			preimages = append(preimages, fmt.Sprintf("[%s]:%s", service, operation))
		}
	}
	return preimages
}

type sharedFiles struct {
	anonymized string
	ui         string
	mapping    string
	captured   string
}

func captureSpans(t *testing.T, opts anonymizer.Options, spans []*model.Span) sharedFiles {
	t.Helper()
	dir := t.TempDir()
	files := sharedFiles{
		anonymized: filepath.Join(dir, "trace.anonymized.json"),
		ui:         filepath.Join(dir, "trace.ui.json"),
		mapping:    filepath.Join(dir, "trace.mapping.json"),
		captured:   filepath.Join(dir, "trace.original.json"),
	}
	w, err := New(Config{
		CapturedFile:   files.captured,
		AnonymizedFile: files.anonymized,
		MappingFile:    files.mapping,
		AnonymizerOpts: opts,
	}, zap.NewNop())
	require.NoError(t, err)
	defer w.Close()

	traceID := spans[0].TraceID.String()
	for _, span := range spans {
		require.NoError(t, w.WriteSpan(span))
	}
	// Close before reading. The anonymized file has no trailing newline, and
	// therefore no closing bracket, until Close runs.
	w.Close()

	require.NoError(t, uiconv.Extract(uiconv.Config{
		CapturedFile: files.anonymized,
		UIFile:       files.ui,
		TraceID:      traceID,
	}, zap.NewNop()))
	return files
}

func sensitiveSpan(tags, processTags []model.KeyValue, logs []model.Log) *model.Span {
	return &model.Span{
		TraceID:       model.NewTraceID(1, 2),
		SpanID:        model.NewSpanID(1),
		OperationName: lowEntropyOperation,
		Tags:          tags,
		Logs:          logs,
		Duration:      time.Second,
		StartTime:     time.Unix(300, 0),
		Process: &model.Process{
			ServiceName: lowEntropyService,
			Tags:        processTags,
		},
	}
}

type publishedKV struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

type publishedSpan struct {
	OperationName string        `json:"operationName"`
	Tags          []publishedKV `json:"tags"`
	Logs          []struct {
		Fields []publishedKV `json:"fields"`
	} `json:"logs"`
	Process *struct {
		ServiceName string        `json:"serviceName"`
		Tags        []publishedKV `json:"tags"`
	} `json:"process"`
}

func loadJSON(t *testing.T, path string) any {
	t.Helper()
	buf, err := os.ReadFile(path)
	require.NoError(t, err)
	var v any
	require.NoError(t, json.Unmarshal(buf, &v), "file %s is not JSON", path)
	return v
}

func loadAnonymizedSpans(t *testing.T, path string) []publishedSpan {
	t.Helper()
	buf, err := os.ReadFile(path)
	require.NoError(t, err)
	var spans []publishedSpan
	require.NoError(t, json.Unmarshal(buf, &spans), "anonymized file is not a span array")
	return spans
}

func allStrings(v any) []string {
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			for key, child := range n {
				out = append(out, key)
				walk(child)
			}
		case []any:
			for _, child := range n {
				walk(child)
			}
		case string:
			out = append(out, n)
		}
	}
	walk(v)
	return out
}

func fnv64Hex(s string) (string, error) {
	h := fnv.New64()
	if _, err := h.Write([]byte(s)); err != nil {
		return "", err
	}
	return fmt.Sprintf("%016x", h.Sum64()), nil
}

func macEncodings(key, msg string) ([]string, error) {
	m := hmac.New(sha256.New, []byte(key))
	if _, err := m.Write([]byte(msg)); err != nil {
		return nil, err
	}
	sum := m.Sum(nil)
	full := hex.EncodeToString(sum)
	return []string{
		full,
		fmt.Sprintf("%X", sum),
		full[:16],
		base64.StdEncoding.EncodeToString(sum),
		base64.RawStdEncoding.EncodeToString(sum),
		base64.URLEncoding.EncodeToString(sum),
		base64.RawURLEncoding.EncodeToString(sum),
	}, nil
}

// recoveryHits reports dictionary preimages whose public hash equals published.
// Keys are taken only from the shared JSON, which is all the recipient has.
func recoveryHits(t *testing.T, published string, preimages, leakedKeys []string) []string {
	t.Helper()
	var hits []string
	for _, preimage := range preimages {
		sum, err := fnv64Hex(preimage)
		require.NoError(t, err)
		if published == sum {
			hits = append(hits, fmt.Sprintf("unsalted FNV-64(%q) = %s", preimage, published))
		}
		for _, key := range leakedKeys {
			encodings, err := macEncodings(key, preimage)
			require.NoError(t, err)
			for _, encoding := range encodings {
				if published == encoding {
					hits = append(hits, fmt.Sprintf("HMAC-SHA256 of %q with a key found in the shared file", preimage))
					break
				}
			}
		}
	}
	return hits
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func intersection(values []string, banned []string) []string {
	var present []string
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	for _, value := range banned {
		if _, ok := set[value]; ok {
			present = append(present, value)
		}
	}
	return present
}

// mappingSecrets are long strings that stay in the owner mapping file and do
// not appear in either file the operator shares.
func mappingSecrets(t *testing.T, files sharedFiles) []string {
	t.Helper()
	shared := map[string]struct{}{}
	for _, value := range allStrings(loadJSON(t, files.anonymized)) {
		shared[value] = struct{}{}
	}
	for _, value := range allStrings(loadJSON(t, files.ui)) {
		shared[value] = struct{}{}
	}
	var secrets []string
	for _, value := range allStrings(loadJSON(t, files.mapping)) {
		if _, leaked := shared[value]; leaked || len(value) < 32 {
			continue
		}
		secrets = append(secrets, value)
	}
	return secrets
}

func TestSharedTraceHidesServiceAndOperationNames(t *testing.T) {
	first := sensitiveSpan(nil, nil, nil)
	second := sensitiveSpan(nil, nil, nil)
	second.SpanID = model.NewSpanID(2)
	files := captureSpans(t, anonymizer.Options{}, []*model.Span{first, second})

	captured, err := os.ReadFile(files.captured)
	require.NoError(t, err)
	require.Contains(t, string(captured), lowEntropyService)
	require.Contains(t, string(captured), lowEntropyOperation)

	spans := loadAnonymizedSpans(t, files.anonymized)
	require.Len(t, spans, 2)
	require.NotNil(t, spans[0].Process)
	require.NotNil(t, spans[1].Process)
	require.Equal(t, spans[0].Process.ServiceName, spans[1].Process.ServiceName, "one run must keep a stable service identifier")
	require.Equal(t, spans[0].OperationName, spans[1].OperationName, "one run must keep a stable operation identifier")
	require.NotEqual(t, lowEntropyService, spans[0].Process.ServiceName)
	require.NotEqual(t, lowEntropyOperation, spans[0].OperationName)

	var owner struct {
		Services   map[string]string
		Operations map[string]string
	}
	mappingBuf, err := os.ReadFile(files.mapping)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(mappingBuf, &owner))
	require.Equal(t, spans[0].Process.ServiceName, owner.Services[lowEntropyService])
	require.Equal(t, spans[0].OperationName, owner.Operations[fmt.Sprintf("[%s]:%s", lowEntropyService, lowEntropyOperation)])
	secretCount := len(mappingSecrets(t, files))

	// The recipient is left with the two files the command writes for sharing.
	require.NoError(t, os.Remove(files.mapping))
	require.NoError(t, os.Remove(files.captured))

	anonymizedStrings := allStrings(loadJSON(t, files.anonymized))
	uiStrings := allStrings(loadJSON(t, files.ui))
	sharedStrings := uniqueStrings(append(anonymizedStrings, uiStrings...))
	require.Empty(t, intersection(sharedStrings, []string{
		lowEntropyService,
		lowEntropyOperation,
		fmt.Sprintf("[%s]:%s", lowEntropyService, lowEntropyOperation),
	}), "plaintext identifiers were published in the shared JSON")
	require.Contains(t, anonymizedStrings, spans[0].Process.ServiceName)
	require.Contains(t, anonymizedStrings, spans[0].OperationName)
	require.Contains(t, uiStrings, spans[0].Process.ServiceName)
	require.Contains(t, uiStrings, spans[0].OperationName)

	var problems []string
	problems = append(problems, recoveryHits(t, spans[0].Process.ServiceName, serviceDictionary(), sharedStrings)...)
	problems = append(problems, recoveryHits(t, spans[0].OperationName, operationPreimages(), sharedStrings)...)
	if secretCount == 0 {
		problems = append(problems, "mapping file has no per-run secret that is absent from the shared JSON")
	}
	require.Empty(t, strings.Join(problems, "\n"))
}

func hashedTagFixtures() (tags, processTags []model.KeyValue, logs []model.Log, preimages []string) {
	tags = []model.KeyValue{
		model.Bool("error", true),
		model.String("http.method", "POST"),
		model.Int64("http.status_code", 201),
		model.String("customer", "alice"),
	}
	processTags = []model.KeyValue{
		model.String("pod", "web"),
	}
	// HashLogs is exercised by the caller. The hashed log fields are not part
	// of preimages: AnonymizeSpan writes them onto the range variable, so the
	// value WriteSpan publishes is not that hash.
	logs = []model.Log{{
		Timestamp: time.Unix(300, 0),
		Fields: []model.KeyValue{
			model.String("event", "login"),
		},
	}}
	seen := map[string]struct{}{}
	add := func(value string) {
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		preimages = append(preimages, value)
	}
	for _, tag := range append(append([]model.KeyValue{}, tags...), processTags...) {
		add(tag.Key)
		add(tag.AsString())
	}
	for _, decoy := range []string{"admin", "root", "password", "false", "GET", "200"} {
		add(decoy)
	}
	return tags, processTags, logs, preimages
}

func publishedTagTokens(span publishedSpan) []publishedKV {
	tokens := append([]publishedKV{}, span.Tags...)
	if span.Process != nil {
		tokens = append(tokens, span.Process.Tags...)
	}
	return tokens
}

func TestSharedTraceHidesHashedTagLogAndProcessValues(t *testing.T) {
	tags, processTags, logs, preimages := hashedTagFixtures()
	opts := anonymizer.Options{
		HashStandardTags: true,
		HashCustomTags:   true,
		HashLogs:         true,
		HashProcess:      true,
	}
	first := captureSpans(t, opts, []*model.Span{sensitiveSpan(tags, processTags, logs)})
	second := captureSpans(t, opts, []*model.Span{sensitiveSpan(tags, processTags, logs)})

	firstSpans := loadAnonymizedSpans(t, first.anonymized)
	secondSpans := loadAnonymizedSpans(t, second.anonymized)
	require.Len(t, firstSpans, 1)
	require.Len(t, secondSpans, 1)
	firstTokens := publishedTagTokens(firstSpans[0])
	secondTokens := publishedTagTokens(secondSpans[0])
	// 3 allow-listed span tags + 1 custom tag + 1 process tag.
	require.Len(t, firstTokens, 5)
	require.Len(t, secondTokens, 5)

	require.NoError(t, os.Remove(first.mapping))
	require.NoError(t, os.Remove(first.captured))
	anonymizedStrings := allStrings(loadJSON(t, first.anonymized))
	uiStrings := allStrings(loadJSON(t, first.ui))
	sharedStrings := uniqueStrings(append(anonymizedStrings, uiStrings...))
	for _, token := range firstTokens {
		require.Contains(t, anonymizedStrings, token.Key)
		require.Contains(t, uiStrings, token.Key)
	}
	plaintext := []string{"error", "true", "http.method", "POST", "http.status_code", "201", "customer", "alice", "pod", "web"}
	require.Empty(t, intersection(sharedStrings, plaintext),
		"plaintext tag or process values were published")

	var problems []string
	for i, token := range firstTokens {
		value, ok := token.Value.(string)
		require.True(t, ok, "hashed tag value %d was not published as a string", i)
		if token.Key == secondTokens[i].Key || value == stringValue(secondTokens[i].Value) {
			problems = append(problems, fmt.Sprintf("tag token %d is identical across runs", i))
		}
		problems = append(problems, recoveryHits(t, token.Key, preimages, sharedStrings)...)
		problems = append(problems, recoveryHits(t, value, preimages, sharedStrings)...)
	}
	require.Empty(t, strings.Join(problems, "\n"))
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func TestSeparateAnonymizerRunsPublishDifferentIdentifiers(t *testing.T) {
	opts := anonymizer.Options{}
	first := captureSpans(t, opts, []*model.Span{sensitiveSpan(nil, nil, nil)})
	second := captureSpans(t, opts, []*model.Span{sensitiveSpan(nil, nil, nil)})
	firstSpans := loadAnonymizedSpans(t, first.anonymized)
	secondSpans := loadAnonymizedSpans(t, second.anonymized)
	require.Len(t, firstSpans, 1)
	require.Len(t, secondSpans, 1)
	require.NotNil(t, firstSpans[0].Process)
	require.NotNil(t, secondSpans[0].Process)
	require.NotEmpty(t, firstSpans[0].Process.ServiceName)
	require.NotEmpty(t, firstSpans[0].OperationName)
	require.NotEqual(t, firstSpans[0].Process.ServiceName, secondSpans[0].Process.ServiceName,
		"service identifiers from two runs match, so they do not depend on a per-run secret")
	require.NotEqual(t, firstSpans[0].OperationName, secondSpans[0].OperationName,
		"operation identifiers from two runs match, so they do not depend on a per-run secret")
}
