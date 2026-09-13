package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type nhlOverviewStanding struct {
	TeamLogo           string       `json:"teamLogo"`
	SeasonID           int          `json:"seasonId"`
	Date               string       `json:"date"`
	TeamAbbrev         nhlLocalized `json:"teamAbbrev"`
	Wins               *int         `json:"wins"`
	Losses             *int         `json:"losses"`
	OTLosses           *int         `json:"otLosses"`
	DivisionName       string       `json:"divisionName"`
	ConferenceAbbrev   string       `json:"conferenceAbbrev"`
	DivisionSequence   int          `json:"divisionSequence"`
	ConferenceSequence int          `json:"conferenceSequence"`
	WildcardSequence   int          `json:"wildcardSequence"`
	ClinchIndicator    string       `json:"clinchIndicator"`
}

func (a *NHLAdapter) Overview(ctx context.Context, req SportsScheduleRequest) (SportsOverview, error) {
	if err := req.Validate(); err != nil {
		return SportsOverview{}, err
	}
	if req.League != LeagueNHL {
		return SportsOverview{}, errorsForUnsupportedLeague()
	}
	team, ok := nhlTeams[req.TeamID]
	if !ok {
		return SportsOverview{}, UnknownSportsTeam(req.League, req.TeamID)
	}
	table, ts, te := a.OverviewCache.Standings.GetWithPolicy(ctx, "nhl", func(ctx context.Context) (overviewTable, CachePolicy, error) {
		t, e := a.fetchOverviewStandings(ctx)
		return t, overviewPolicy(t.Phase, OverviewStandingsTTL), e
	})
	schedule, ss, se := a.OverviewCache.Schedules.GetWithPolicy(ctx, string(req.TeamID), func(ctx context.Context) (overviewSchedule, CachePolicy, error) {
		s, e := a.fetchOverviewSchedule(ctx, team, table.Phase)
		return s, overviewPolicy(s.Phase, OverviewScheduleTTL), e
	})
	if te != nil && se != nil {
		return SportsOverview{League: req.League, Team: team, Season: OverviewSeason{Phase: "unknown"}}, nil
	}
	return assembleOverview(team, table, schedule, ts || ss || (te != nil) || (se != nil), a.now(), req.Timezone), nil
}
func (a *NHLAdapter) fetchOverviewStandings(ctx context.Context) (overviewTable, error) {
	var p struct {
		Standings []nhlOverviewStanding `json:"standings"`
	}
	if err := overviewJSON(ctx, a.Client, strings.TrimRight(a.BaseURL, "/")+"/standings/now", &p); err != nil {
		return overviewTable{}, err
	}
	if p.Standings == nil || len(p.Standings) > 64 {
		return overviewTable{}, SportsUnavailable()
	}
	t := overviewTable{Rows: map[ProviderTeamID]overviewRow{}, FetchedAt: a.now(), Phase: "regular"}
	for _, raw := range p.Standings {
		var team Team
		for _, v := range nhlTeams {
			if v.Abbreviation == raw.TeamAbbrev.Default {
				team = v
				break
			}
		}
		if team.ID == "" {
			continue
		}
		date, e := time.Parse("2006-01-02", raw.Date)
		if e != nil {
			continue
		}
		if a.now().Sub(date) > 21*24*time.Hour {
			t.Phase = "offseason"
		}
		row := overviewRow{LogoURL: raw.TeamLogo, Season: OverviewSeason{Label: nhlSeasonLabel(raw.SeasonID), Phase: t.Phase}}
		if raw.Wins != nil && raw.Losses != nil && raw.OTLosses != nil {
			row.Season.Record = overviewRecord(LeagueNHL, *raw.Wins, *raw.Losses, *raw.OTLosses)
		}
		division := map[string]string{"Atlantic": "ATL", "Metropolitan": "MET", "Central": "CEN", "Pacific": "PAC"}[raw.DivisionName]
		row.Standing = OverviewStanding{Division: division, Conference: strings.ToUpper(raw.ConferenceAbbrev), DivisionRank: validRank(raw.DivisionSequence, 8), ConferenceRank: validRank(raw.ConferenceSequence, 16), Wildcard: validRank(raw.WildcardSequence, 2), Clinched: containsAny(strings.ToLower(raw.ClinchIndicator), "x", "y", "z", "p")}
		t.Rows[team.ProviderID] = row
	}
	if len(t.Rows) == 0 && len(p.Standings) > 0 {
		return overviewTable{}, SportsUnavailable()
	}
	return t, nil
}
func nhlSeasonLabel(season int) string {
	if season < 20002001 || season > 22002201 {
		return ""
	}
	return fmt.Sprintf("%d-%02d", season/10000, season%100)
}
func (a *NHLAdapter) fetchOverviewSchedule(ctx context.Context, team Team, tablePhase string) (overviewSchedule, error) {
	var p struct {
		Season int                   `json:"season"`
		Games  []jsonNHLOverviewGame `json:"games"`
	}
	endpoint := strings.TrimRight(a.BaseURL, "/") + "/club-schedule-season/" + team.Abbreviation + "/"
	if err := overviewJSON(ctx, a.Client, endpoint+"now", &p); err != nil {
		return overviewSchedule{}, err
	}
	if p.Games == nil || len(p.Games) > 200 {
		return overviewSchedule{}, SportsUnavailable()
	}
	s := overviewSchedule{Season: nhlSeasonLabel(p.Season), Phase: tablePhase, FetchedAt: a.now()}
	add := func(raws []jsonNHLOverviewGame) {
		for _, raw := range raws {
			g, ok := normalizeNHLGame(raw.nhlGamePayload, a.now())
			if ok && (g.Status != GameFinal || raw.ScoresKnown) {
				g.GameType = strconv.Itoa(raw.GameType)
				s.Games = append(s.Games, g)
			}
		}
	}
	add(p.Games)
	// Newly published preseason schedules can omit the preceding final result.
	hasFinal := false
	for _, g := range s.Games {
		if g.Status == GameFinal {
			hasFinal = true
		}
	}
	if !hasFinal && p.Season > 20002001 {
		var previous struct {
			Games []jsonNHLOverviewGame `json:"games"`
		}
		if overviewJSON(ctx, a.Client, endpoint+strconv.Itoa(p.Season-10001), &previous) == nil && len(previous.Games) <= 200 {
			add(previous.Games)
		}
	}
	closest := time.Duration(1<<63 - 1)
	for _, g := range s.Games {
		delta := g.ScheduledAt.Sub(a.now())
		if delta < -7*24*time.Hour || delta > 14*24*time.Hour {
			continue
		}
		if delta < 0 {
			delta = -delta
		}
		if delta < closest {
			closest = delta
			s.Phase = map[string]string{"1": "preseason", "2": "regular", "3": "playoffs"}[g.GameType]
		}
	}
	if s.Phase == "" {
		s.Phase = "unknown"
	}
	sortGames(s.Games)
	return s, nil
}

type jsonNHLOverviewGame struct {
	nhlGamePayload
	GameType    int  `json:"gameType"`
	ScoresKnown bool `json:"-"`
}

func validRank(n, max int) int {
	if n < 1 || n > max {
		return 0
	}
	return n
}

func (g *jsonNHLOverviewGame) UnmarshalJSON(body []byte) error {
	type plain jsonNHLOverviewGame
	var v plain
	if err := json.Unmarshal(body, &v); err != nil {
		return err
	}
	var scores struct {
		Home struct {
			Score *int `json:"score"`
		} `json:"homeTeam"`
		Away struct {
			Score *int `json:"score"`
		} `json:"awayTeam"`
	}
	if err := json.Unmarshal(body, &scores); err != nil {
		return err
	}
	*g = jsonNHLOverviewGame(v)
	g.ScoresKnown = scores.Home.Score != nil && scores.Away.Score != nil
	return nil
}
