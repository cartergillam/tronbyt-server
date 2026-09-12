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
	adapter.NBABaseURL = "https://espn.test/scoreboard"
	adapter.NFLBaseURL = "https://espn.test/scoreboard"
	adapter.Cache = NewCache[SportsSnapshot](0)
	adapter.Now = func() time.Time { return time.Date(2026, 8, 6, 16, 0, 0, 0, time.UTC) }
	return adapter
}

func mlbTestAdapter(t *testing.T, handler http.HandlerFunc) *MLBAdapter {
	t.Helper()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Result(), nil
	})}
	adapter := NewMLBAdapter(client)
	adapter.BaseURL = "https://mlb.test/schedule"
	adapter.Cache = NewCache[SportsSnapshot](0)
	adapter.Now = func() time.Time { return time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC) }
	return adapter
}

func TestMLBFixtureNormalizesBaseballContextAndStates(t *testing.T) {
	adapter := mlbTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(espnFixture(t, "mlb_states.json"))
	})
	games, err := adapter.fetchGames(context.Background(), "141", time.Now(), time.Now(), false)
	require.NoError(t, err)
	require.Len(t, games, 14)
	byID := map[string]Game{}
	for _, game := range games {
		byID[game.ProviderGameID] = game
	}
	for id, status := range map[string]GameStatus{"1001": GameScheduled, "1002": GamePregame, "1003": GameLive, "1004": GameLive, "1005": GameIntermission, "1006": GameLive, "1007": GameFinal, "1008": GamePostponed, "1009": GameSuspended, "1010": GameDelayed, "1011": GameCancelled, "1012": GameUnknown, "1013": GameFinal, "1014": GameScheduled} {
		assert.Equal(t, status, byID[id].Status, "game %s", id)
	}
	assert.Equal(t, LeagueMLB, byID["1001"].League)
	assert.Equal(t, CanonicalTeamID("mlb-stats-api:mlb:141"), byID["1001"].AwayTeam.ID)
	assert.Equal(t, "TOR", byID["1001"].AwayTeam.Abbreviation)
	assert.Equal(t, "52-38", byID["1001"].AwayRecord)
	assert.Equal(t, 3, byID["1003"].Inning)
	assert.Equal(t, "top", byID["1003"].InningHalf)
	assert.Equal(t, 2, byID["1003"].Outs)
	assert.True(t, byID["1003"].RunnerOnFirst)
	assert.True(t, byID["1003"].RunnerOnThird)
	assert.Equal(t, "bottom", byID["1004"].InningHalf)
	assert.Equal(t, "middle", byID["1005"].InningHalf)
	assert.Equal(t, "MIDDLE 3", byID["1005"].StatusDetail)
	assert.True(t, byID["1006"].Overtime)
	assert.Equal(t, 11, byID["1006"].Inning)
	assert.Equal(t, "FINAL", byID["1007"].StatusDetail)
}

func TestMLBSchedulePreservesLocalWindowDateBoundaryAndDoubleheaderOrder(t *testing.T) {
	adapter := mlbTestAdapter(t, func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "1", request.URL.Query().Get("sportId"))
		assert.Equal(t, "141", request.URL.Query().Get("teamId"))
		assert.Equal(t, "linescore", request.URL.Query().Get("hydrate"))
		assert.Equal(t, "2026-07-30", request.URL.Query().Get("startDate"))
		assert.Equal(t, "2026-08-02", request.URL.Query().Get("endDate"))
		_, _ = writer.Write(espnFixture(t, "mlb_states.json"))
	})
	adapter.Now = func() time.Time { return time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC) }
	snapshot, err := adapter.Schedule(context.Background(), SportsScheduleRequest{League: LeagueMLB, TeamID: "141", Timezone: "America/Toronto"})
	require.NoError(t, err)
	require.NotEmpty(t, snapshot.Games)
	assert.Equal(t, LeagueMLB, snapshot.League)
	assert.Equal(t, "2026-07-31T19:09:00-04:00", snapshot.Games[0].ScheduledLocal)
	assert.Equal(t, "1003", snapshot.Games[0].ProviderGameID, "live game outranks preview/final like the incumbent app")
	foundG2 := false
	for _, game := range snapshot.Games {
		if game.ProviderGameID == "1014" {
			foundG2 = true
			assert.NotEmpty(t, game.GameLabel)
			assert.Equal(t, 2, game.GameNumber)
			assert.True(t, game.Doubleheader)
		}
	}
	assert.True(t, foundG2)
}

