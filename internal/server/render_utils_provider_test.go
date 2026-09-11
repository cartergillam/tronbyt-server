package server

import (
	"context"
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
	return []providers.MarketQuote{{Symbol: request.Symbols[0], Price: 42, QuoteTimestamp: time.Unix(1, 0)}}, nil
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
