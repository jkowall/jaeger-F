// Copyright (c) 2018 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package spanstore

import (
	"slices"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"

	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
)

// Service and operation names come from ingested spans, so the cache is bounded
// to keep a stream of distinct names from growing the heap for the span TTL.
// When a limit is reached, the entries closest to expiry are evicted first.
const (
	maxCachedServices             = 1024
	maxCachedOperationsPerService = 1024
	// maxCachedBytes bounds the total size of cached names across all services,
	// each name being charged its length plus cacheEntryOverhead.
	maxCachedBytes     = 32 << 20
	cacheEntryOverhead = 64
)

// CacheStore saves expensive calculations from the K/V store
type CacheStore struct {
	// Given the small amount of data these will store, we use the same structure as the memory store
	cacheLock  sync.Mutex // write heavy - Mutex is faster than RWMutex for writes
	services   map[string]uint64
	operations map[string]map[string]uint64
	// size is the accounted size of the cached names, see maxCachedBytes.
	size int

	store *badger.DB
	ttl   time.Duration
}

// NewCacheStore returns initialized CacheStore for badger use
func NewCacheStore(db *badger.DB, ttl time.Duration) *CacheStore {
	cs := &CacheStore{
		services:   make(map[string]uint64),
		operations: make(map[string]map[string]uint64),
		ttl:        ttl,
		store:      db,
	}
	return cs
}

func nowUnix() uint64 {
	//nolint:gosec // G115
	return uint64(time.Now().Unix())
}

func entrySize(name string) int {
	return len(name) + cacheEntryOverhead
}

// AddService fills the services into the cache with the most updated expiration time
func (c *CacheStore) AddService(service string, keyTTL uint64) {
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()
	c.putService(service, keyTTL, nowUnix())
}

// AddOperation adds the cache with operation names with most updated expiration time
func (c *CacheStore) AddOperation(service, operation string, keyTTL uint64) {
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()
	now := nowUnix()
	if c.putService(service, keyTTL, now) {
		c.putOperation(service, operation, keyTTL, now)
	}
}

// Update caches the results of service and service + operation indexes and maintains their TTL
func (c *CacheStore) Update(service, operation string, expireTime uint64) {
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()
	now := nowUnix()
	if c.putService(service, expireTime, now) {
		c.putOperation(service, operation, expireTime, now)
	}
}

// putService records the service with the latest of its known expiry times.
// It returns false if the service is not cached because keyTTL has passed.
// Must be called with cacheLock held.
func (c *CacheStore) putService(service string, keyTTL, now uint64) bool {
	if keyTTL <= now {
		return false
	}
	if v, found := c.services[service]; found {
		if v < keyTTL {
			c.services[service] = keyTTL
		}
		return true
	}
	c.sweepExpiredServices(now)
	for len(c.services) >= maxCachedServices {
		c.removeService(soonestExpiring(c.services))
	}
	c.reserve(service, entrySize(service), now)
	c.services[service] = keyTTL
	c.size += entrySize(service)
	return true
}

// putOperation records the operation of an already cached service with the
// latest of its known expiry times. Must be called with cacheLock held.
func (c *CacheStore) putOperation(service, operation string, keyTTL, now uint64) {
	ops, found := c.operations[service]
	if found {
		if v, found := ops[operation]; found {
			if v < keyTTL {
				ops[operation] = keyTTL
			}
			return
		}
		c.sweepExpiredOperations(service, now)
		for len(ops) >= maxCachedOperationsPerService {
			c.removeOperation(service, soonestExpiring(ops))
		}
	}
	need := entrySize(operation)
	if !found {
		need += entrySize(service)
	}
	c.reserve(service, need, now)
	if ops = c.operations[service]; ops == nil {
		ops = make(map[string]uint64)
		c.operations[service] = ops
	}
	ops[operation] = keyTTL
	c.size += need
}

