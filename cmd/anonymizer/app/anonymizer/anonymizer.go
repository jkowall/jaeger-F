// Copyright (c) 2020 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package anonymizer

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger/internal/uimodel"
	uiconv "github.com/jaegertracing/jaeger/internal/uimodel/converter/v1/json"
)

var allowedTags = map[string]bool{
	"error":               true,
	"http.method":         true,
	"http.status_code":    true,
	model.SpanKindKey:     true,
	model.SamplerTypeKey:  true,
	model.SamplerParamKey: true,
}

const PermUserRW = 0o600 // Read-write for owner only

const hashKeySize = 32

// mapping stores the mapping of service/operation names to their keyed hashes,
// so that we can do a reverse lookup should the researchers have questions.
type mapping struct {
	// HashKey is the hex-encoded HMAC key for all hashes in the anonymized output.
	// Anyone holding it can test guesses against those hashes, so it must only be
	// stored in the owner-only mapping file.
	HashKey    string
	Services   map[string]string
	Operations map[string]string // key=[service]:operation
}

// Anonymizer transforms Jaeger span in the domain model by obfuscating site-specific strings,
// like service and operation names, and removes custom tags. It returns obfuscated span in the
// Jaeger UI format, to make it easy to visualize traces.
//
// The mapping from original to obfuscated strings is stored in a file and can be reused between runs.
type Anonymizer struct {
	mappingFile string
	logger      *zap.Logger
	lock        sync.Mutex
	mapping     mapping
	options     Options
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

// Options represents the various options with which the anonymizer can be configured.
type Options struct {
	HashStandardTags bool `yaml:"hash_standard_tags" name:"hash_standard_tags"`
	HashCustomTags   bool `yaml:"hash_custom_tags" name:"hash_custom_tags"`
	HashLogs         bool `yaml:"hash_logs" name:"hash_logs"`
	HashProcess      bool `yaml:"hash_process" name:"hash_process"`
}

// New creates new Anonymizer. The mappingFile stores the mapping from original to
// obfuscated strings, in case later investigations require looking at the original traces.
func New(mappingFile string, options Options, logger *zap.Logger) *Anonymizer {
	ctx, cancel := context.WithCancel(context.Background())
	a := &Anonymizer{
		mappingFile: mappingFile,
		logger:      logger,
		mapping: mapping{
			Services:   make(map[string]string),
			Operations: make(map[string]string),
		},
		options: options,
		cancel:  cancel,
	}
	if _, err := os.Stat(filepath.Clean(mappingFile)); err == nil {
		dat, err := os.ReadFile(filepath.Clean(mappingFile))
		if err != nil {
			logger.Fatal("Cannot load previous mapping", zap.Error(err))
		}
		if err := json.Unmarshal(dat, &a.mapping); err != nil {
			logger.Fatal("Cannot unmarshal previous mapping", zap.Error(err))
		}
	}
	if a.mapping.HashKey == "" {
		a.mapping.HashKey = newHashKey()
	}
	a.wg.Go(func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.SaveMapping()
			case <-ctx.Done():
				return
			}
		}
	})
	return a
}

func (a *Anonymizer) Stop() {
	a.cancel()
	a.wg.Wait()
}

// SaveMapping writes the mapping from original to obfuscated strings to a file.
// It is called by the anonymizer itself periodically, and should be called at
// the end of the extraction run.
func (a *Anonymizer) SaveMapping() {
	a.lock.Lock()
	defer a.lock.Unlock()
	dat, err := json.Marshal(a.mapping)
	if err != nil {
		a.logger.Error("Failed to marshal mapping file", zap.Error(err))
		return
	}
	if err := os.WriteFile(filepath.Clean(a.mappingFile), dat, PermUserRW); err != nil {
		a.logger.Error("Failed to write mapping file", zap.Error(err))
		return
	}
	a.logger.Sugar().Infof("Saved mapping file %s: %d services, %d operations",
		a.mappingFile, len(a.mapping.Services), len(a.mapping.Operations))
}