func TestMLBOffDayFutureMalformedAndStableIdentity(t *testing.T) {
	t.Run("off day", func(t *testing.T) {
		adapter := mlbTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(`{"dates":[]}`)) })
		snapshot, err := adapter.LiveGames(context.Background(), SportsLiveRequest{League: LeagueMLB, Timezone: "America/Toronto"})
		require.NoError(t, err)
		assert.Empty(t, snapshot.Games)
		policy := espnCachePolicy(snapshot)
		assert.Equal(t, 20*time.Minute, policy.FreshTTL)
		assert.Equal(t, 45*time.Minute, policy.StaleTTL)
	})
	t.Run("future", func(t *testing.T) {
		payload := `{"dates":[{"games":[{"gamePk":2001,"gameDate":"2026-08-02T23:07:00Z","gameType":"R","gameNumber":1,"status":{"abstractGameState":"Preview","detailedState":"Scheduled"},"teams":{"away":{"team":{"id":141}},"home":{"team":{"id":138}}}}]}]}`
		adapter := mlbTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(payload)) })
		snapshot, err := adapter.Schedule(context.Background(), SportsScheduleRequest{League: LeagueMLB, TeamID: "141", Timezone: "America/Toronto"})
		require.NoError(t, err)
		assert.Empty(t, snapshot.Games)
		require.NotNil(t, snapshot.NextGame)
		assert.Equal(t, "2001", snapshot.NextGame.ProviderGameID)
	})
	t.Run("malformed and partial", func(t *testing.T) {
		adapter := mlbTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write(espnFixture(t, "malformed.json"))
		})
		_, err := adapter.fetchGames(context.Background(), "141", time.Now(), time.Now(), false)
		var sanitized SanitizedError
		require.ErrorAs(t, err, &sanitized)
		assert.Equal(t, "sports_response_invalid", sanitized.Code)
		adapter = mlbTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`{"dates":[{"games":[{"gamePk":0}]}]}`))
		})
		games, err := adapter.fetchGames(context.Background(), "141", time.Now(), time.Now(), false)
		require.NoError(t, err)
		assert.Empty(t, games)
	})
	teams, err := NewMLBAdapter(nil).Teams(context.Background(), LeagueMLB)
	require.NoError(t, err)
	require.Len(t, teams, 30)
	assert.Equal(t, CanonicalTeamID("mlb-stats-api:mlb:141"), mlbTeams["141"].ID)
	assert.Equal(t, "TOR", mlbTeams["141"].Abbreviation)
}

func TestMLBDoubleheaderKeepsBothStableGamesAndLabelsSecondGame(t *testing.T) {
	location, err := time.LoadLocation("America/Toronto")
	require.NoError(t, err)
	start := time.Date(2026, 7, 31, 17, 0, 0, 0, location).UTC()
	games := []Game{
		{ID: NewGameID(ProviderMLB, LeagueMLB, "first"), ProviderGameID: "first", League: LeagueMLB, AwayTeam: mlbTeams["141"], HomeTeam: mlbTeams["138"], Status: GameFinal, ScheduledAt: start, GameType: "R", GameNumber: 1},
		{ID: NewGameID(ProviderMLB, LeagueMLB, "second"), ProviderGameID: "second", League: LeagueMLB, AwayTeam: mlbTeams["141"], HomeTeam: mlbTeams["138"], Status: GameScheduled, ScheduledAt: start.Add(6 * time.Hour), GameType: "R", GameNumber: 2},
	}
	selected := selectMLBCurrentGames(games, "141", "America/Toronto", start)
	require.Len(t, selected, 2)
	assert.Equal(t, "second", selected[0].ProviderGameID, "scheduled G2 retains incumbent priority over final G1")
	assert.Equal(t, "G2", selected[0].GameLabel)
	assert.Equal(t, "G1", selected[1].GameLabel)
	assert.NotEqual(t, selected[0].ID, selected[1].ID)
}

