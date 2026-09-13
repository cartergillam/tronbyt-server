package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var overviewNow = time.Date(2026, 1, 10, 18, 0, 0, 0, time.UTC)

func nhlOverviewFixture(kind string) string {
	return fmt.Sprintf(`{"season":20252026,"games":[{"id":1,"gameType":2,"startTimeUTC":"2026-01-09T00:30:00Z","gameState":"OFF","awayTeam":{"id":6,"score":2},"homeTeam":{"id":10,"score":4},"gameOutcome":{"lastPeriodType":%q}},{"id":2,"gameType":2,"startTimeUTC":"2026-01-11T00:00:00Z","gameState":"FUT","awayTeam":{"id":10},"homeTeam":{"id":9}}]}`, kind)
}

const nhlOverviewTableFixture = `{"standings":[{"seasonId":20252026,"date":"2026-01-10","teamAbbrev":{"default":"TOR"},"wins":32,"losses":18,"otLosses":6,"divisionName":"Atlantic","conferenceAbbrev":"E","divisionSequence":3,"conferenceSequence":6,"wildcardSequence":1},{"seasonId":20252026,"date":"2026-01-10","teamAbbrev":{"default":"BOS"},"wins":30,"losses":20,"otLosses":5,"divisionName":"Atlantic","conferenceAbbrev":"E","divisionSequence":4}]}`

