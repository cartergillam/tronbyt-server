package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func espnFixture(t *testing.T, name string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", "espn", name))
	require.NoError(t, err)
	return content
}

func espnTestAdapter(t *testing.T, handler http.HandlerFunc) *ESPNAdapter {
	t.Helper()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Result(), nil
	})}
	adapter := NewESPNAdapter(client)
	adapter.BaseURL = "https://espn.test/scoreboard"
	adapter.Cache = NewCache[SportsSnapshot](0)
	adapter.Now = func() time.Time { return time.Date(2026, 8, 6, 16, 0, 0, 0, time.UTC) }
	return adapter
}

func TestCFLFixtureNormalizesStatesRecordsAndHomeAway(t *testing.T) {
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(espnFixture(t, "cfl_states.json"))
	})
	games, err := adapter.fetchCFLGames(context.Background(), time.Now(), time.Now())
	require.NoError(t, err)
	require.Len(t, games, 14)
	want := []GameStatus{GameScheduled, GamePregame, GameLive, GameLive, GameLive, GameLive, GameIntermission, GameLive, GameFinal, GameFinal, GameDelayed, GamePostponed, GameCancelled, GameSuspended}
	for index := range want {
		assert.Equal(t, want[index], games[index].Status, "game %s", games[index].ID)
	}
	assert.Equal(t, ProviderTeamID("86"), games[0].AwayTeam.ProviderID)
	assert.Equal(t, ProviderTeamID("85"), games[0].HomeTeam.ProviderID)
	assert.Equal(t, "4-3", games[0].AwayRecord)
	assert.Equal(t, "5-2", games[0].HomeRecord)
	assert.Equal(t, "Q1 08:42", games[2].StatusDetail)
	assert.Equal(t, "HALFTIME", games[6].StatusDetail)
	assert.Equal(t, "OT", games[7].PeriodLabel)
	assert.True(t, games[7].Overtime)
	assert.Equal(t, "FINAL", games[8].StatusDetail)
	assert.Equal(t, "FINAL/OT", games[9].StatusDetail)
	assert.Equal(t, GameUnknown, normalizeESPNStatus("new", "NEW_STATE", "", "", "", 0))
}

func TestCFLScheduleFindsOrderedFutureGamesAndHonorsLimit(t *testing.T) {
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "200", request.URL.Query().Get("limit"))
		assert.Equal(t, "20260805-20270202", request.URL.Query().Get("dates"))
		_, _ = writer.Write(espnFixture(t, "cfl_future.json"))
	})
	snapshot, err := adapter.Schedule(context.Background(), SportsScheduleRequest{League: LeagueCFL, TeamID: "85", Timezone: "America/Toronto", Limit: 2})
	require.NoError(t, err)
	require.Len(t, snapshot.UpcomingGames, 2)
	assert.Equal(t, "2001", snapshot.UpcomingGames[0].ProviderGameID)
	assert.Equal(t, "2002", snapshot.UpcomingGames[1].ProviderGameID)
	require.NotNil(t, snapshot.NextGame)
	assert.Equal(t, "2001", snapshot.NextGame.ProviderGameID)
	assert.Empty(t, snapshot.Games)
}

func TestCFLOffDayReturnsEmptySnapshotWithBoundedPolicy(t *testing.T) {
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(`{"events":[]}`)) })
	snapshot, err := adapter.LiveGames(context.Background(), SportsLiveRequest{League: LeagueCFL, Timezone: "America/Toronto"})
	require.NoError(t, err)
	assert.Empty(t, snapshot.Games)
	assert.Nil(t, snapshot.NextGame)
	policy := espnCachePolicy(snapshot)
	assert.Equal(t, espnOffDayFreshTTL, policy.FreshTTL)
	assert.Equal(t, espnScheduleStaleTTL, policy.StaleTTL)
}

func TestCFLScheduleUsesDeviceLocalDateAcrossUTCMidnight(t *testing.T) {
	payload := `{"events":[{"id":"4001","date":"2026-08-07T02:00Z","status":{"type":{"state":"pre","name":"STATUS_SCHEDULED"}},"competitions":[{"competitors":[{"homeAway":"away","team":{"id":"85","abbreviation":"TOR"}},{"homeAway":"home","team":{"id":"86","abbreviation":"WPG"}}]}]}]}`
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(payload)) })
	adapter.Now = func() time.Time { return time.Date(2026, 8, 7, 0, 30, 0, 0, time.UTC) }
	snapshot, err := adapter.Schedule(context.Background(), SportsScheduleRequest{League: LeagueCFL, TeamID: "85", Timezone: "America/Toronto"})
	require.NoError(t, err)
	require.Len(t, snapshot.Games, 1)
	location, err := time.LoadLocation("America/Toronto")
	require.NoError(t, err)
	assert.Equal(t, "2026-08-06", snapshot.Games[0].ScheduledAt.In(location).Format("2006-01-02"))
}