func TestMLBStaleFallbackAndProviderFailureWithoutCache(t *testing.T) {
	var fail atomic.Bool
	adapter := mlbTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = writer.Write(espnFixture(t, "mlb_states.json"))
	})
	request := SportsLiveRequest{League: LeagueMLB, Timezone: "UTC"}
	_, err := adapter.LiveGames(context.Background(), request)
	require.NoError(t, err)
	key := "mlb:live:UTC:20260731"
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

	other := mlbTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusServiceUnavailable) })
	_, err = other.LiveGames(context.Background(), request)
	var sanitized SanitizedError
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "sports_provider_unavailable", sanitized.Code)
}

func TestNFLFixtureNormalizesStatesRecordsOvertimeAndTie(t *testing.T) {
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(espnFixture(t, "nfl_states.json"))
	})
	games, err := adapter.fetchNFLGames(context.Background(), time.Now(), time.Now())
	require.NoError(t, err)
	require.Len(t, games, 17)
	want := []GameStatus{GameScheduled, GamePregame, GameLive, GameLive, GameIntermission, GameLive, GameLive, GameIntermission, GameLive, GameFinal, GameFinal, GameFinal, GameDelayed, GamePostponed, GameCancelled, GameSuspended, GameUnknown}
	for index := range want {
		assert.Equal(t, want[index], games[index].Status, "game %s", games[index].ProviderGameID)
	}
	assert.Equal(t, LeagueNFL, games[0].League)
	assert.Equal(t, CanonicalTeamID("espn-site:nfl:2"), games[0].AwayTeam.ID)
	assert.Equal(t, "BUF", games[0].AwayTeam.Abbreviation)
	assert.Equal(t, "8-3", games[0].AwayRecord)
	assert.Equal(t, "Q1 08:42", games[2].StatusDetail)
	assert.Equal(t, "HALFTIME", games[4].StatusDetail)
	assert.Equal(t, "END Q3", games[7].StatusDetail)
	assert.Equal(t, "OT", games[8].PeriodLabel)
	assert.Equal(t, "FINAL", games[9].StatusDetail)
	assert.Equal(t, "FINAL/OT", games[10].StatusDetail)
	assert.True(t, games[11].Tie)
	assert.Equal(t, "FINAL/TIE", games[11].StatusDetail)
	assert.Equal(t, GameUnknown, normalizeNFLStatus("mystery", "STATUS_NEW", "", "", ""))
}

func TestNFLScheduleUsesDeviceLocalDateAndReturnsFutureGame(t *testing.T) {
	payload := `{"events":[{"id":"nfl-date","date":"2026-09-11T02:30:00Z","status":{"type":{"state":"pre","name":"STATUS_SCHEDULED"}},"competitions":[{"competitors":[{"homeAway":"away","team":{"id":"2"}},{"homeAway":"home","team":{"id":"17"}}]}]}]}`
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "20260909-20261025", request.URL.Query().Get("dates"))
		_, _ = writer.Write([]byte(payload))
	})
	adapter.Now = func() time.Time { return time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC) }
	snapshot, err := adapter.Schedule(context.Background(), SportsScheduleRequest{League: LeagueNFL, TeamID: "2", Timezone: "America/Toronto"})
	require.NoError(t, err)
	require.Len(t, snapshot.Games, 1)
	assert.Equal(t, LeagueNFL, snapshot.League)
	assert.Equal(t, "2026-09-10T22:30:00-04:00", snapshot.Games[0].ScheduledLocal)

	adapter = espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(payload)) })
	adapter.Now = func() time.Time { return time.Date(2026, 9, 8, 16, 0, 0, 0, time.UTC) }
	future, err := adapter.Schedule(context.Background(), SportsScheduleRequest{League: LeagueNFL, TeamID: "2", Timezone: "America/Toronto"})
	require.NoError(t, err)
	assert.Empty(t, future.Games)
	require.NotNil(t, future.NextGame)
	assert.Equal(t, "nfl-date", future.NextGame.ProviderGameID)
}