// reserve evicts entries until need more bytes fit within maxCachedBytes,
// preferring the operations of other services over those of keep.
// Must be called with cacheLock held.
func (c *CacheStore) reserve(keep string, need int, now uint64) {
	if c.size+need <= maxCachedBytes {
		return
	}
	c.sweepExpiredServices(now)
	for c.size+need > maxCachedBytes {
		var withOps, oldest string
		var withOpsTTL, oldestTTL uint64
		foundWithOps, foundOldest := false, false
		for s, v := range c.services {
			if s == keep {
				continue
			}
			if !foundOldest || v < oldestTTL {
				oldest, oldestTTL, foundOldest = s, v, true
			}
			if len(c.operations[s]) > 0 && (!foundWithOps || v < withOpsTTL) {
				withOps, withOpsTTL, foundWithOps = s, v, true
			}
		}
		switch {
		case foundWithOps:
			c.removeOperations(withOps)
		case foundOldest:
			c.removeService(oldest)
		case len(c.operations[keep]) > 0:
			c.removeOperation(keep, soonestExpiring(c.operations[keep]))
		default:
			return
		}
	}
}

// sweepExpiredServices removes services whose expiry has passed, along with
// their operations. Must be called with cacheLock held.
func (c *CacheStore) sweepExpiredServices(now uint64) {
	for s, v := range c.services {
		if v <= now {
			c.removeService(s)
		}
	}
}

// sweepExpiredOperations removes the operations of service whose expiry has
// passed. Must be called with cacheLock held.
func (c *CacheStore) sweepExpiredOperations(service string, now uint64) {
	for o, v := range c.operations[service] {
		if v <= now {
			c.removeOperation(service, o)
		}
	}
}

// soonestExpiring returns the key with the earliest expiry in a non-empty map.
func soonestExpiring(entries map[string]uint64) string {
	found, name := false, ""
	var ttl uint64
	for k, v := range entries {
		if !found || v < ttl {
			found, name, ttl = true, k, v
		}
	}
	return name
}

func (c *CacheStore) release(n int) {
	c.size = max(c.size-n, 0)
}

// removeService must be called with cacheLock held.
func (c *CacheStore) removeService(service string) {
	if _, found := c.services[service]; found {
		delete(c.services, service)
		c.release(entrySize(service))
	}
	c.removeOperations(service)
}

// removeOperations must be called with cacheLock held.
func (c *CacheStore) removeOperations(service string) {
	ops, found := c.operations[service]
	if !found {
		return
	}
	n := entrySize(service)
	for o := range ops {
		n += entrySize(o)
	}
	delete(c.operations, service)
	c.release(n)
}

// removeOperation must be called with cacheLock held.
func (c *CacheStore) removeOperation(service, operation string) {
	if _, found := c.operations[service][operation]; found {
		delete(c.operations[service], operation)
		c.release(entrySize(operation))
	}
}

// GetOperations returns all operations for a specific service & spanKind traced by Jaeger
func (c *CacheStore) GetOperations(service string) ([]tracestore.Operation, error) {
	t := nowUnix()
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()
	operations := make([]string, 0, len(c.operations[service]))

	if v, ok := c.services[service]; ok {
		if v < t {
			// Expired, remove
			c.removeService(service)
			return []tracestore.Operation{}, nil // empty slice rather than nil
		}
		for o, e := range c.operations[service] {
			if e > t {
				operations = append(operations, o)
			} else {
				c.removeOperation(service, o)
			}
		}
	}

	slices.Sort(operations)

	// TODO: https://github.com/jaegertracing/jaeger/issues/1922
	// 	- return the operations with actual spanKind
	result := make([]tracestore.Operation, 0, len(operations))
	for _, op := range operations {
		result = append(result, tracestore.Operation{
			Name: op,
		})
	}
	return result, nil
}

// GetServices returns all services traced by Jaeger
func (c *CacheStore) GetServices() ([]string, error) {
	t := nowUnix()
	c.cacheLock.Lock()
	services := make([]string, 0, len(c.services))
	// Fetch the items
	for k, v := range c.services {
		if v > t {
			services = append(services, k)
		} else {
			// Service has expired, remove it along with its operations
			c.removeService(k)
		}
	}
	c.cacheLock.Unlock()

	slices.Sort(services)

	return services, nil
}
