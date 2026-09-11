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

func nhlFixture(t *testing.T, name string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", "nhl", name))
	require.NoError(t, err)
	return content
}

func nhlTestAdapter(t *testing.T, handler http.HandlerFunc) *NHLAdapter {
	t.Helper()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Result(), nil
	})}
	adapter := NewNHLAdapter(client)
	adapter.BaseURL = "https://nhl.test"
	adapter.Cache = NewCache[SportsSnapshot](0)
	adapter.Now = func() time.Time { return time.Date(2026, 1, 9, 23, 30, 0, 0, time.UTC) }
	return adapter
}

func TestNHLFixtureNormalizesEverySupportedState(t *testing.T) {
	adapter := nhlTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(nhlFixture(t, "state_games.json"))
	})
	games, err := adapter.fetchGames(context.Background(), "/score/now")
	require.NoError(t, err)
	require.Len(t, games, 15)
	want := []GameStatus{GameScheduled, GamePregame, GameLive, GameLive, GameLive, GameIntermission, GameLive, GameLive, GameFinal, GameFinal, GameFinal, GameDelayed, GamePostponed, GameSuspended, GameCancelled}
	for index := range want {
		assert.Equal(t, want[index], games[index].Status, "game %s", games[index].ID)
	}
	assert.Equal(t, "P1 12:34", games[2].StatusDetail)
	assert.Equal(t, "OT", games[6].PeriodLabel)
	assert.True(t, games[6].Overtime)
	assert.True(t, games[7].Shootout)
	assert.Equal(t, "FINAL", games[8].StatusDetail)
	assert.Equal(t, "FINAL/OT", games[9].StatusDetail)
	assert.Equal(t, "FINAL/SO", games[10].StatusDetail)
	assert.Equal(t, "FUT/DELAYED", games[11].ProviderState)
	assert.Equal(t, ProviderTeamID("10"), games[0].AwayTeam.ProviderID)
	assert.Equal(t, NewCanonicalTeamID(ProviderNHLWeb, LeagueNHL, "10"), games[0].AwayTeam.ID)
	assert.Equal(t, GameUnknown, normalizeNHLStatus("NEW_PROVIDER_STATE", "OK", false))
}

func TestNHLScheduleFindsFutureGameInsteadOfAssumingFirstEntry(t *testing.T) {
	adapter := nhlTestAdapter(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/score/now" {
			_, _ = writer.Write(nhlFixture(t, "score_empty.json"))
			return
		}
		assert.Equal(t, "/club-schedule-season/TOR/now", request.URL.Path)
		_, _ = writer.Write(nhlFixture(t, "schedule_future.json"))
	})
	snapshot, err := adapter.Schedule(context.Background(), SportsScheduleRequest{League: LeagueNHL, TeamID: "10", Timezone: "America/Toronto"})
	require.NoError(t, err)
	require.NotNil(t, snapshot.NextGame)
	assert.Equal(t, GameID("nhl-web:nhl:2001"), snapshot.NextGame.ID)
	assert.Empty(t, snapshot.Games)
}

func TestNHLScheduleUsesDeviceLocalDateAcrossUTCMidnight(t *testing.T) {
	payload := `{"games":[{"id":4001,"startTimeUTC":"2026-01-10T02:00:00Z","gameState":"FUT","gameScheduleState":"OK","awayTeam":{"id":10,"abbrev":"TOR"},"homeTeam":{"id":6,"abbrev":"BOS"}}]}`
	adapter := nhlTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(payload)) })
	snapshot, err := adapter.Schedule(context.Background(), SportsScheduleRequest{League: LeagueNHL, TeamID: "10", Timezone: "America/Toronto"})
	require.NoError(t, err)
	require.Len(t, snapshot.Games, 1)
	assert.Equal(t, "2026-01-09", snapshot.Games[0].ScheduledAt.In(time.FixedZone("EST", -5*60*60)).Format("2006-01-02"))
}

func TestNHLScheduleKeepsOrderedDoubleheaderGames(t *testing.T) {
	payload := `{"games":[
	{"id":4102,"startTimeUTC":"2026-01-10T03:00:00Z","gameState":"LIVE","awayTeam":{"id":10,"abbrev":"TOR","score":2},"homeTeam":{"id":6,"abbrev":"BOS","score":1}},
	{"id":4101,"startTimeUTC":"2026-01-09T20:00:00Z","gameState":"FINAL","awayTeam":{"id":6,"abbrev":"BOS","score":1},"homeTeam":{"id":10,"abbrev":"TOR","score":3}}]}`
	adapter := nhlTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(payload)) })
	snapshot, err := adapter.Schedule(context.Background(), SportsScheduleRequest{League: LeagueNHL, TeamID: "10", Timezone: "America/Toronto"})
	require.NoError(t, err)
	require.Len(t, snapshot.Games, 2)
	assert.Equal(t, "4101", snapshot.Games[0].ProviderGameID)
	assert.Equal(t, "4102", snapshot.Games[1].ProviderGameID)
	assert.Equal(t, nhlLiveFreshTTL, sportsCachePolicy(snapshot).FreshTTL)
}

