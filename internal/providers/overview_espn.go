package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type espnOverviewStat struct {
	Name  string   `json:"name"`
	Value *float64 `json:"value"`
}
type espnOverviewGroup struct {
	Abbreviation string              `json:"abbreviation"`
	IsConference bool                `json:"isConference"`
	Children     []espnOverviewGroup `json:"children"`
	Standings    struct {
		Season            int    `json:"season"`
		SeasonType        int    `json:"seasonType"`
		SeasonDisplayName string `json:"seasonDisplayName"`
		Entries           []struct {
			Team  espnOverviewTeamPayload `json:"team"`
			Stats []espnOverviewStat      `json:"stats"`
		} `json:"entries"`
	} `json:"standings"`
}

func (a *ESPNAdapter) overviewBase(league LeagueID) string {
	if league == LeagueNBA {
		return strings.TrimSuffix(a.NBABaseURL, "/scoreboard")
	}
	return strings.TrimSuffix(a.NFLBaseURL, "/scoreboard")
}
func (a *ESPNAdapter) Overview(ctx context.Context, req SportsScheduleRequest) (SportsOverview, error) {
	if err := req.Validate(); err != nil {
		return SportsOverview{}, err
	}
	if req.League != LeagueNBA && req.League != LeagueNFL {
		return SportsOverview{}, errorsForUnsupportedLeague()
	}
	catalog := nbaTeams
	if req.League == LeagueNFL {
		catalog = nflTeams
	}
	team, ok := catalog[req.TeamID]
	if !ok {
		return SportsOverview{}, UnknownSportsTeam(req.League, req.TeamID)
	}
	table, ts, te := a.OverviewCache.Standings.GetWithPolicy(ctx, string(req.League), func(ctx context.Context) (overviewTable, CachePolicy, error) {
		t, e := a.fetchOverviewStandings(ctx, req.League)
		return t, overviewPolicy(t.Phase, OverviewStandingsTTL), e
	})
	schedule, ss, se := a.OverviewCache.Schedules.GetWithPolicy(ctx, string(req.League)+":"+string(req.TeamID), func(ctx context.Context) (overviewSchedule, CachePolicy, error) {
		s, e := a.fetchOverviewSchedule(ctx, req.League, req.TeamID, table.Phase)
		return s, overviewPolicy(s.Phase, OverviewScheduleTTL), e
	})
	if te != nil && se != nil {
		return SportsOverview{League: req.League, Team: team, Season: OverviewSeason{Phase: "unknown"}}, nil
	}
	return assembleOverview(team, table, schedule, ts || ss || te != nil || se != nil, a.now(), req.Timezone), nil
}
func (a *ESPNAdapter) fetchOverviewStandings(ctx context.Context, league LeagueID) (overviewTable, error) {
	var p struct {
		Children []espnOverviewGroup `json:"children"`
		Season   struct {
			StartDate string `json:"startDate"`
			EndDate   string `json:"endDate"`
		} `json:"season"`
	}
	endpoint := strings.Replace(a.overviewBase(league), "/apis/site/v2/", "/apis/v2/", 1) + "/standings"
	if err := overviewJSON(ctx, a.Client, endpoint, &p); err != nil {
		return overviewTable{}, err
	}
	if p.Children == nil || len(p.Children) > 16 {
		return overviewTable{}, SportsUnavailable()
	}
	t := overviewTable{Rows: map[ProviderTeamID]overviewRow{}, FetchedAt: a.now()}
	off := false
	if start, ok := parseESPNTime(p.Season.StartDate); ok && a.now().Before(start) {
		off = true
	}
	if end, ok := parseESPNTime(p.Season.EndDate); ok && a.now().After(end) {
		off = true
	}
	var visit func(espnOverviewGroup, int)
	visit = func(g espnOverviewGroup, depth int) {
		if depth > 3 || len(g.Standings.Entries) > 64 {
			return
		}
		phase := espnOverviewPhase(g.Standings.SeasonType)
		if off {
			phase = "offseason"
		}
		if t.Phase == "" {
			t.Phase = phase
		}
		for _, entry := range g.Standings.Entries {
			stats := map[string]int{}
			for _, stat := range entry.Stats {
				if stat.Value != nil && !math.IsNaN(*stat.Value) && !math.IsInf(*stat.Value, 0) && *stat.Value >= 0 && *stat.Value <= 200 && math.Trunc(*stat.Value) == *stat.Value {
					stats[stat.Name] = int(*stat.Value)
				}
			}
			row := overviewRow{LogoURL: entry.Team.logoURL(), Season: OverviewSeason{Label: overviewSeasonLabel(g.Standings.SeasonDisplayName), Phase: phase}}
			w, wok := stats["wins"]
			l, lok := stats["losses"]
			if wok && lok && g.Standings.SeasonType != 1 {
				row.Season.Record = overviewRecord(league, w, l, stats["ties"])
			}
			conference := map[string]string{"East": "EAST", "West": "WEST", "EAST": "EAST", "WEST": "WEST", "AFC": "AFC", "NFC": "NFC"}[g.Abbreviation]
			if g.IsConference {
				row.Standing.Conference = conference
				row.Standing.Seed = validRank(stats["playoffSeed"], 16)
			}
			row.Standing.ConferenceRank = validRank(stats["conferenceRank"], 16)
			if !g.IsConference {
				row.Standing.Division = conference
				row.Standing.DivisionRank = validRank(stats["divisionRank"], 8)
			}
			t.Rows[ProviderTeamID(entry.Team.ID)] = row
		}
		for _, child := range g.Children {
			visit(child, depth+1)
		}
	}
	for _, g := range p.Children {
		visit(g, 0)
	}
	return t, nil
}
func espnOverviewPhase(n int) string {
	return map[int]string{1: "preseason", 2: "regular", 3: "playoffs", 4: "offseason"}[n]
}

