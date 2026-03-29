// Copyright (c) 2018 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package spanstore

import (
	"sort"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"

	"github.com/jaegertracing/jaeger/internal/storage/v1/api/spanstore"
)

// CacheStore saves expensive calculations from the K/V store
type CacheStore struct {
	// Given the small amount of data these will store, we use the same structure as the memory store
	cacheLock  sync.Mutex // write heavy - Mutex is faster than RWMutex for writes
	services   map[string]uint64
	operations map[string]map[spanstore.Operation]uint64

	store *badger.DB
	ttl   time.Duration
}

// NewCacheStore returns initialized CacheStore for badger use
func NewCacheStore(db *badger.DB, ttl time.Duration) *CacheStore {
	cs := &CacheStore{
		services:   make(map[string]uint64),
		operations: make(map[string]map[spanstore.Operation]uint64),
		ttl:        ttl,
		store:      db,
	}
	return cs
}

// AddService fills the services into the cache with the most updated expiration time
func (c *CacheStore) AddService(service string, keyTTL uint64) {
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()
	if v, found := c.services[service]; found {
		if v > keyTTL {
			return
		}
	}
	c.services[service] = keyTTL
}

// AddOperation adds the cache with operation names with most updated expiration time
func (c *CacheStore) AddOperation(service, operation, spanKind string, keyTTL uint64) {
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()
	if _, found := c.operations[service]; !found {
		c.operations[service] = make(map[spanstore.Operation]uint64)
	}
	op := spanstore.Operation{Name: operation, SpanKind: spanKind}
	if v, found := c.operations[service][op]; found {
		if v > keyTTL {
			return
		}
	}
	c.operations[service][op] = keyTTL
}

// Update caches the results of service and service + operation indexes and maintains their TTL
func (c *CacheStore) Update(service, operation, spanKind string, expireTime uint64) {
	c.cacheLock.Lock()

	c.services[service] = expireTime
	if _, ok := c.operations[service]; !ok {
		c.operations[service] = make(map[spanstore.Operation]uint64)
	}
	op := spanstore.Operation{Name: operation, SpanKind: spanKind}
	c.operations[service][op] = expireTime
	c.cacheLock.Unlock()
}

// GetOperations returns all operations for a specific service & spanKind traced by Jaeger
func (c *CacheStore) GetOperations(service, spanKind string) ([]spanstore.Operation, error) {
	//nolint:gosec // G115
	t := uint64(time.Now().Unix())
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()

	if v, ok := c.services[service]; ok {
		if v < t {
			// Expired, remove
			delete(c.services, service)
			delete(c.operations, service)
			return []spanstore.Operation{}, nil // empty slice rather than nil
		}

		result := make([]spanstore.Operation, 0, len(c.operations[service]))
		for op, e := range c.operations[service] {
			if e > t {
				if spanKind == "" || spanKind == op.SpanKind {
					result = append(result, op)
				}
			} else {
				delete(c.operations[service], op)
			}
		}

		sort.Slice(result, func(i, j int) bool {
			if result[i].Name != result[j].Name {
				return result[i].Name < result[j].Name
			}
			return result[i].SpanKind < result[j].SpanKind
		})

		return result, nil
	}

	return []spanstore.Operation{}, nil
}

// GetServices returns all services traced by Jaeger
func (c *CacheStore) GetServices() ([]string, error) {
	services := make([]string, 0, len(c.services))
	//nolint:gosec // G115
	t := uint64(time.Now().Unix())
	c.cacheLock.Lock()
	// Fetch the items
	for k, v := range c.services {
		if v > t {
			services = append(services, k)
		} else {
			// Service has expired, remove it
			delete(c.services, k)
		}
	}
	c.cacheLock.Unlock()

	sort.Strings(services)

	return services, nil
}
