package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"tronbyt-server/internal/data"
	"tronbyt-server/internal/providers"
)

type recordingMarketProvider struct{ requests []providers.MarketRequest }

func (provider *recordingMarketProvider) Quotes(_ context.Context, request providers.MarketRequest) ([]providers.MarketQuote, error) {
	provider.requests = append(provider.requests, request)
	return []providers.MarketQuote{{
		Symbol: request.Symbols[0], Price: 42, QuoteTimestamp: time.Unix(1, 0), ProviderUpdated: time.Unix(1, 0),
		Exchange: "NASDAQ", MIC: "XNAS", Currency: "USD", MarketStatus: providers.MarketOpen,
	}}, nil
}

type recordingWeatherProvider struct{ requests []providers.WeatherRequest }

func (provider *recordingWeatherProvider) Weather(_ context.Context, request providers.WeatherRequest) (providers.WeatherSnapshot, error) {
	provider.requests = append(provider.requests, request)
	return providers.WeatherSnapshot{Location: request.Location, ProviderUpdated: time.Unix(1, 0)}, nil
}

func TestManagedMarketDataUsesOwnerScopeAndKeepsDevicesOutOfSharedInputs(t *testing.T) {
	provider := &recordingMarketProvider{}
	server := &Server{MarketProvider: provider}
	app := &data.App{Name: "market-watch"}
	for _, device := range []*data.Device{
		{ID: "display-a", Username: "owner", Timezone: stringPointer("America/Toronto")},
		{ID: "display-b", Username: "owner", Timezone: stringPointer("America/Vancouver")},
	} {
		config := map[string]any{"credential_id": "markets", "symbols": " shop:tsx, AAPL, AAPL "}
		server.injectManagedProviderData(context.Background(), device, app, config)
		require.Contains(t, config, "$provider_data")
		assert.NotContains(t, config, "secret")
	}
	require.Len(t, provider.requests, 2)
	assert.Equal(t, []string{"SHOP:TSX", "AAPL"}, provider.requests[0].Symbols)
	assert.Equal(t, "server_owner", provider.requests[0].ScopeType)
	assert.Equal(t, "owner", provider.requests[0].ScopeID)
	assert.NotEqual(t, "display-a", provider.requests[0].CredentialID)
}

func TestManagedMarketDataPreservesQuoteMetadataAndReportsInvalidSymbols(t *testing.T) {
	provider := &recordingMarketProvider{}
	server := &Server{MarketProvider: provider}
	device := &data.Device{ID: "display-a", Username: "owner"}
	config := map[string]any{"credential_id": "markets", "symbols": " shop:tsx, AAPL "}
	server.injectManagedProviderData(context.Background(), device, &data.App{Name: "market-watch"}, config)
	require.Len(t, provider.requests, 1)
	assert.Equal(t, []string{"SHOP:TSX", "AAPL"}, provider.requests[0].Symbols)

	var quotes []providers.MarketQuote
	require.NoError(t, json.Unmarshal([]byte(config["$provider_data"].(string)), &quotes))
	require.Len(t, quotes, 1)
	assert.Equal(t, "NASDAQ", quotes[0].Exchange)
	assert.Equal(t, "XNAS", quotes[0].MIC)
	assert.Equal(t, "USD", quotes[0].Currency)
	assert.Equal(t, providers.MarketOpen, quotes[0].MarketStatus)
	assert.False(t, quotes[0].ProviderUpdated.IsZero())
	assert.NotContains(t, config["$provider_data"].(string), "secret")

	config = map[string]any{"credential_id": "markets", "symbols": "not a symbol"}
	server.injectManagedProviderData(context.Background(), device, &data.App{Name: "market-watch"}, config)
	// The recording provider is intentionally permissive. Production validation
	// happens in the provider adapter; this verifies raw input is not silently
	// dropped before it can produce its sanitized invalid_symbol response.
	require.Len(t, provider.requests, 2)
	assert.Equal(t, []string{"not a symbol"}, provider.requests[1].Symbols)
}

func TestManagedWeatherDataInheritsDeviceLocationAndIsolatesCustomOverride(t *testing.T) {
	provider := &recordingWeatherProvider{}
	server := &Server{WeatherProvider: provider}
	app := &data.App{Name: "local-weather"}
	device := &data.Device{
		ID:       "display-a",
		Username: "owner",
		Timezone: stringPointer("America/Toronto"),
		Location: data.DeviceLocation{Lat: 43.65, Lng: -79.38, Description: "Toronto"},
	}
	inherited := map[string]any{"credential_id": "weather", "units": "metric"}
	custom := map[string]any{
		"credential_id":   "weather",
		"units":           "imperial",
		"custom_location": `{"lat":49.28,"lng":-123.12,"description":"Vancouver","timezone":"America/Vancouver"}`,
	}
	server.injectManagedProviderData(context.Background(), device, app, inherited)
	server.injectManagedProviderData(context.Background(), device, app, custom)
	require.Len(t, provider.requests, 2)
	assert.Equal(t, 43.65, provider.requests[0].Location.Latitude)
	assert.Equal(t, "America/Toronto", provider.requests[0].Location.Timezone)
	assert.Equal(t, 49.28, provider.requests[1].Location.Latitude)
	assert.Equal(t, "America/Vancouver", provider.requests[1].Location.Timezone)
	assert.Equal(t, "metric", provider.requests[0].Units)
	assert.Equal(t, "imperial", provider.requests[1].Units)
}

func stringPointer(value string) *string { return &value }