type espnOverviewSchedulePayload struct {
	Events []json.RawMessage `json:"events"`
	Season struct {
		Year        int    `json:"year"`
		Type        int    `json:"type"`
		DisplayName string `json:"displayName"`
	} `json:"season"`
	Team struct {
		StandingSummary string `json:"standingSummary"`
	} `json:"team"`
}

var overviewDivision = regexp.MustCompile(`^([1-4])(?:st|nd|rd|th) in (AFC|NFC) (East|West|North|South)$`)

func (a *ESPNAdapter) fetchOverviewSchedule(ctx context.Context, league LeagueID, team ProviderTeamID, tablePhase string) (overviewSchedule, error) {
	endpoint := a.overviewBase(league) + "/teams/" + url.PathEscape(string(team)) + "/schedule"
	var p espnOverviewSchedulePayload
	if err := overviewJSON(ctx, a.Client, endpoint, &p); err != nil {
		return overviewSchedule{}, err
	}
	if p.Events == nil || len(p.Events) > 200 {
		return overviewSchedule{}, SportsUnavailable()
	}
	s := overviewSchedule{Season: overviewSeasonLabel(p.Season.DisplayName), Phase: espnOverviewPhase(p.Season.Type), FetchedAt: a.now()}
	if tablePhase == "offseason" {
		s.Phase = "offseason"
	}
	if match := overviewDivision.FindStringSubmatch(p.Team.StandingSummary); match != nil {
		s.DivisionRank, _ = strconv.Atoi(match[1])
		s.Division = match[2] + " " + strings.ToUpper(match[3][:1])
	}
	add := func(events []json.RawMessage, phase int) {
		for _, raw := range events {
			if game, ok := normalizeOverviewESPNEvent(raw, league, a.now()); ok {
				game.GameType = espnOverviewPhase(phase)
				s.Games = append(s.Games, game)
			}
		}
	}
	add(p.Events, p.Season.Type)
	// ESPN returns one season type per request. Fetch adjacent types only when
	// needed to find the last completed game; all are covered by the team cache.
	hasFinal := func() bool {
		for _, g := range s.Games {
			if g.Status == GameFinal {
				return true
			}
		}
		return false
	}
	if !hasFinal() && p.Season.Year >= 2000 && p.Season.Year <= 2200 {
		year, phase := p.Season.Year, p.Season.Type-1
		if phase < 1 {
			year--
			phase = 3
		}
		for attempt := 0; attempt < 2 && !hasFinal() && phase >= 1; attempt++ {
			var previous espnOverviewSchedulePayload
			if overviewJSON(ctx, a.Client, fmt.Sprintf("%s?season=%d&seasontype=%d", endpoint, year, phase), &previous) == nil && len(previous.Events) <= 200 {
				add(previous.Events, phase)
			}
			phase--
		}
	}
	sortGames(s.Games)
	return s, nil
}