func TestNFLLiveGamesHandleSundayClusterWithoutDuplicates(t *testing.T) {
	payload := `{"events":[
	{"id":"late","date":"2026-09-13T20:25Z","status":{"period":2,"type":{"state":"in","name":"STATUS_HALFTIME","shortDetail":"Halftime"}},"competitions":[{"competitors":[{"homeAway":"away","team":{"id":"2"}},{"homeAway":"home","team":{"id":"17"}}]}]},
	{"id":"early","date":"2026-09-13T17:00Z","status":{"period":5,"displayClock":"3:10","type":{"state":"in","name":"STATUS_IN_PROGRESS"}},"competitions":[{"competitors":[{"homeAway":"away","team":{"id":"12"}},{"homeAway":"home","team":{"id":"33"}}]}]},
	{"id":"early","date":"2026-09-13T17:00Z","status":{"period":5,"type":{"state":"in","name":"STATUS_IN_PROGRESS"}},"competitions":[{"competitors":[{"homeAway":"away","team":{"id":"12"}},{"homeAway":"home","team":{"id":"33"}}]}]}]}`
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(payload)) })
	adapter.Now = func() time.Time { return time.Date(2026, 9, 13, 18, 0, 0, 0, time.UTC) }
	snapshot, err := adapter.LiveGames(context.Background(), SportsLiveRequest{League: LeagueNFL, Timezone: "America/Toronto"})
	require.NoError(t, err)
	require.Len(t, snapshot.Games, 2)
	assert.Equal(t, "early", snapshot.Games[0].ProviderGameID)
	assert.Equal(t, "OT", snapshot.Games[0].PeriodLabel)
	assert.Equal(t, GameIntermission, snapshot.Games[1].Status)
}

func TestNFLZeroClockDoesNotInventFinalState(t *testing.T) {
	assert.Equal(t, GameLive, normalizeNFLStatus("in", "STATUS_IN_PROGRESS", "", "", "0:00 - 4th Quarter"))
	assert.Equal(t, GameIntermission, normalizeNFLStatus("in", "STATUS_END_PERIOD", "", "", "End of 3rd Quarter"))
}

func TestNFLStaleFallbackAndFailureWithoutCache(t *testing.T) {
	var fail atomic.Bool
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = writer.Write(espnFixture(t, "nfl_states.json"))
	})
	adapter.Now = func() time.Time { return time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC) }
	request := SportsLiveRequest{League: LeagueNFL, Timezone: "UTC"}
	_, err := adapter.LiveGames(context.Background(), request)
	require.NoError(t, err)
	key := "espn:nfl:live:UTC:20260910"
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

	other := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusBadGateway) })
	_, err = other.LiveGames(context.Background(), request)
	var sanitized SanitizedError
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "sports_provider_unavailable", sanitized.Code)
}

