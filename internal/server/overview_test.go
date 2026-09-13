package server

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"tronbyt-server/internal/data"
	"tronbyt-server/internal/providers"
)

type overviewOfflineTransport struct{ calls atomic.Int32 }

func (t *overviewOfflineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	body := `{"season":20252026,"games":[{"id":1,"gameType":2,"startTimeUTC":"2026-01-11T00:00:00Z","gameState":"FUT","awayTeam":{"id":10},"homeTeam":{"id":9}}]}`
	if strings.Contains(r.URL.Path, "standings") {
		body = `{"standings":[{"date":"2026-01-10","seasonId":20252026,"teamAbbrev":{"default":"TOR"},"wins":32,"losses":18,"otLosses":6}]}`
	}
	if strings.Contains(r.URL.Path, "20242025") {
		body = `{"games":[]}`
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
}
func TestOverviewInjectionSharesCacheAndDeviceTimezone(t *testing.T) {
	transport := &overviewOfflineTransport{}
	adapter := providers.NewNHLAdapter(&http.Client{Transport: transport})
	adapter.Now = func() time.Time { return time.Date(2026, 1, 10, 18, 0, 0, 0, time.UTC) }
	s := &Server{SportsProvider: providers.NewSportsRegistry(map[providers.LeagueID]providers.SportsProvider{providers.LeagueNHL: adapter})}
	for i := 0; i < 40; i++ {
		tz, want := "America/Toronto", "7:00PM"
		if i%2 == 0 {
			tz, want = "America/Vancouver", "4:00PM"
		}
		device := &data.Device{ID: tz, Location: data.DeviceLocation{Lat: 43, Lng: -79, Timezone: tz}}
		config := map[string]any{"teamid": "10"}
		s.injectManagedProviderData(t.Context(), device, &data.App{Name: "nhl-overview"}, config)
		var r providers.SportsOverview
		require.NoError(t, json.Unmarshal([]byte(config["$overview_data"].(string)), &r))
		require.Equal(t, want, r.NextGame.TimeLabel)
		require.NotContains(t, config["$overview_data"], "https://")
	}
	require.Equal(t, int32(3), transport.calls.Load()) // One table, one schedule, one previous-season fallback.
}
func TestOverviewSchemasMatchFriendlyCanonicalTeams(t *testing.T) {
	registry := providers.NewSportsRegistry(map[providers.LeagueID]providers.SportsProvider{providers.LeagueNHL: providers.NewNHLAdapter(nil), providers.LeagueNBA: providers.NewESPNAdapter(nil), providers.LeagueNFL: providers.NewESPNAdapter(nil)})
	for _, league := range []providers.LeagueID{providers.LeagueNHL, providers.LeagueNBA, providers.LeagueNFL} {
		raw, e := os.ReadFile(filepath.Join("testdata", "overview", string(league)+"-schema.json"))
		require.NoError(t, e)
		schema, e := normalizeSchemaBytes(raw)
		require.NoError(t, e)
		require.Len(t, schema.Fields, 2)
		field := schema.Fields[0]
		require.Equal(t, "teamid", field.Key)
		require.Equal(t, "Favorite Team", field.Title)
		require.Equal(t, "enum", field.Type)
		teams, e := registry.Teams(t.Context(), league)
		require.NoError(t, e)
		require.Len(t, field.Options, len(teams))
		names := map[string]string{}
		for _, team := range teams {
			names[string(team.ProviderID)] = team.DisplayName
		}
		for _, option := range field.Options {
			require.Equal(t, names[option.Value], option.Label)
		}
	}
}