func espnOverviewTableFixture(league LeagueID, phase int) string {
	id, conf := "13", "West"
	if league == LeagueNFL {
		id, conf = "2", "AFC"
	}
	return fmt.Sprintf(`{"season":{"startDate":"2025-09-01T00:00Z","endDate":"2026-07-01T00:00Z"},"children":[{"abbreviation":%q,"isConference":true,"standings":{"season":2026,"seasonType":%d,"seasonDisplayName":"2025-26","entries":[{"team":{"id":%q},"stats":[{"name":"wins","value":10},{"name":"losses","value":6},{"name":"ties","value":1},{"name":"playoffSeed","value":3}]}]}}]}`, conf, phase, id)
}
func espnOverviewScheduleFixture(league LeagueID, period, home, away int) string {
	favorite, opponent := "13", "28"
	if league == LeagueNFL {
		favorite, opponent = "2", "17"
	}
	return fmt.Sprintf(`{"season":{"year":2026,"type":2,"displayName":"2025-26"},"team":{"standingSummary":"2nd in AFC East"},"events":[{"id":"1","date":"2026-01-09T00:30Z","competitions":[{"status":{"period":%d,"type":{"state":"post","name":"STATUS_FINAL","completed":true}},"competitors":[{"homeAway":"home","team":{"id":%q},"score":{"value":%d}},{"homeAway":"away","team":{"id":%q},"score":{"value":%d}}]}]},{"id":"2","date":"2026-01-11T18:00Z","competitions":[{"status":{"type":{"state":"pre","name":"STATUS_SCHEDULED"}},"competitors":[{"homeAway":"away","team":{"id":%q}},{"homeAway":"home","team":{"id":%q}}]}]}]}`, period, favorite, home, opponent, away, favorite, opponent)
}
func overviewHTTP(body func(*http.Request) string, calls *atomic.Int32) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body(r)))}, nil
	})}
}
func TestOverviewNHLRecordRanksNextLastAndOvertime(t *testing.T) {
	for _, kind := range []string{"REG", "OT", "SO"} {
		t.Run(kind, func(t *testing.T) {
			calls := &atomic.Int32{}
			a := NewNHLAdapter(overviewHTTP(func(r *http.Request) string {
				if strings.Contains(r.URL.Path, "standings") {
					return nhlOverviewTableFixture
				}
				return nhlOverviewFixture(kind)
			}, calls))
			a.Now = func() time.Time { return overviewNow }
			req := SportsScheduleRequest{League: LeagueNHL, TeamID: "10", Timezone: "America/Toronto"}
			r, e := a.Overview(t.Context(), req)
			require.NoError(t, e)
			require.Equal(t, "32-18-6", r.Season.Record.Display)
			require.Equal(t, 56, r.Season.Record.GamesPlayed)
			require.Equal(t, 6, *r.Season.Record.OvertimeLosses)
			require.Equal(t, 3, r.Standing.DivisionRank)
			require.Equal(t, 6, r.Standing.ConferenceRank)
			require.Equal(t, "#3 ATL", r.Standing.Label)
			require.Equal(t, "7:00PM", r.NextGame.TimeLabel)
			require.Equal(t, "JAN 10", r.NextGame.DateLabel)
			require.Equal(t, "W", r.LastGame.Result)
			require.Equal(t, kind == "SO", r.LastGame.Shootout)
			require.Equal(t, kind == "OT", r.LastGame.Overtime)
			for i := 0; i < 30; i++ {
				req.Timezone = "America/Vancouver"
				_, e = a.Overview(t.Context(), req)
				require.NoError(t, e)
			}
			require.Equal(t, int32(2), calls.Load())
			req.TeamID = "6"
			_, e = a.Overview(t.Context(), req)
			require.NoError(t, e)
			require.Equal(t, int32(3), calls.Load()) // Standings are league-wide.
		})
	}
}
func TestOverviewESPNLeaguesAndResults(t *testing.T) {
	for _, league := range []LeagueID{LeagueNBA, LeagueNFL} {
		for _, period := range []int{4, 5, 6} {
			for _, scores := range [][2]int{{120, 110}, {17, 20}, {20, 20}} {
				t.Run(fmt.Sprint(league, period, scores), func(t *testing.T) {
					calls := &atomic.Int32{}
					a := NewESPNAdapter(overviewHTTP(func(r *http.Request) string {
						if strings.Contains(r.URL.Path, "standings") {
							return espnOverviewTableFixture(league, 2)
						}
						return espnOverviewScheduleFixture(league, period, scores[0], scores[1])
					}, calls))
					a.Now = func() time.Time { return overviewNow }
					id := ProviderTeamID("13")
					if league == LeagueNFL {
						id = "2"
					}
					r, e := a.Overview(t.Context(), SportsScheduleRequest{League: league, TeamID: id, Timezone: "America/Toronto"})
					require.NoError(t, e)
					record := "10-6"
					if league == LeagueNFL {
						record += "-1"
						require.Equal(t, "#2 AFC E", r.Standing.Label)
					}
					require.Equal(t, record, r.Season.Record.Display)
					require.Equal(t, 3, r.Standing.Seed)
					require.Equal(t, "1:00PM", r.NextGame.TimeLabel)
					result := "W"
					if scores[0] < scores[1] {
						result = "L"
					} else if scores[0] == scores[1] {
						result = "T"
					}
					require.Equal(t, result, r.LastGame.Result)
					require.Equal(t, period > 4, r.LastGame.Overtime)
					if league == LeagueNBA && period == 6 {
						require.Equal(t, "FINAL/2OT", r.LastGame.FinalLabel)
					}
				})
			}
		}
	}
}
func expireOverview[T any](cache *Cache[T], lkg bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for k, e := range cache.entries {
		e.freshUntil = time.Now().Add(-time.Minute)
		if !lkg {
			e.staleUntil = time.Now().Add(-time.Minute)
		}
		cache.entries[k] = e
		delete(cache.lastRun, k)
	}
}
func TestOverviewCacheStaleCoalescingAndPolicies(t *testing.T) {
	calls := &atomic.Int32{}
	fail := atomic.Bool{}
	a := NewNHLAdapter(overviewHTTP(func(r *http.Request) string {
		if fail.Load() {
			return `{broken`
		}
		if strings.Contains(r.URL.Path, "standings") {
			return nhlOverviewTableFixture
		}
		return nhlOverviewFixture("REG")
	}, calls))
	a.Now = func() time.Time { return overviewNow }
	req := SportsScheduleRequest{League: LeagueNHL, TeamID: "10", Timezone: "America/Toronto"}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() { _, e := a.Overview(context.Background(), req); require.NoError(t, e) })
	}
	wg.Wait()
	require.Equal(t, int32(2), calls.Load())
	expireOverview(a.OverviewCache.Standings, true)
	expireOverview(a.OverviewCache.Schedules, true)
	fail.Store(true)
	for i := 0; i < 20; i++ {
		r, e := a.Overview(t.Context(), req)
		require.NoError(t, e)
		require.True(t, r.Stale)
		require.NotNil(t, r.Season.Record)
		require.NotNil(t, r.LastGame)
	}
	require.Equal(t, int32(4), calls.Load())
	require.Equal(t, 30*time.Minute, overviewPolicy("regular", OverviewStandingsTTL).FreshTTL)
	require.Equal(t, 10*time.Minute, overviewPolicy("regular", OverviewScheduleTTL).FreshTTL)
	require.Equal(t, 6*time.Hour, overviewPolicy("offseason", OverviewScheduleTTL).FreshTTL)
	require.Equal(t, 24*time.Hour, overviewPolicy("offseason", OverviewScheduleTTL).StaleTTL)
}
func TestOverviewSeasonAndPartialFailure(t *testing.T) {
	for _, league := range []LeagueID{LeagueNBA, LeagueNFL} {
		for _, phase := range []int{1, 2, 3, 4} {
			calls := &atomic.Int32{}
			a := NewESPNAdapter(overviewHTTP(func(r *http.Request) string {
				if strings.Contains(r.URL.Path, "standings") {
					return espnOverviewTableFixture(league, phase)
				}
				return fmt.Sprintf(`{"season":{"type":%d},"events":[]}`, phase)
			}, calls))
			a.Now = func() time.Time { return overviewNow }
			id := ProviderTeamID("13")
			if league == LeagueNFL {
				id = "2"
			}
			r, e := a.Overview(t.Context(), SportsScheduleRequest{League: league, TeamID: id, Timezone: "UTC"})
			require.NoError(t, e)
			require.Equal(t, espnOverviewPhase(phase), r.Season.Phase)
			require.Nil(t, r.NextGame)
			require.Nil(t, r.LastGame)
			if phase == 1 {
				require.Nil(t, r.Season.Record)
				require.Nil(t, r.Standing)
			}
			if phase == 4 {
				require.Nil(t, r.Standing)
			}
		}
	}
	calls := &atomic.Int32{}
	a := NewNHLAdapter(overviewHTTP(func(r *http.Request) string {
		if strings.Contains(r.URL.Path, "standings") {
			return strings.ReplaceAll(nhlOverviewTableFixture, "2026-01-10", "2025-04-17")
		}
		return `{"games":[]}`
	}, calls))
	a.Now = func() time.Time { return overviewNow }
	r, e := a.Overview(t.Context(), SportsScheduleRequest{League: LeagueNHL, TeamID: "10", Timezone: "UTC"})
	require.NoError(t, e)
	require.Equal(t, "offseason", r.Season.Phase)
	require.Nil(t, r.Standing)
	require.NotNil(t, r.Season.Record)
}
func TestOverviewMalformedNullBoundedAndBadScores(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"standings":null}`, `invalid`, strings.Repeat("x", (2<<20)+1)} {
		calls := &atomic.Int32{}
		a := NewNHLAdapter(overviewHTTP(func(*http.Request) string { return body }, calls))
		_, e := a.fetchOverviewStandings(t.Context())
		require.Error(t, e)
	}
	for _, score := range []string{`null`, `"bad"`, `{"value":-1}`, `{"value":1000}`, `{"value":1.5}`, `{"value":null}`, `{"value":NaN}`} {
		body := espnOverviewScheduleFixture(LeagueNBA, 4, 120, 110)
		body = strings.Replace(body, `{"value":120}`, score, 1)
		var p espnOverviewSchedulePayload
		require.NoError(t, json.Unmarshal([]byte(strings.ReplaceAll(body, "NaN", "null")), &p))
		_, ok := normalizeOverviewESPNEvent(p.Events[0], LeagueNBA, overviewNow)
		require.False(t, ok, score)
	}
	for _, league := range []LeagueID{LeagueNHL, LeagueNBA, LeagueNFL} {
		require.Nil(t, overviewRecord(league, -1, 0, 0))
		require.Nil(t, overviewRecord(league, 0, 1000, 0))
	}
}
func TestOverviewTimezoneAndWireSafety(t *testing.T) {
	for _, tc := range []struct{ at, tz, date, clock string }{{"2026-01-11T00:00:00Z", "America/Toronto", "JAN 10", "7:00PM"}, {"2026-03-08T06:30:00Z", "America/Toronto", "MAR 8", "1:30AM"}, {"2026-03-08T07:30:00Z", "America/Toronto", "MAR 8", "3:30AM"}} {
		at, e := time.Parse(time.RFC3339, tc.at)
		require.NoError(t, e)
		r := overviewGame(Game{ScheduledAt: at}, "10", tc.tz)
		require.Equal(t, tc.date, r.DateLabel)
		require.Equal(t, tc.clock, r.TimeLabel)
	}
	team := nhlTeams["10"]
	body, e := json.Marshal(SportsOverview{Team: team})
	require.NoError(t, e)
	require.NotContains(t, string(body), "https://")
	require.NotContains(t, string(body), "ProviderLogoURL")
}

func TestOverviewSeasonRolloverDoesNotReuseOldRank(t *testing.T) {
	team := nhlTeams["10"]
	table := overviewTable{Rows: map[ProviderTeamID]overviewRow{"10": {Season: OverviewSeason{Label: "2025-26", Record: overviewRecord(LeagueNHL, 40, 30, 12)}, Standing: OverviewStanding{Division: "ATL", DivisionRank: 1}}}}
	r := assembleOverview(team, table, overviewSchedule{Season: "2026-27", Phase: "regular"}, false, overviewNow, "UTC")
	require.Nil(t, r.Season.Record)
	require.Nil(t, r.Standing)
	require.Equal(t, "2026-27", r.Season.Label)
}
func TestOverviewNHLNullFinalScoreIsNotInvented(t *testing.T) {
	calls := &atomic.Int32{}
	a := NewNHLAdapter(overviewHTTP(func(r *http.Request) string {
		return strings.Replace(nhlOverviewFixture("REG"), `"score":4`, `"score":null`, 1)
	}, calls))
	a.Now = func() time.Time { return overviewNow }
	s, e := a.fetchOverviewSchedule(t.Context(), nhlTeams["10"], "regular")
	require.NoError(t, e)
	for _, g := range s.Games {
		require.NotEqual(t, GameFinal, g.Status)
	}
}
func TestOverviewUnknownScheduledTimeIsTBD(t *testing.T) {
	var p espnOverviewSchedulePayload
	body := strings.ReplaceAll(espnOverviewScheduleFixture(LeagueNFL, 4, 20, 17), `"status":`, `"timeValid":false,"status":`)
	require.NoError(t, json.Unmarshal([]byte(body), &p))
	g, ok := normalizeOverviewESPNEvent(p.Events[1], LeagueNFL, overviewNow)
	require.True(t, ok)
	require.Equal(t, "TBD", overviewGame(g, "2", "America/Toronto").TimeLabel)
}

func TestOverviewESPNStandingsSharedAcrossTeamsAndTimezones(t *testing.T) {
	for _, league := range []LeagueID{LeagueNBA, LeagueNFL} {
		calls := &atomic.Int32{}
		a := NewESPNAdapter(overviewHTTP(func(r *http.Request) string {
			if strings.Contains(r.URL.Path, "standings") {
				return espnOverviewTableFixture(league, 2)
			}
			return espnOverviewScheduleFixture(league, 4, 20, 17)
		}, calls))
		a.Now = func() time.Time { return overviewNow }
		ids := []ProviderTeamID{"13", "28"}
		if league == LeagueNFL {
			ids = []ProviderTeamID{"2", "17"}
		}
		for i := 0; i < 40; i++ {
			tz := "America/Toronto"
			if i%3 == 0 {
				tz = "America/Vancouver"
			}
			_, e := a.Overview(t.Context(), SportsScheduleRequest{League: league, TeamID: ids[i%2], Timezone: tz})
			require.NoError(t, e)
		}
		require.Equal(t, int32(3), calls.Load())
	}
}
func TestOverviewLogosShareExistingCacheAndDoNotMutateReport(t *testing.T) {
	var encoded bytes.Buffer
	canvas := image.NewNRGBA(image.Rect(0, 0, 20, 20))
	for y := 5; y < 15; y++ {
		for x := 5; x < 15; x++ {
			canvas.SetNRGBA(x, y, color.NRGBA{255, 255, 255, 255})
		}
	}
	require.NoError(t, png.Encode(&encoded, canvas))
	calls := atomic.Int32{}
	h := NewSportsLogoCache(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Request: r, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(encoded.Bytes()))}, nil
	})})

	for i, tc := range []struct {
		team Team
		url  string
	}{
		{nhlTeams["10"], "https://assets.nhle.com/logos/nhl/svg/TOR_light.svg"},
		{nbaTeams["13"], "https://a.espncdn.com/i/teamlogos/nba/500/lal.png"},
		{nflTeams["2"], "https://a.espncdn.com/i/teamlogos/nfl/500/buf.png"},
	} {
		static := tc.team
		scheduleTeam := static
		scheduleTeam.ProviderLogoURL = tc.url
		report := SportsOverview{League: static.League, Team: static, NextGame: &OverviewGame{Game: Game{HomeTeam: scheduleTeam, AwayTeam: scheduleTeam}}}
		for n := 0; n < 20; n++ {
			r := report.WithLogos(t.Context(), h)
			require.NotEmpty(t, r.Team.LogoData)
			require.NotEmpty(t, r.NextGame.HomeTeam.LogoData)
			require.Equal(t, static.ID, r.Team.ID)
		}
		require.Equal(t, int32(i+1), calls.Load())
		// The same cache instance also serves a Live snapshot without another fetch.
		live := h.Hydrate(t.Context(), SportsSnapshot{League: static.League, Games: []Game{{HomeTeam: scheduleTeam}}})
		require.NotEmpty(t, live.Games[0].HomeTeam.LogoData)
		require.Equal(t, int32(i+1), calls.Load())
		require.Empty(t, report.Team.LogoData)
		require.Empty(t, report.Team.ProviderLogoURL)
		require.Empty(t, report.NextGame.HomeTeam.LogoData)
	}

}

func TestOverviewLogoMetadataReachesExistingHydrator(t *testing.T) {
	calls := &atomic.Int32{}
	table := strings.Replace(nhlOverviewTableFixture, `"teamAbbrev":{"default":"TOR"}`, `"teamLogo":"https://assets.nhle.com/logos/nhl/svg/TOR_light.svg","teamAbbrev":{"default":"TOR"}`, 1)
	a := NewNHLAdapter(overviewHTTP(func(*http.Request) string { return table }, calls))
	a.Now = func() time.Time { return overviewNow }
	t1, e := a.fetchOverviewStandings(t.Context())
	require.NoError(t, e)
	r := assembleOverview(nhlTeams["10"], t1, overviewSchedule{}, false, overviewNow, "UTC")
	require.NotEmpty(t, r.Team.ProviderLogoURL)

	for _, tc := range []struct {
		league   LeagueID
		id, logo string
	}{
		{LeagueNBA, "13", "https://a.espncdn.com/i/teamlogos/nba/500/lal.png"},
		{LeagueNFL, "2", "https://a.espncdn.com/i/teamlogos/nfl/500/buf.png"},
	} {
		needle := fmt.Sprintf(`"team":{"id":%q}`, tc.id)
		replacement := fmt.Sprintf(`"team":{"id":%q,"logos":[{"href":%q}]}`, tc.id, tc.logo)
		body := strings.ReplaceAll(espnOverviewScheduleFixture(tc.league, 4, 120, 110), needle, replacement)
		var p espnOverviewSchedulePayload
		require.NoError(t, json.Unmarshal([]byte(body), &p))
		g, ok := normalizeOverviewESPNEvent(p.Events[0], tc.league, overviewNow)
		require.True(t, ok)
		require.Equal(t, tc.logo, g.HomeTeam.ProviderLogoURL)
		body2 := strings.Replace(espnOverviewTableFixture(tc.league, 2), needle, replacement, 1)
		adapter := NewESPNAdapter(overviewHTTP(func(*http.Request) string { return body2 }, calls))
		adapter.Now = func() time.Time { return overviewNow }
		table, e := adapter.fetchOverviewStandings(t.Context(), tc.league)
		require.NoError(t, e)
		require.Equal(t, tc.logo, table.Rows[ProviderTeamID(tc.id)].LogoURL)
	}
}
