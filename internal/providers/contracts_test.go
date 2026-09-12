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
	assert.NoError(t, (MarketRequest{Symbols: []string{"AAPL", "RY:TSX"}}).Validate())
	assert.Error(t, (MarketRequest{Symbols: nil}).Validate())
	assert.Error(t, (MarketRequest{Symbols: []string{"AAPL", "aapl"}}).Validate())
	assert.NoError(t, (MarketRequest{Symbols: []string{"1", "2", "3", "4", "5"}}).Validate())
	assert.Error(t, (MarketRequest{Symbols: []string{"1", "2", "3", "4", "5", "6"}}).Validate())
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
			"percent_change": "1.25", "timestamp": 1786028400, "last_quote_at": 1786028460, "is_market_open": true,
			"exchange": "NASDAQ", "mic_code": "XNAS", "currency": "USD",
		}), nil
	})}
	adapter := NewTwelveDataAdapter(fakeResolver{secret: "test-secret"}, client)
	request := MarketRequest{Symbols: []string{"shop:tsx", "AAPL"}, CredentialID: "market-primary", ScopeType: "server_owner", ScopeID: "owner"}
	first, err := adapter.Quotes(t.Context(), request)
	require.NoError(t, err)
	second, err := adapter.Quotes(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, first, 2)
	assert.Equal(t, "SHOP:TSX", first[0].Symbol)
	assert.Equal(t, "AAPL", first[1].Symbol)
	assert.Equal(t, "NASDAQ", first[0].Exchange)
	assert.Equal(t, "XNAS", first[0].MIC)
	assert.Equal(t, "USD", first[0].Currency)
	assert.Equal(t, MarketOpen, first[0].MarketStatus)
	assert.Equal(t, time.Unix(1786028460, 0).UTC(), first[0].ProviderUpdated)
	assert.False(t, first[0].Delayed)
	assert.Equal(t, first, second)
	assert.Equal(t, int32(2), calls.Load())
}

func TestTwelveDataAdapterReportsMissingCredentialWithoutRequestingProvider(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(http.StatusOK, map[string]any{}), nil
	})}
	adapter := NewTwelveDataAdapter(fakeResolver{}, client)
	_, err := adapter.Quotes(t.Context(), MarketRequest{Symbols: []string{"AAPL"}, CredentialID: "market-primary", ScopeType: "server_owner", ScopeID: "owner"})
	var sanitized SanitizedError
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "provider_credential_missing", sanitized.Code)
	assert.Equal(t, int32(0), calls.Load())
}

func TestTwelveDataAdapterUsesClosedMarketTTLAndExplicitEODStatus(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(http.StatusOK, map[string]any{
			"symbol": request.URL.Query().Get("symbol"), "name": "Shopify", "close": "156.32", "change": "-1.84",
			"percent_change": "-1.16", "timestamp": 1786028400, "is_market_open": false, "is_eod": true,
			"exchange": "Toronto Stock Exchange", "mic_code": "XTSE", "currency": "CAD",
		}), nil
	})}
	adapter := NewTwelveDataAdapter(fakeResolver{secret: "test-secret"}, client)
	request := MarketRequest{Symbols: []string{"shop:tsx"}, CredentialID: "market-primary", ScopeType: "server_owner", ScopeID: "owner"}
	quotes, err := adapter.Quotes(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, quotes, 1)
	assert.Equal(t, "SHOP:TSX", quotes[0].Symbol)
	assert.Equal(t, MarketClosed, quotes[0].MarketStatus)
	assert.True(t, quotes[0].Delayed)
	assert.Equal(t, "XTSE", quotes[0].MIC)

	key := "server_owner:owner:market-primary:SHOP:TSX"
	adapter.Cache.mu.Lock()
	entry := adapter.Cache.entries[key]
	adapter.Cache.mu.Unlock()
	assert.WithinDuration(t, time.Now().Add(marketClosedFreshTTL), entry.freshUntil, time.Second)
	assert.Equal(t, int32(1), calls.Load())
}

