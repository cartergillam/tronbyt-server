package providers

import (
	"context"
	"sync"
	"time"
)

type cacheEntry[T any] struct {
	value      T
	freshUntil time.Time
	staleUntil time.Time
}

// Cache is intended to be shared by server-side adapters. Keys must include
// every provider input (symbols/location/units) but never credential values.
// Device identity is intentionally absent when request inputs are identical.
type Cache[T any] struct {
	mu              sync.Mutex
	entries         map[string]cacheEntry[T]
	lastRun         map[string]time.Time
	minimumInterval time.Duration
}

func NewCache[T any](minimumInterval time.Duration) *Cache[T] {
	return &Cache[T]{entries: map[string]cacheEntry[T]{}, lastRun: map[string]time.Time{}, minimumInterval: minimumInterval}
}

func (cache *Cache[T]) Get(ctx context.Context, key string, ttl, staleTTL time.Duration, fetch func(context.Context) (T, error)) (T, bool, error) {
	cache.mu.Lock()
	now := time.Now()
	entry, found := cache.entries[key]
	if found && now.Before(entry.freshUntil) {
		cache.mu.Unlock()
		return entry.value, false, nil
	}
	if last := cache.lastRun[key]; !last.IsZero() && now.Sub(last) < cache.minimumInterval {
		cache.mu.Unlock()
		if found && now.Before(entry.staleUntil) {
			return entry.value, true, nil
		}
		var zero T
		return zero, false, TemporarilyUnavailable()
	}
	cache.lastRun[key] = now
	cache.mu.Unlock()

	value, err := fetch(ctx)
	if err != nil {
		if found && now.Before(entry.staleUntil) {
			return entry.value, true, nil
		}
		var zero T
		return zero, false, err
	}
	cache.mu.Lock()
	cache.entries[key] = cacheEntry[T]{value: value, freshUntil: now.Add(ttl), staleUntil: now.Add(ttl + staleTTL)}
	cache.mu.Unlock()
	return value, false, nil
}