func TestNHLLiveGamesAreOrderedAndDeduplicated(t *testing.T) {
	payload := `{"games":[
	{"id":5002,"startTimeUTC":"2026-01-10T03:00:00Z","gameState":"LIVE","awayTeam":{"id":10,"abbrev":"TOR"},"homeTeam":{"id":6,"abbrev":"BOS"}},
	{"id":5001,"startTimeUTC":"2026-01-10T02:00:00Z","gameState":"LIVE","awayTeam":{"id":8,"abbrev":"MTL"},"homeTeam":{"id":9,"abbrev":"OTT"}},
	{"id":5001,"startTimeUTC":"2026-01-10T02:00:00Z","gameState":"LIVE","awayTeam":{"id":8,"abbrev":"MTL"},"homeTeam":{"id":9,"abbrev":"OTT"}}]}`
	adapter := nhlTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(payload)) })
	snapshot, err := adapter.LiveGames(context.Background(), SportsLiveRequest{League: LeagueNHL, Timezone: "UTC"})
	require.NoError(t, err)
	require.Len(t, snapshot.Games, 2)
	assert.Equal(t, GameID("nhl-web:nhl:5001"), snapshot.Games[0].ID)
	assert.Equal(t, GameID("nhl-web:nhl:5002"), snapshot.Games[1].ID)
}

func TestNHLPartialAndMalformedResponsesAreSafe(t *testing.T) {
	t.Run("partial skips unusable games", func(t *testing.T) {
		adapter := nhlTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write(nhlFixture(t, "partial.json")) })
		games, err := adapter.fetchGames(context.Background(), "/score/now")
		require.NoError(t, err)
		require.Len(t, games, 1)
		assert.Equal(t, "NEW", games[0].HomeTeam.Abbreviation)
	})
	t.Run("malformed is sanitized", func(t *testing.T) {
		adapter := nhlTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write(nhlFixture(t, "malformed.json"))
		})
		_, err := adapter.fetchGames(context.Background(), "/score/now")
		var sanitized SanitizedError
		require.ErrorAs(t, err, &sanitized)
		assert.Equal(t, "sports_response_invalid", sanitized.Code)
	})
}

func TestNHLStaleFallbackIsBoundedAndAnnotated(t *testing.T) {
	var fail atomic.Bool
	adapter := nhlTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = writer.Write(nhlFixture(t, "state_games.json"))
	})
	request := SportsLiveRequest{League: LeagueNHL, Timezone: "UTC"}
	fresh, err := adapter.LiveGames(context.Background(), request)
	require.NoError(t, err)
	assert.False(t, fresh.Stale)
	key := "nhl:live:UTC"
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
	assert.False(t, fresh.Games[0].Stale, "stale annotation must not mutate the cached/fresh slice")
}

func TestNHLCachePolicyPrioritizesActiveDoubleheaderGame(t *testing.T) {
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	policy := sportsCachePolicy(SportsSnapshot{FreshAsOf: now, Games: []Game{
		{Status: GameFinal, ScheduledAt: now.Add(-4 * time.Hour)},
		{Status: GameLive, ScheduledAt: now.Add(-time.Hour)},
	}})
	assert.Equal(t, nhlLiveFreshTTL, policy.FreshTTL)
	assert.Equal(t, nhlLiveStaleTTL, policy.StaleTTL)
}

func TestNHLProviderRejectsOversizedPayload(t *testing.T) {
	adapter := nhlTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(make([]byte, nhlMaximumBodyBytes+1))
	})
	_, err := adapter.fetchGames(context.Background(), "/score/now")
	require.Error(t, err)
}

func TestNHLProviderFailureWithoutCacheReturnsSanitizedError(t *testing.T) {
	adapter := nhlTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusBadGateway) })
	_, err := adapter.LiveGames(context.Background(), SportsLiveRequest{League: LeagueNHL, Timezone: "UTC"})
	var sanitized SanitizedError
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "sports_provider_unavailable", sanitized.Code)
}

func TestNHLLiveCacheCoalescesConcurrentRequests(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{})
	adapter := nhlTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = writer.Write(nhlFixture(t, "state_games.json"))
	})
	request := SportsLiveRequest{League: LeagueNHL, Timezone: "UTC"}
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

func TestNHLTeamCatalogUsesStableNumericIdentities(t *testing.T) {
	adapter := NewNHLAdapter(nil)
	teams, err := adapter.Teams(context.Background(), LeagueNHL)
	require.NoError(t, err)
	require.Len(t, teams, 32)
	assert.Equal(t, CanonicalTeamID("nhl-web:nhl:24"), nhlTeams["24"].ID)
	assert.Equal(t, "ANA", nhlTeams["24"].Abbreviation)
	_, err = adapter.Schedule(context.Background(), SportsScheduleRequest{League: LeagueNHL, TeamID: "TOR", Timezone: "UTC"})
	var sanitized SanitizedError
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "sports_team_invalid", sanitized.Code)
}
