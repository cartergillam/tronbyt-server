package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	OverviewStandingsTTL = 30 * time.Minute
	OverviewScheduleTTL  = 10 * time.Minute
	OverviewOffseasonTTL = 6 * time.Hour
	OverviewLKGTTL       = 24 * time.Hour
)

type SeasonRecord struct {
	Wins           int    `json:"wins"`
	Losses         int    `json:"losses"`
	OvertimeLosses *int   `json:"overtimeLosses,omitempty"`
	Ties           *int   `json:"ties,omitempty"`
	GamesPlayed    int    `json:"gamesPlayed"`
	Display        string `json:"display"`
}
type OverviewStanding struct {
	Division       string `json:"division,omitempty"`
	Conference     string `json:"conference,omitempty"`
	DivisionRank   int    `json:"divisionRank,omitempty"`
	ConferenceRank int    `json:"conferenceRank,omitempty"`
	Seed           int    `json:"seed,omitempty"`
	Wildcard       int    `json:"wildcard,omitempty"`
	Clinched       bool   `json:"clinched,omitempty"`
	Label          string `json:"label,omitempty"`
}
type OverviewSeason struct {
	Label  string        `json:"label,omitempty"`
	Phase  string        `json:"phase"`
	Record *SeasonRecord `json:"record,omitempty"`
}

// OverviewGame wraps the existing Game instead of duplicating team/opponent,
// score, home/away, final state, identity or timestamp contracts.
type OverviewGame struct {
	Game
	Result     string `json:"result,omitempty"`
	DateLabel  string `json:"dateLabel,omitempty"`
	TimeLabel  string `json:"timeLabel,omitempty"`
	FinalLabel string `json:"finalLabel,omitempty"`
}
type SportsOverview struct {
	League    LeagueID          `json:"league"`
	Team      Team              `json:"team"`
	Season    OverviewSeason    `json:"season"`
	Standing  *OverviewStanding `json:"standing,omitempty"`
	NextGame  *OverviewGame     `json:"nextGame,omitempty"`
	LastGame  *OverviewGame     `json:"lastGame,omitempty"`
	FetchedAt time.Time         `json:"fetchedAt"`
	Stale     bool              `json:"stale"`
}
type SportsOverviewProvider interface {
	Overview(context.Context, SportsScheduleRequest) (SportsOverview, error)
}
type overviewRow struct {
	LogoURL  string
	Season   OverviewSeason
	Standing OverviewStanding
}
type overviewTable struct {
	Rows      map[ProviderTeamID]overviewRow
	FetchedAt time.Time
	Phase     string
}
type overviewSchedule struct {
	Games        []Game
	Phase        string
	Season       string
	Division     string
	DivisionRank int
	FetchedAt    time.Time
}
type overviewCaches struct {
	Standings *Cache[overviewTable]
	Schedules *Cache[overviewSchedule]
}

func newOverviewCaches() overviewCaches {
	return overviewCaches{NewCache[overviewTable](5 * time.Minute), NewCache[overviewSchedule](5 * time.Minute)}
}

