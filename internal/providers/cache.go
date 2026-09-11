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

type inflightCall[T any] struct {
	done  chan struct{}
	value T
	stale bool
	err   error
}

// Cache is intended to be shared by server-side adapters. Keys must include
// every provider input (symbols/location/units) but never credential values.
// Device identity is intentionally absent when request inputs are identical.
type Cache[T any] struct {
	mu              sync.Mutex
	entries         map[string]cacheEntry[T]
	lastRun         map[string]time.Time
	inflight        map[string]*inflightCall[T]
	minimumInterval time.Duration
}

func NewCache[T any](minimumInterval time.Duration) *Cache[T] {
	return &Cache[T]{entries: map[string]cacheEntry[T]{}, lastRun: map[string]time.Time{}, inflight: map[string]*inflightCall[T]{}, minimumInterval: minimumInterval}
}

func (cache *Cache[T]) Get(ctx context.Context, key string, ttl, staleTTL time.Duration, fetch func(context.Context) (T, error)) (T, bool, error) {
	cache.mu.Lock()
	now := time.Now()
	entry, found := cache.entries[key]
	if found && now.Before(entry.freshUntil) {
		cache.mu.Unlock()
		return entry.value, false, nil
	}
	if call := cache.inflight[key]; call != nil {
		cache.mu.Unlock()
		select {
		case <-ctx.Done():
			var zero T
			return zero, false, ctx.Err()
		case <-call.done:
			return call.value, call.stale, call.err
		}
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
	call := &inflightCall[T]{done: make(chan struct{})}
	cache.inflight[key] = call
	cache.mu.Unlock()

	value, err := fetch(ctx)
	if err != nil {
		if found && now.Before(entry.staleUntil) {
			cache.finish(key, call, entry.value, true, nil)
			return entry.value, true, nil
		}
		var zero T
		cache.finish(key, call, zero, false, err)
		return zero, false, err
	}
	cache.mu.Lock()
	cache.entries[key] = cacheEntry[T]{value: value, freshUntil: now.Add(ttl), staleUntil: now.Add(ttl + staleTTL)}
	call.value = value
	close(call.done)
	delete(cache.inflight, key)
	cache.mu.Unlock()
	return value, false, nil
}

func (cache *Cache[T]) finish(key string, call *inflightCall[T], value T, stale bool, err error) {
	cache.mu.Lock()
	call.value, call.stale, call.err = value, stale, err
	close(call.done)
	delete(cache.inflight, key)
	cache.mu.Unlock()
}