func (a *Anonymizer) mapServiceName(service string) string {
	return a.mapString(service, a.mapping.Services)
}

func (a *Anonymizer) mapOperationName(service, operation string) string {
	v := fmt.Sprintf("[%s]:%s", service, operation)
	return a.mapString(v, a.mapping.Operations)
}

func (a *Anonymizer) mapString(v string, m map[string]string) string {
	a.lock.Lock()
	defer a.lock.Unlock()
	if s, ok := m[v]; ok {
		return s
	}
	s := a.hash(v)
	m[v] = s
	return s
}

func newHashKey() string {
	key := make([]byte, hashKeySize)
	_, _ = rand.Read(key) // crypto/rand.Read never returns an error, it crashes the program instead
	return hex.EncodeToString(key)
}

// hash returns a truncated HMAC-SHA256 of value under the mapping's secret key,
// so that it cannot be recomputed from a dictionary of candidate names.
func (a *Anonymizer) hash(value string) string {
	m := hmac.New(sha256.New, []byte(a.mapping.HashKey))
	_, _ = m.Write([]byte(value))
	return hex.EncodeToString(m.Sum(nil)[:8])
}

// AnonymizeSpan obfuscates and converts the span.
func (a *Anonymizer) AnonymizeSpan(span *model.Span) *uimodel.Span {
	service := span.Process.ServiceName
	span.OperationName = a.mapOperationName(service, span.OperationName)

	outputTags := filterStandardTags(span.Tags)
	// when true, the allowedTags are hashed and when false they are preserved as it is
	if a.options.HashStandardTags {
		outputTags = a.hashTags(outputTags)
	}
	// when true, all tags other than allowedTags are hashed, when false they are dropped
	if a.options.HashCustomTags {
		customTags := a.hashTags(filterCustomTags(span.Tags))
		outputTags = append(outputTags, customTags...)
	}
	span.Tags = outputTags

	// when true, logs are hashed, when false, they are dropped
	if a.options.HashLogs {
		for _, log := range span.Logs {
			log.Fields = a.hashTags(log.Fields)
		}
	} else {
		span.Logs = nil
	}

	span.Process.ServiceName = a.mapServiceName(service)

	// when true, process tags are hashed, when false they are dropped
	if a.options.HashProcess {
		span.Process.Tags = a.hashTags(span.Process.Tags)
	} else {
		span.Process.Tags = nil
	}

	span.Warnings = nil
	return uiconv.FromDomainEmbedProcess(span)
}

// filterStandardTags returns only allowedTags
func filterStandardTags(tags []model.KeyValue) []model.KeyValue {
	out := make([]model.KeyValue, 0, len(tags))
	for _, tag := range tags {
		if !allowedTags[tag.Key] {
			continue
		}
		if tag.Key == "error" {
			switch tag.VType {
			case model.BoolType:
				// allowed
			case model.StringType:
				if tag.VStr != "true" && tag.VStr != "false" {
					tag = model.Bool("error", true)
				}
			default:
				tag = model.Bool("error", true)
			}
		}
		out = append(out, tag)
	}
	return out
}

// filterCustomTags returns all tags other than allowedTags
func filterCustomTags(tags []model.KeyValue) []model.KeyValue {
	out := make([]model.KeyValue, 0, len(tags))
	for _, tag := range tags {
		if !allowedTags[tag.Key] {
			out = append(out, tag)
		}
	}
	return out
}

// hashTags converts each tag into corresponding string values
// and then find its hash
func (a *Anonymizer) hashTags(tags []model.KeyValue) []model.KeyValue {
	out := make([]model.KeyValue, 0, len(tags))
	for _, tag := range tags {
		kv := model.String(a.hash(tag.Key), a.hash(tag.AsString()))
		out = append(out, kv)
	}
	return out
}