func TestCFLLiveGamesAreOrderedDeduplicatedAndIncludeHalftime(t *testing.T) {
	payload := `{"events":[
	{"id":"5002","date":"2026-08-06T20:00Z","status":{"period":2,"type":{"state":"in","name":"STATUS_HALFTIME","shortDetail":"Halftime"}},"competitions":[{"competitors":[{"homeAway":"away","team":{"id":"85","abbreviation":"TOR"}},{"homeAway":"home","team":{"id":"86","abbreviation":"WPG"}}]}]},
	{"id":"5001","date":"2026-08-06T19:00Z","status":{"period":1,"displayClock":"10:00","type":{"state":"in","name":"STATUS_IN_PROGRESS"}},"competitions":[{"competitors":[{"homeAway":"away","team":{"id":"79","abbreviation":"BC"}},{"homeAway":"home","team":{"id":"80","abbreviation":"CGY"}}]}]},
	{"id":"5001","date":"2026-08-06T19:00Z","status":{"period":1,"type":{"state":"in","name":"STATUS_IN_PROGRESS"}},"competitions":[{"competitors":[{"homeAway":"away","team":{"id":"79","abbreviation":"BC"}},{"homeAway":"home","team":{"id":"80","abbreviation":"CGY"}}]}]}]}`
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(payload)) })
	snapshot, err := adapter.LiveGames(context.Background(), SportsLiveRequest{League: LeagueCFL, Timezone: "America/Toronto"})
	require.NoError(t, err)
	require.Len(t, snapshot.Games, 2)
	assert.Equal(t, "5001", snapshot.Games[0].ProviderGameID)
	assert.Equal(t, GameIntermission, snapshot.Games[1].Status)
}

func TestCFLPartialAndMalformedResponsesAreSafe(t *testing.T) {
	t.Run("partial", func(t *testing.T) {
		adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write(espnFixture(t, "cfl_partial.json"))
		})
		games, err := adapter.fetchCFLGames(context.Background(), time.Now(), time.Now())
		require.NoError(t, err)
		require.Len(t, games, 1)
		assert.Equal(t, "NEW", games[0].HomeTeam.Abbreviation)
		assert.Equal(t, "#333333", games[0].HomeTeam.PrimaryColor)
	})
	t.Run("malformed", func(t *testing.T) {
		adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write(espnFixture(t, "malformed.json"))
		})
		_, err := adapter.fetchCFLGames(context.Background(), time.Now(), time.Now())
		var sanitized SanitizedError
		require.ErrorAs(t, err, &sanitized)
		assert.Equal(t, "sports_response_invalid", sanitized.Code)
	})
}

func TestCFLStaleFallbackIsBoundedAndAnnotated(t *testing.T) {
	var fail atomic.Bool
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = writer.Write(espnFixture(t, "cfl_states.json"))
	})
	request := SportsLiveRequest{League: LeagueCFL, Timezone: "UTC"}
	fresh, err := adapter.LiveGames(context.Background(), request)
	require.NoError(t, err)
	key := "espn:cfl:live:UTC:20260806"
	adapter.Cache.mu.Lock()
	entry := adapter.Cache.entries[key]
	entry.freshUntil = time.Now().Add(-time.Second)
	entry.staleUntil = time.Now().Add(time.Minute)
	adapter.Cache.entries[key] = entry
	adapter.Cache.mu.Unlock()
	fail.Store(true)
	stale, err := adapter.LiveGames(context.Background(), request)
	require.NoError(t, err)
	assert.True(t, stale.Stale)
	require.NotEmpty(t, stale.Games)
	assert.True(t, stale.Games[0].Stale)
	assert.False(t, fresh.Games[0].Stale)
}

func TestCFLProviderFailureWithoutCacheAndOversizeAreSanitized(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusBadGateway) })
		_, err := adapter.LiveGames(context.Background(), SportsLiveRequest{League: LeagueCFL, Timezone: "UTC"})
		var sanitized SanitizedError
		require.ErrorAs(t, err, &sanitized)
		assert.Equal(t, "sports_provider_unavailable", sanitized.Code)
	})
	t.Run("oversize", func(t *testing.T) {
		adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write(make([]byte, espnMaximumBodyBytes+1))
		})
		_, err := adapter.fetchCFLGames(context.Background(), time.Now(), time.Now())
		require.Error(t, err)
	})
}

func TestCFLLiveCacheCoalescesConcurrentRequests(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	fixture := espnFixture(t, "cfl_states.json")
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = writer.Write(fixture)
	})
	request := SportsLiveRequest{League: LeagueCFL, Timezone: "UTC"}
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() { defer wait.Done(); _, _ = adapter.LiveGames(context.Background(), request) }()
	}
	<-started
	close(release)
	wait.Wait()
	assert.Equal(t, int32(1), calls.Load())
}

func TestCFLTeamCatalogAndSportsRegistryUseStableProviderIDs(t *testing.T) {
	adapter := NewESPNAdapter(nil)
	teams, err := adapter.Teams(context.Background(), LeagueCFL)
	require.NoError(t, err)
	require.Len(t, teams, 9)
	assert.Equal(t, CanonicalTeamID("espn-site:cfl:85"), cflTeams["85"].ID)
	assert.Equal(t, "TOR", cflTeams["85"].Abbreviation)
	registry := NewSportsRegistry(map[LeagueID]SportsProvider{LeagueCFL: adapter})
	_, err = registry.Teams(context.Background(), LeagueNHL)
	var sanitized SanitizedError
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "sports_league_unsupported", sanitized.Code)
	_, err = adapter.Schedule(context.Background(), SportsScheduleRequest{League: LeagueCFL, TeamID: "TOR", Timezone: "UTC"})
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "sports_team_invalid", sanitized.Code)
}
