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

type recordingSportsProvider struct {
	scheduleRequests []providers.SportsScheduleRequest
	liveRequests     []providers.SportsLiveRequest
	err              error
}

func (provider *recordingSportsProvider) Teams(context.Context, providers.LeagueID) ([]providers.Team, error) {
	return nil, provider.err
}

func (provider *recordingSportsProvider) Schedule(_ context.Context, request providers.SportsScheduleRequest) (providers.SportsSnapshot, error) {
	provider.scheduleRequests = append(provider.scheduleRequests, request)
	return providers.SportsSnapshot{League: providers.LeagueNHL, Provider: providers.ProviderNHLWeb, DeviceTimezone: request.Timezone, Games: []providers.Game{}}, provider.err
}

func (provider *recordingSportsProvider) LiveGames(_ context.Context, request providers.SportsLiveRequest) (providers.SportsSnapshot, error) {
	provider.liveRequests = append(provider.liveRequests, request)
	return providers.SportsSnapshot{League: providers.LeagueNHL, Provider: providers.ProviderNHLWeb, DeviceTimezone: request.Timezone, Games: []providers.Game{}}, provider.err
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

func TestManagedNHLDataUsesStableTeamIDAndDeviceTimezoneWithoutCredential(t *testing.T) {
	provider := &recordingSportsProvider{}
	server := &Server{SportsProvider: provider}
	device := &data.Device{ID: "display-a", Username: "owner", Timezone: stringPointer("America/Toronto")}
	config := map[string]any{"mode": "favorite", "teamid": "10"}
	server.injectManagedProviderData(context.Background(), device, &data.App{Name: "nhl-live"}, config)
	require.Len(t, provider.scheduleRequests, 1)
	assert.Equal(t, providers.ProviderTeamID("10"), provider.scheduleRequests[0].TeamID)
	assert.Equal(t, "America/Toronto", provider.scheduleRequests[0].Timezone)
	assert.Contains(t, config, "$provider_data")
	assert.NotContains(t, config, "credential_id")
}

func TestManagedNHLLiveAndLegacyRandomModesUseAllLiveGames(t *testing.T) {
	for name, config := range map[string]map[string]any{
		"explicit": {"mode": "all_live", "teamid": "10"},
		"legacy":   {"teamid": "0"},
	} {
		t.Run(name, func(t *testing.T) {
			provider := &recordingSportsProvider{}
			server := &Server{SportsProvider: provider}
			server.injectManagedProviderData(context.Background(), &data.Device{Timezone: stringPointer("America/Vancouver")}, &data.App{Name: "nhl-live"}, config)
			require.Len(t, provider.liveRequests, 1)
			assert.Equal(t, "America/Vancouver", provider.liveRequests[0].Timezone)
			assert.Empty(t, provider.scheduleRequests)
		})
	}
}

func TestManagedNHLProviderErrorsAreSanitized(t *testing.T) {
	provider := &recordingSportsProvider{err: providers.SanitizedError{Code: "sports_response_invalid", Message: "NHL data could not be read", Retryable: true}}
	config := map[string]any{"teamid": 10.0}
	(&Server{SportsProvider: provider}).injectManagedProviderData(context.Background(), &data.Device{}, &data.App{Name: "nhl-live"}, config)
	assert.NotContains(t, config, "$provider_data")
	assert.JSONEq(t, `{"code":"sports_response_invalid","message":"NHL data could not be read"}`, config["$provider_error"].(string))
}

func TestManagedCFLAutoPreservesFavoriteAndAllTeamsSemantics(t *testing.T) {
	t.Run("numeric auto follows favorite", func(t *testing.T) {
		provider := &recordingSportsProvider{}
		config := map[string]any{"scoreMode": "auto", "selectedTeam": "85", "upcomingGames": "3"}
		(&Server{SportsProvider: provider}).injectManagedProviderData(context.Background(), &data.Device{Timezone: stringPointer("America/Toronto")}, &data.App{Name: "cfl-scores"}, config)
		require.Len(t, provider.scheduleRequests, 1)
		assert.Equal(t, providers.LeagueCFL, provider.scheduleRequests[0].League)
		assert.Equal(t, providers.ProviderTeamID("85"), provider.scheduleRequests[0].TeamID)
		assert.Equal(t, 3, provider.scheduleRequests[0].Limit)
		assert.Equal(t, "America/Toronto", provider.scheduleRequests[0].Timezone)
		assert.Contains(t, config, "$sports_data")
		assert.NotContains(t, config, "$provider_data")
	})

	for name, config := range map[string]map[string]any{
		"all teams auto": {"scoreMode": "auto", "selectedTeam": "all"},
		"league mode":    {"scoreMode": "league", "selectedTeam": "85"},
	} {
		t.Run(name, func(t *testing.T) {
			provider := &recordingSportsProvider{}
			(&Server{SportsProvider: provider}).injectManagedProviderData(context.Background(), &data.Device{Timezone: stringPointer("America/Vancouver")}, &data.App{Name: "cfl-scores"}, config)
			require.Len(t, provider.liveRequests, 1)
			assert.Equal(t, providers.LeagueCFL, provider.liveRequests[0].League)
			assert.Equal(t, "America/Vancouver", provider.liveRequests[0].Timezone)
			assert.Contains(t, config, "$sports_data")
		})
	}
}

func TestManagedCFLErrorsRemainSanitized(t *testing.T) {
	provider := &recordingSportsProvider{err: providers.SanitizedError{Code: "sports_response_invalid", Message: "CFL data could not be read", Retryable: true}}
	config := map[string]any{"scoreMode": "favorite", "selectedTeam": 85.0}
	(&Server{SportsProvider: provider}).injectManagedProviderData(context.Background(), &data.Device{}, &data.App{Name: "cfl-scores"}, config)
	assert.NotContains(t, config, "$sports_data")
	assert.JSONEq(t, `{"code":"sports_response_invalid","message":"CFL data could not be read"}`, config["$provider_error"].(string))
}

func TestManagedNBAModesUseStableTeamIDAndDeviceTimezone(t *testing.T) {
	provider := &recordingSportsProvider{}
	server := &Server{SportsProvider: provider}
	device := &data.Device{Timezone: stringPointer("America/Toronto")}
	config := map[string]any{"mode": "favorite", "teamid": "28"}
	server.injectManagedProviderData(context.Background(), device, &data.App{Name: "nba-live"}, config)
	require.Len(t, provider.scheduleRequests, 1)
	assert.Equal(t, providers.LeagueNBA, provider.scheduleRequests[0].League)
	assert.Equal(t, providers.ProviderTeamID("28"), provider.scheduleRequests[0].TeamID)
	assert.Equal(t, "America/Toronto", provider.scheduleRequests[0].Timezone)
	assert.Contains(t, config, "$sports_data")

	provider = &recordingSportsProvider{}
	config = map[string]any{"mode": "all_live", "teamid": "28"}
	(&Server{SportsProvider: provider}).injectManagedProviderData(context.Background(), device, &data.App{Name: "nba-live"}, config)
	require.Len(t, provider.liveRequests, 1)
	assert.Equal(t, providers.LeagueNBA, provider.liveRequests[0].League)
	assert.Empty(t, provider.scheduleRequests)
}

func TestManagedNBAErrorsRemainSanitized(t *testing.T) {
	provider := &recordingSportsProvider{err: providers.SanitizedError{Code: "sports_response_invalid", Message: "NBA data could not be read", Retryable: true}}
	config := map[string]any{"mode": "favorite", "teamid": "28"}
	(&Server{SportsProvider: provider}).injectManagedProviderData(context.Background(), &data.Device{}, &data.App{Name: "nba-live"}, config)
	assert.NotContains(t, config, "$sports_data")
	assert.JSONEq(t, `{"code":"sports_response_invalid","message":"NBA data could not be read"}`, config["$provider_error"].(string))
}

func stringPointer(value string) *string { return &value }