func TestTwelveDataAdapterMarksBoundedFallbackStaleWithoutMutatingCache(t *testing.T) {
	failed := atomic.Bool{}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if failed.Load() {
			return nil, errors.New("provider unavailable")
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"symbol": request.URL.Query().Get("symbol"), "name": "Apple", "close": "100", "change": "1",
			"percent_change": "1", "timestamp": 1786028400, "is_market_open": true,
		}), nil
	})}
	adapter := NewTwelveDataAdapter(fakeResolver{secret: "test-secret"}, client)
	request := MarketRequest{Symbols: []string{"AAPL"}, CredentialID: "market-primary", ScopeType: "server_owner", ScopeID: "owner"}
	first, err := adapter.Quotes(t.Context(), request)
	require.NoError(t, err)
	assert.False(t, first[0].Stale)

	key := "server_owner:owner:market-primary:AAPL"
	adapter.Cache.mu.Lock()
	entry := adapter.Cache.entries[key]
	entry.freshUntil = time.Now().Add(-time.Second)
	adapter.Cache.entries[key] = entry
	adapter.Cache.lastRun[key] = time.Time{}
	adapter.Cache.mu.Unlock()
	failed.Store(true)
	stale, err := adapter.Quotes(t.Context(), request)
	require.NoError(t, err)
	assert.True(t, stale[0].Stale)

	adapter.Cache.mu.Lock()
	entry = adapter.Cache.entries[key]
	entry.freshUntil = time.Now().Add(time.Minute)
	adapter.Cache.entries[key] = entry
	adapter.Cache.mu.Unlock()
	fresh, err := adapter.Quotes(t.Context(), request)
	require.NoError(t, err)
	assert.False(t, fresh[0].Stale)
}

func TestTwelveDataAdapterBacksOffAcrossWatchlistsAfterRateLimit(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(http.StatusTooManyRequests, map[string]any{}), nil
	})}
	adapter := NewTwelveDataAdapter(fakeResolver{secret: "test-secret"}, client)
	request := MarketRequest{Symbols: []string{"AAPL"}, CredentialID: "market-primary", ScopeType: "server_owner", ScopeID: "owner"}
	_, err := adapter.Quotes(t.Context(), request)
	var sanitized SanitizedError
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "provider_rate_limited", sanitized.Code)

	_, err = adapter.Quotes(t.Context(), MarketRequest{Symbols: []string{"MSFT"}, CredentialID: "market-primary", ScopeType: "server_owner", ScopeID: "owner"})
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "provider_rate_limited", sanitized.Code)
	assert.Equal(t, int32(1), calls.Load(), "a second watchlist must honor the credential-level backoff")
}

func TestTwelveDataAdapterMapsJSONErrorCodesWithoutLeakingProviderMessages(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		want string
	}{
		{name: "rate limit", code: http.StatusTooManyRequests, want: "provider_rate_limited"},
		{name: "invalid credential", code: http.StatusUnauthorized, want: "provider_credential_invalid"},
		{name: "plan entitlement", code: http.StatusForbidden, want: "provider_entitlement_required"},
		{name: "invalid symbol", code: http.StatusBadRequest, want: "invalid_symbol"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK, map[string]any{"status": "error", "code": test.code, "message": "secret provider detail"}), nil
			})}
			adapter := NewTwelveDataAdapter(fakeResolver{secret: "test-secret"}, client)
			_, err := adapter.Quotes(t.Context(), MarketRequest{Symbols: []string{"AAPL"}, CredentialID: "market-primary", ScopeType: "server_owner", ScopeID: "owner"})
			var sanitized SanitizedError
			require.ErrorAs(t, err, &sanitized)
			assert.Equal(t, test.want, sanitized.Code)
			assert.NotContains(t, sanitized.Message, "secret provider detail")
		})
	}
}

func TestTwelveDataAdapterRecognizesPlanRestrictionReportedAsCode401(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, map[string]any{
			"status": "error", "code": http.StatusUnauthorized,
			"message": "Your current plan does not have access to this exchange",
		}), nil
	})}
	adapter := NewTwelveDataAdapter(fakeResolver{secret: "test-secret"}, client)
	_, err := adapter.Quotes(t.Context(), MarketRequest{Symbols: []string{"SHOP:TSX"}, CredentialID: "market-primary", ScopeType: "server_owner", ScopeID: "owner"})
	var sanitized SanitizedError
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "provider_entitlement_required", sanitized.Code)
	assert.NotContains(t, sanitized.Message, "exchange")
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
