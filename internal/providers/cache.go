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

// CachePolicy lets a provider choose freshness and stale-fallback bounds from
// the response it just received. Non-positive freshness uses the one-minute
// cache default; a negative stale lifetime is treated as zero.
type CachePolicy struct {
	FreshTTL time.Duration
	StaleTTL time.Duration
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
	return cache.GetWithTTL(ctx, key, staleTTL, func(ctx context.Context) (T, time.Duration, error) {
		value, err := fetch(ctx)
		return value, ttl, err
	})
}

// GetWithTTL is Get with a provider-selected freshness lifetime. This lets a
// response such as a closed-market quote remain fresh longer without creating
// a separate cache or bypassing request coalescing.
func (cache *Cache[T]) GetWithTTL(ctx context.Context, key string, staleTTL time.Duration, fetch func(context.Context) (T, time.Duration, error)) (T, bool, error) {
	return cache.GetWithPolicy(ctx, key, func(ctx context.Context) (T, CachePolicy, error) {
		value, ttl, err := fetch(ctx)
		return value, CachePolicy{FreshTTL: ttl, StaleTTL: staleTTL}, err
	})
}

// GetWithPolicy preserves the cache's request coalescing and minimum-interval
// behavior while allowing state-aware providers to bound live and schedule
// fallbacks differently.
func (cache *Cache[T]) GetWithPolicy(ctx context.Context, key string, fetch func(context.Context) (T, CachePolicy, error)) (T, bool, error) {
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

	value, policy, err := fetch(ctx)
	if err != nil {
		if found && time.Now().Before(entry.staleUntil) {
			cache.finish(key, call, entry.value, true, nil)
			return entry.value, true, nil
		}
		var zero T
		cache.finish(key, call, zero, false, err)
		return zero, false, err
	}
	if policy.FreshTTL <= 0 {
		policy.FreshTTL = time.Minute
	}
	if policy.StaleTTL < 0 {
		policy.StaleTTL = 0
	}
	storedAt := time.Now()
	cache.mu.Lock()
	cache.entries[key] = cacheEntry[T]{
		value: value, freshUntil: storedAt.Add(policy.FreshTTL),
		staleUntil: storedAt.Add(policy.FreshTTL + policy.StaleTTL),
	}
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