func (registry *SportsRegistry) Overview(ctx context.Context, req SportsScheduleRequest) (SportsOverview, error) {
	p, err := registry.provider(req.League)
	if err != nil {
		return SportsOverview{}, err
	}
	overview, ok := p.(SportsOverviewProvider)
	if !ok {
		return SportsOverview{}, errorsForUnsupportedLeague()
	}
	return overview.Overview(ctx, req)
}
func overviewRecord(league LeagueID, w, l, extra int) *SeasonRecord {
	if w < 0 || l < 0 || extra < 0 || w > 200 || l > 200 || extra > 100 {
		return nil
	}
	r := &SeasonRecord{Wins: w, Losses: l, GamesPlayed: w + l, Display: fmt.Sprintf("%d-%d", w, l)}
	if league == LeagueNHL {
		r.OvertimeLosses = &extra
		r.GamesPlayed += extra
		r.Display += fmt.Sprintf("-%d", extra)
	}
	if league == LeagueNFL {
		r.Ties = &extra
		r.GamesPlayed += extra
		if extra > 0 {
			r.Display += fmt.Sprintf("-%d", extra)
		}
	}
	return r
}
func assembleOverview(team Team, table overviewTable, schedule overviewSchedule, stale bool, now time.Time, tz string) SportsOverview {
	row := table.Rows[team.ProviderID]
	team.ProviderLogoURL = row.LogoURL
	r := SportsOverview{League: team.League, Team: team, Season: row.Season, FetchedAt: table.FetchedAt, Stale: stale}
	if r.FetchedAt.IsZero() || (!schedule.FetchedAt.IsZero() && schedule.FetchedAt.Before(r.FetchedAt)) {
		r.FetchedAt = schedule.FetchedAt
	}
	if schedule.Phase != "" {
		r.Season.Phase = schedule.Phase
	}
	if r.Season.Phase == "" {
		r.Season.Phase = table.Phase
	}
	if r.Season.Phase == "" {
		r.Season.Phase = "unknown"
	}
	// Never label a previous season's record as the newly scheduled season.
	if r.Season.Label == "" {
		r.Season.Label = schedule.Season
	}
	if r.Season.Phase != "offseason" && schedule.Season != "" && row.Season.Label != "" && schedule.Season != row.Season.Label {
		r.Season.Record = nil
		r.Season.Label = schedule.Season
	}
	if r.Season.Phase == "preseason" {
		r.Season.Record = nil
	}
	if r.Season.Record != nil && r.Season.Record.GamesPlayed > 0 {
		standing := row.Standing
		if team.League == LeagueNFL && schedule.DivisionRank > 0 {
			standing.Division = schedule.Division
			standing.DivisionRank = schedule.DivisionRank
		}
		if r.Season.Phase == "regular" || r.Season.Phase == "playoffs" {
			standing.Label = standingLabel(team.League, standing)
			r.Standing = &standing
		}
	}
	for _, g := range schedule.Games {
		if g.AwayTeam.ProviderID != team.ProviderID && g.HomeTeam.ProviderID != team.ProviderID {
			continue
		}
		if g.Status == GameFinal && !g.ScheduledAt.After(now) && g.AwayScore >= 0 && g.HomeScore >= 0 && g.AwayScore <= 999 && g.HomeScore <= 999 {
			if r.LastGame == nil || g.ScheduledAt.After(r.LastGame.ScheduledAt) {
				v := overviewGame(g, team.ProviderID, tz)
				r.LastGame = &v
			}
		} else if (g.Status == GameScheduled || g.Status == GamePregame || g.Status == GameDelayed || g.Status == GamePostponed) && !g.ScheduledAt.Before(now.Add(-6*time.Hour)) {
			if r.NextGame == nil || g.ScheduledAt.Before(r.NextGame.ScheduledAt) {
				v := overviewGame(g, team.ProviderID, tz)
				r.NextGame = &v
			}
		}
	}
	return r
}
func standingLabel(league LeagueID, s OverviewStanding) string {
	if s.DivisionRank > 0 && (league == LeagueNHL || league == LeagueNFL) {
		return fmt.Sprintf("#%d %s", s.DivisionRank, s.Division)
	}
	if s.ConferenceRank > 0 {
		return fmt.Sprintf("#%d %s", s.ConferenceRank, s.Conference)
	}
	if s.Seed > 0 {
		return fmt.Sprintf("%s SEED %d", s.Conference, s.Seed)
	}
	return ""
}
func overviewGame(g Game, favorite ProviderTeamID, tz string) OverviewGame {
	r := OverviewGame{Game: g}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	if !g.ScheduledAt.IsZero() {
		local := g.ScheduledAt.In(loc)
		r.ScheduledLocal = local.Format(time.RFC3339)
		r.DateLabel = strings.ToUpper(local.Format("Jan 2"))
		r.TimeLabel = local.Format("3:04PM")
		if g.StatusDetail == "TIME TBD" {
			r.TimeLabel = "TBD"
		}
	}
	if g.Status == GameFinal {
		a, b := g.HomeScore, g.AwayScore
		if g.AwayTeam.ProviderID == favorite {
			a, b = b, a
		}
		r.Result = "W"
		if a < b {
			r.Result = "L"
		} else if a == b {
			r.Result = "T"
		}
		r.FinalLabel = "FINAL"
		if g.Shootout {
			r.FinalLabel = "FINAL/SO"
		} else if g.Overtime {
			r.FinalLabel = "FINAL/OT"
			if g.League == LeagueNBA && g.Period > 5 {
				r.FinalLabel = fmt.Sprintf("FINAL/%dOT", g.Period-4)
			}
		}
	}
	return r
}

// Reuse the existing hydrator and its seven-day cache, including identity-only
// pages. Temporary snapshot containers never enter the render contract.
func (r SportsOverview) WithLogos(ctx context.Context, h SportsLogoHydrator) SportsOverview {
	if h == nil {
		return r
	}
	if r.Team.ProviderLogoURL == "" {
		for _, game := range []*OverviewGame{r.NextGame, r.LastGame} {
			if game == nil {
				continue
			}
			for _, team := range []Team{game.HomeTeam, game.AwayTeam} {
				if team.ID == r.Team.ID && team.ProviderLogoURL != "" {
					r.Team.ProviderLogoURL = team.ProviderLogoURL
				}
			}
		}
	}
	games := []Game{{HomeTeam: r.Team}}
	if r.NextGame != nil {
		games = append(games, r.NextGame.Game)
	}
	if r.LastGame != nil {
		games = append(games, r.LastGame.Game)
	}
	hydrated := h.Hydrate(ctx, SportsSnapshot{League: r.League, Games: games})
	if len(hydrated.Games) != len(games) {
		return r
	}
	r.Team = hydrated.Games[0].HomeTeam
	i := 1
	if r.NextGame != nil {
		v := *r.NextGame
		v.Game = hydrated.Games[i]
		r.NextGame = &v
		i++
	}
	if r.LastGame != nil {
		v := *r.LastGame
		v.Game = hydrated.Games[i]
		r.LastGame = &v
	}
	return r
}
func overviewJSON(ctx context.Context, client *http.Client, endpoint string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return SportsUnavailable()
	}
	resp, err := client.Do(req)
	if err != nil {
		return SportsUnavailable()
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return SportsUnavailable()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	if err != nil || len(body) > 2<<20 {
		return SportsUnavailable()
	}
	if json.Unmarshal(body, target) != nil {
		return SportsUnavailable()
	}
	return nil
}
func overviewPolicy(phase string, ttl time.Duration) CachePolicy {
	if phase == "offseason" {
		ttl = OverviewOffseasonTTL
	}
	return CachePolicy{FreshTTL: ttl, StaleTTL: OverviewLKGTTL}
}
