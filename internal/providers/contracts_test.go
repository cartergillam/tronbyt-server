package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarketContractLimitsSymbols(t *testing.T) {
	assert.NoError(t, (MarketRequest{Symbols: []string{"AAPL", "TSX:RY"}}).Validate())
	assert.Error(t, (MarketRequest{Symbols: nil}).Validate())
	assert.Error(t, (MarketRequest{Symbols: []string{"AAPL", "aapl"}}).Validate())
	assert.Error(t, (MarketRequest{Symbols: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}}).Validate())
}

func TestProviderCacheSharesFreshDataAndPreservesStaleData(t *testing.T) {
	cache := NewCache[string](10 * time.Millisecond)
	calls := 0
	value, stale, err := cache.Get(context.Background(), "same-inputs", time.Millisecond, time.Minute, func(context.Context) (string, error) {
		calls++
		return "last-known-good", nil
	})
	require.NoError(t, err)
	assert.False(t, stale)
	assert.Equal(t, "last-known-good", value)

	value, stale, err = cache.Get(context.Background(), "same-inputs", time.Minute, time.Minute, func(context.Context) (string, error) {
		calls++
		return "unexpected", nil
	})
	require.NoError(t, err)
	assert.False(t, stale)
	assert.Equal(t, "last-known-good", value)
	assert.Equal(t, 1, calls)

	time.Sleep(2 * time.Millisecond)
	value, stale, err = cache.Get(context.Background(), "same-inputs", time.Minute, time.Minute, func(context.Context) (string, error) {
		calls++
		return "", errors.New("provider down")
	})
	require.NoError(t, err)
	assert.True(t, stale)
	assert.Equal(t, "last-known-good", value)
}

func TestProviderCacheCoalescesConcurrentIdenticalRequests(t *testing.T) {
	cache := NewCache[string](time.Millisecond)
	var calls atomic.Int32
	start := make(chan struct{})
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			value, _, err := cache.Get(context.Background(), "owner:same-inputs", time.Minute, time.Minute, func(context.Context) (string, error) {
				calls.Add(1)
				time.Sleep(10 * time.Millisecond)
				return "shared", nil
			})
			require.NoError(t, err)
			assert.Equal(t, "shared", value)
		}()
	}
	close(start)
	wait.Wait()
	assert.Equal(t, int32(1), calls.Load())
}

type fakeResolver struct{ secret string }

func (resolver fakeResolver) Resolve(context.Context, string, string, string) (string, error) {
	if resolver.secret == "" {
		return "", errors.New("missing")
	}
	return resolver.secret, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func jsonResponse(status int, value any) *http.Response {
	encoded, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(encoded))), Header: make(http.Header)}
}

func TestTwelveDataAdapterNormalizesCachesAndRedactsCredential(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		assert.Equal(t, "apikey test-secret", request.Header.Get("Authorization"))
		assert.NotContains(t, request.URL.String(), "test-secret")
		symbol := request.URL.Query().Get("symbol")
		return jsonResponse(http.StatusOK, map[string]any{
			"symbol": symbol, "name": symbol + " Inc", "close": "101.25", "change": "1.25",
			"percent_change": "1.25", "timestamp": 1786028400, "is_market_open": true,
		}), nil
	})}
	adapter := NewTwelveDataAdapter(fakeResolver{secret: "test-secret"}, client)
	request := MarketRequest{Symbols: []string{"shop:tsx"}, CredentialID: "market-primary", ScopeType: "server_owner", ScopeID: "owner"}
	first, err := adapter.Quotes(t.Context(), request)
	require.NoError(t, err)
	second, err := adapter.Quotes(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, "SHOP:TSX", first[0].Symbol)
	assert.Equal(t, first, second)
	assert.Equal(t, int32(1), calls.Load())
}

func TestOpenWeatherAdapterMapsNoAlertsAndUsesStaleCache(t *testing.T) {
	var calls atomic.Int32
	fail := atomic.Bool{}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		assert.Equal(t, "test-secret", request.URL.Query().Get("appid"))
		if fail.Load() {
			return nil, errors.New("temporary outage")
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"timezone": "America/Toronto",
			"current":  map[string]any{"dt": 1786028400, "temp": 22.0, "feels_like": 23.0, "weather": []any{map[string]any{"description": "light rain"}}},
			"hourly":   []any{}, "daily": []any{map[string]any{"temp": map[string]any{"min": 17.0, "max": 25.0}, "pop": 0.4}}, "alerts": []any{},
		}), nil
	})}
	adapter := NewOpenWeatherAdapter(fakeResolver{secret: "test-secret"}, client)
	adapter.Cache = NewCache[WeatherSnapshot](0)
	request := WeatherRequest{Location: Location{Latitude: 43.07, Longitude: -79.95}, Units: "metric", CredentialID: "weather", ScopeType: "server_owner", ScopeID: "owner"}
	first, err := adapter.Weather(t.Context(), request)
	require.NoError(t, err)
	assert.Empty(t, first.Alerts, "no active alerts is normal")
	adapter.Cache.mu.Lock()
	entry := adapter.Cache.entries["server_owner:owner:43.0700,-79.9500:metric"]
	entry.freshUntil = time.Now().Add(-time.Second)
	adapter.Cache.entries["server_owner:owner:43.0700,-79.9500:metric"] = entry
	adapter.Cache.mu.Unlock()
	fail.Store(true)
	stale, err := adapter.Weather(t.Context(), request)
	require.NoError(t, err)
	assert.True(t, stale.Stale)
	assert.Equal(t, first.Current, stale.Current)
}