// Schedule scores are objects and status is inside competitions; scoreboard
// scores are strings and status is top-level. Translate just that boundary,
// then reuse the existing NBA/NFL normalizers, including OT and tie semantics.
func normalizeOverviewESPNEvent(body json.RawMessage, league LeagueID, now time.Time) (Game, bool) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return Game{}, false
	}
	var competitions []map[string]json.RawMessage
	if json.Unmarshal(raw["competitions"], &competitions) != nil || len(competitions) != 1 {
		return Game{}, false
	}
	c := competitions[0]
	raw["status"] = c["status"]
	var people []map[string]json.RawMessage
	if json.Unmarshal(c["competitors"], &people) != nil || len(people) != 2 {
		return Game{}, false
	}
	for _, p := range people {
		var team espnOverviewTeamPayload
		if json.Unmarshal(p["team"], &team) == nil {
			team.Logo = team.logoURL()
			p["team"], _ = json.Marshal(team.espnTeamPayload)
		}
		value := ""
		if score := p["score"]; len(score) > 0 && string(score) != "null" {
			if json.Unmarshal(score, &value) != nil {
				var obj struct {
					Value *float64 `json:"value"`
				}
				if json.Unmarshal(score, &obj) != nil || obj.Value == nil || *obj.Value < 0 || *obj.Value > 999 || math.Trunc(*obj.Value) != *obj.Value {
					return Game{}, false
				}
				value = strconv.Itoa(int(*obj.Value))
			}
			n, e := strconv.Atoi(value)
			if e != nil || n < 0 || n > 999 {
				return Game{}, false
			}
		}
		p["score"], _ = json.Marshal(value)
	}
	c["competitors"], _ = json.Marshal(people)
	raw["competitions"], _ = json.Marshal(competitions)
	translated, _ := json.Marshal(raw)
	var event espnEventPayload
	if json.Unmarshal(translated, &event) != nil {
		return Game{}, false
	}
	if event.Status.Type.State == "post" {
		for _, p := range event.Competitions[0].Competitors {
			if p.Score == "" {
				return Game{}, false
			}
		}
	}
	var g Game
	var ok bool
	if league == LeagueNBA {
		g, ok = normalizeNBAGame(event, now)
	} else {
		g, ok = normalizeNFLGame(event, now)
	}
	var timeValid *bool
	_ = json.Unmarshal(c["timeValid"], &timeValid)
	if timeValid != nil && !*timeValid && g.Status != GameFinal {
		g.StatusDetail = "TIME TBD"
	}
	return g, ok
}

var overviewSeasonPattern = regexp.MustCompile(`^20[0-9]{2}(?:-[0-9]{2})?$`)

func overviewSeasonLabel(value string) string {
	if overviewSeasonPattern.MatchString(value) {
		return value
	}
	return ""
}

type espnOverviewTeamPayload struct {
	espnTeamPayload
	Logos []struct {
		Href string `json:"href"`
	} `json:"logos"`
}

func (t espnOverviewTeamPayload) logoURL() string {
	if allowedSportsLogoURL(t.Logo) {
		return t.Logo
	}
	for _, logo := range t.Logos {
		if allowedSportsLogoURL(logo.Href) {
			return logo.Href
		}
	}
	return ""
}