func TestNFLOffDayMalformedPartialAndCatalogAreSafe(t *testing.T) {
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(`{"events":[]}`)) })
	snapshot, err := adapter.LiveGames(context.Background(), SportsLiveRequest{League: LeagueNFL, Timezone: "America/Toronto"})
	require.NoError(t, err)
	assert.Empty(t, snapshot.Games)
	policy := espnCachePolicy(snapshot)
	assert.Equal(t, 20*time.Minute, policy.FreshTTL)
	assert.Equal(t, 45*time.Minute, policy.StaleTTL)

	adapter = espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(espnFixture(t, "malformed.json"))
	})
	_, err = adapter.fetchNFLGames(context.Background(), time.Now(), time.Now())
	var sanitized SanitizedError
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "sports_response_invalid", sanitized.Code)

	adapter = espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"events":[{"id":"partial"}]}`))
	})
	games, err := adapter.fetchNFLGames(context.Background(), time.Now(), time.Now())
	require.NoError(t, err)
	assert.Empty(t, games)

	teams, err := adapter.Teams(context.Background(), LeagueNFL)
	require.NoError(t, err)
	require.Len(t, teams, 32)
	seen := map[ProviderTeamID]bool{}
	for _, team := range teams {
		assert.False(t, seen[team.ProviderID])
		seen[team.ProviderID] = true
		assert.Equal(t, NewCanonicalTeamID(ProviderESPN, LeagueNFL, team.ProviderID), team.ID)
	}
	assert.Equal(t, "BUF", nflTeams["2"].Abbreviation)
	assert.Equal(t, "Washington Commanders", nflTeams["28"].DisplayName)
	_, err = adapter.Schedule(context.Background(), SportsScheduleRequest{League: LeagueNFL, TeamID: "BUF", Timezone: "UTC"})
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "sports_team_invalid", sanitized.Code)
}

func TestNBAFixtureNormalizesLeagueSpecificStatesAndOvertime(t *testing.T) {
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(espnFixture(t, "nba_states.json"))
	})
	games, err := adapter.fetchNBAGames(context.Background(), time.Now(), time.Now())
	require.NoError(t, err)
	require.Len(t, games, 16)
	want := []GameStatus{GameScheduled, GamePregame, GameLive, GameLive, GameIntermission, GameLive, GameLive, GameLive, GameLive, GameFinal, GameFinal, GameDelayed, GamePostponed, GameCancelled, GameSuspended, GameUnknown}
	for index := range want {
		assert.Equal(t, want[index], games[index].Status, "game %s", games[index].ProviderGameID)
	}
	assert.Equal(t, LeagueNBA, games[0].League)
	assert.Equal(t, CanonicalTeamID("espn-site:nba:28"), games[0].AwayTeam.ID)
	assert.Equal(t, "TOR", games[0].AwayTeam.Abbreviation)
	assert.Equal(t, "12-8", games[0].AwayRecord)
	assert.Equal(t, "Q1 08:42", games[2].StatusDetail)
	assert.Equal(t, "HALFTIME", games[4].StatusDetail)
	assert.Equal(t, "OT", games[7].PeriodLabel)
	assert.Equal(t, "2OT", games[8].PeriodLabel)
	assert.Equal(t, "FINAL", games[9].StatusDetail)
	assert.Equal(t, "FINAL/2OT", games[10].StatusDetail)
	assert.Equal(t, GameUnknown, normalizeNBAStatus("mystery", "STATUS_NEW", "", "", ""))
}

func TestNBAScheduleUsesDeviceLocalDateAndReturnsNextGame(t *testing.T) {
	payload := `{"events":[{"id":"nba-date","date":"2026-10-02T02:30:00Z","status":{"type":{"state":"pre","name":"STATUS_SCHEDULED"}},"competitions":[{"competitors":[{"id":"28","homeAway":"away","team":{"id":"28"}},{"id":"2","homeAway":"home","team":{"id":"2"}}]}]}]}`
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "20260930-20261115", request.URL.Query().Get("dates"))
		_, _ = writer.Write([]byte(payload))
	})
	adapter.Now = func() time.Time { return time.Date(2026, 10, 1, 16, 0, 0, 0, time.UTC) }
	snapshot, err := adapter.Schedule(context.Background(), SportsScheduleRequest{League: LeagueNBA, TeamID: "28", Timezone: "America/Toronto"})
	require.NoError(t, err)
	require.Len(t, snapshot.Games, 1)
	assert.Equal(t, LeagueNBA, snapshot.League)
	location, err := time.LoadLocation("America/Toronto")
	require.NoError(t, err)
	assert.Equal(t, "2026-10-01", snapshot.Games[0].ScheduledAt.In(location).Format("2006-01-02"))
	assert.Equal(t, "2026-10-01T22:30:00-04:00", snapshot.Games[0].ScheduledLocal)
}

func TestNBAOffDayAndNextFutureGameUseBoundedScheduleCache(t *testing.T) {
	t.Run("off day", func(t *testing.T) {
		adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(`{"events":[]}`)) })
		snapshot, err := adapter.LiveGames(context.Background(), SportsLiveRequest{League: LeagueNBA, Timezone: "America/Toronto"})
		require.NoError(t, err)
		assert.Empty(t, snapshot.Games)
		assert.Nil(t, snapshot.NextGame)
		policy := espnCachePolicy(snapshot)
		assert.Equal(t, 20*time.Minute, policy.FreshTTL)
		assert.Equal(t, 45*time.Minute, policy.StaleTTL)
	})

	t.Run("future", func(t *testing.T) {
		payload := `{"events":[{"id":"nba-next","date":"2026-08-08T23:30:00Z","status":{"type":{"state":"pre","name":"STATUS_SCHEDULED"}},"competitions":[{"competitors":[{"homeAway":"away","team":{"id":"28"}},{"homeAway":"home","team":{"id":"2"}}]}]}]}`
		adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(payload)) })
		snapshot, err := adapter.Schedule(context.Background(), SportsScheduleRequest{League: LeagueNBA, TeamID: "28", Timezone: "America/Toronto"})
		require.NoError(t, err)
		assert.Empty(t, snapshot.Games)
		require.NotNil(t, snapshot.NextGame)
		assert.Equal(t, "nba-next", snapshot.NextGame.ProviderGameID)
		require.Len(t, snapshot.UpcomingGames, 1)
	})
}

func TestNBALiveGamesAreOrderedDeduplicatedAndKeepHalftimeAndOT(t *testing.T) {
	payload := `{"events":[
	{"id":"nba-2","date":"2026-08-06T21:00Z","status":{"period":2,"type":{"state":"in","name":"STATUS_HALFTIME","shortDetail":"Halftime"}},"competitions":[{"competitors":[{"homeAway":"away","team":{"id":"28"}},{"homeAway":"home","team":{"id":"2"}}]}]},
	{"id":"nba-1","date":"2026-08-06T20:00Z","status":{"period":5,"displayClock":"2:10","type":{"state":"in","name":"STATUS_IN_PROGRESS"}},"competitions":[{"competitors":[{"homeAway":"away","team":{"id":"13"}},{"homeAway":"home","team":{"id":"9"}}]}]},
	{"id":"nba-1","date":"2026-08-06T20:00Z","status":{"period":5,"type":{"state":"in","name":"STATUS_IN_PROGRESS"}},"competitions":[{"competitors":[{"homeAway":"away","team":{"id":"13"}},{"homeAway":"home","team":{"id":"9"}}]}]}]}`
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(payload)) })
	snapshot, err := adapter.LiveGames(context.Background(), SportsLiveRequest{League: LeagueNBA, Timezone: "America/Toronto"})
	require.NoError(t, err)
	require.Len(t, snapshot.Games, 2)
	assert.Equal(t, "nba-1", snapshot.Games[0].ProviderGameID)
	assert.Equal(t, "OT", snapshot.Games[0].PeriodLabel)
	assert.Equal(t, GameIntermission, snapshot.Games[1].Status)
}

func TestNBAStaleFallbackAndFailureWithoutCache(t *testing.T) {
	var fail atomic.Bool
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = writer.Write(espnFixture(t, "nba_states.json"))
	})
	request := SportsLiveRequest{League: LeagueNBA, Timezone: "UTC"}
	_, err := adapter.LiveGames(context.Background(), request)
	require.NoError(t, err)
	key := "espn:nba:live:UTC:20260806"
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

	other := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusBadGateway) })
	_, err = other.LiveGames(context.Background(), request)
	var sanitized SanitizedError
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "sports_provider_unavailable", sanitized.Code)
}

func TestNBAMalformedPartialPayloadAndTeamCatalogAreSafe(t *testing.T) {
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(espnFixture(t, "malformed.json"))
	})
	_, err := adapter.fetchNBAGames(context.Background(), time.Now(), time.Now())
	var sanitized SanitizedError
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "sports_response_invalid", sanitized.Code)

	adapter = espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"events":[{"id":"partial"}]}`))
	})
	games, err := adapter.fetchNBAGames(context.Background(), time.Now(), time.Now())
	require.NoError(t, err)
	assert.Empty(t, games)

	teams, err := adapter.Teams(context.Background(), LeagueNBA)
	require.NoError(t, err)
	require.Len(t, teams, 30)
	seen := map[ProviderTeamID]bool{}
	for _, team := range teams {
		assert.False(t, seen[team.ProviderID])
		seen[team.ProviderID] = true
		assert.Equal(t, NewCanonicalTeamID(ProviderESPN, LeagueNBA, team.ProviderID), team.ID)
	}
	assert.Equal(t, "TOR", nbaTeams["28"].Abbreviation)
	_, err = adapter.Schedule(context.Background(), SportsScheduleRequest{League: LeagueNBA, TeamID: "TOR", Timezone: "UTC"})
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "sports_team_invalid", sanitized.Code)
}

func TestNBACachePolicyMatchesGameLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 1, 16, 0, 0, 0, time.UTC)
	for name, snapshot := range map[string]SportsSnapshot{
		"active":    {Games: []Game{{Status: GameLive}}, FreshAsOf: now},
		"halftime":  {Games: []Game{{Status: GameIntermission}}, FreshAsOf: now},
		"pregame":   {Games: []Game{{Status: GamePregame}}, FreshAsOf: now},
		"scheduled": {Games: []Game{{Status: GameScheduled, ScheduledAt: now.Add(2 * time.Hour)}}, FreshAsOf: now},
		"final":     {Games: []Game{{Status: GameFinal}}, FreshAsOf: now},
		"off day":   {Games: []Game{}, FreshAsOf: now},
	} {
		t.Run(name, func(t *testing.T) {
			policy := espnCachePolicy(snapshot)
			switch name {
			case "active", "halftime":
				assert.Equal(t, 15*time.Second, policy.FreshTTL)
				assert.Equal(t, 2*time.Minute, policy.StaleTTL)
			case "pregame":
				assert.Equal(t, time.Minute, policy.FreshTTL)
			case "scheduled", "final":
				assert.Equal(t, 5*time.Minute, policy.FreshTTL)
				assert.Equal(t, 45*time.Minute, policy.StaleTTL)
			case "off day":
				assert.Equal(t, 20*time.Minute, policy.FreshTTL)
				assert.Equal(t, 45*time.Minute, policy.StaleTTL)
			}
		})
	}
}

func TestNBALiveCacheCoalescesConcurrentRequests(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	fixture := espnFixture(t, "nba_states.json")
	adapter := espnTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = writer.Write(fixture)
	})
	request := SportsLiveRequest{League: LeagueNBA, Timezone: "UTC"}
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _ = adapter.LiveGames(context.Background(), request)
		}()
	}
	<-started
	close(release)
	wait.Wait()
	assert.Equal(t, int32(1), calls.Load())
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

func TestESPNTeamNormalizationPreservesTrustedProviderLogoURLs(t *testing.T) {
	nba := nbaTeamFromPayload(espnTeamPayload{ID: "28", Logo: "https://a.espncdn.com/i/teamlogos/nba/500/tor.png"})
	nfl := nflTeamFromPayload(espnTeamPayload{ID: "2", Logo: "https://a.espncdn.com/i/teamlogos/nfl/500/buf.png"})
	assert.Equal(t, "https://a.espncdn.com/i/teamlogos/nba/500/tor.png", nba.ProviderLogoURL)
	assert.Equal(t, "https://a.espncdn.com/i/teamlogos/nfl/500/buf.png", nfl.ProviderLogoURL)
	assert.Empty(t, nbaTeamFromPayload(espnTeamPayload{ID: "28", Logo: "http://example.test/logo.png"}).ProviderLogoURL)
}
