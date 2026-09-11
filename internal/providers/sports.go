package providers

import (
	"context"
	"errors"
	"strings"
	"time"
)

type LeagueID string
type ProviderID string
type ProviderTeamID string
type CanonicalTeamID string
type GameID string

const (
	LeagueNHL      LeagueID   = "nhl"
	LeagueCFL      LeagueID   = "cfl"
	LeagueNBA      LeagueID   = "nba"
	ProviderNHLWeb ProviderID = "nhl-web"
	ProviderESPN   ProviderID = "espn-site"
)

func NewCanonicalTeamID(provider ProviderID, league LeagueID, teamID ProviderTeamID) CanonicalTeamID {
	return CanonicalTeamID(string(provider) + ":" + string(league) + ":" + string(teamID))
}

func NewGameID(provider ProviderID, league LeagueID, providerGameID string) GameID {
	return GameID(string(provider) + ":" + string(league) + ":" + providerGameID)
}

type GameStatus string

const (
	GameScheduled    GameStatus = "scheduled"
	GamePregame      GameStatus = "pregame"
	GameLive         GameStatus = "live"
	GameIntermission GameStatus = "intermission"
	GameDelayed      GameStatus = "delayed"
	GameSuspended    GameStatus = "suspended"
	GamePostponed    GameStatus = "postponed"
	GameCancelled    GameStatus = "cancelled"
	GameFinal        GameStatus = "final"
	GameUnknown      GameStatus = "unknown"
)

func (status GameStatus) Active() bool {
	return status == GameLive || status == GameIntermission
}

type Team struct {
	ID              CanonicalTeamID `json:"id"`
	ProviderID      ProviderTeamID  `json:"providerId"`
	League          LeagueID        `json:"league"`
	DisplayName     string          `json:"displayName"`
	ShortName       string          `json:"shortName"`
	Abbreviation    string          `json:"abbreviation"`
	ProviderLogoURL string          `json:"providerLogoURL,omitempty"`
	PrimaryColor    string          `json:"primaryColor,omitempty"`
	SecondaryColor  string          `json:"secondaryColor,omitempty"`
}

type Game struct {
	ID             GameID     `json:"id"`
	ProviderGameID string     `json:"providerGameId"`
	League         LeagueID   `json:"league"`
	Provider       ProviderID `json:"provider"`
	AwayTeam       Team       `json:"awayTeam"`
	HomeTeam       Team       `json:"homeTeam"`
	AwayScore      int        `json:"awayScore"`
	HomeScore      int        `json:"homeScore"`
	ScheduledAt    time.Time  `json:"scheduledAt"`
	ScheduledLocal string     `json:"scheduledLocal,omitempty"`
	ProviderState  string     `json:"providerState"`
	Status         GameStatus `json:"status"`
	Period         int        `json:"period,omitempty"`
	PeriodLabel    string     `json:"periodLabel,omitempty"`
	Clock          string     `json:"clock,omitempty"`
	StatusDetail   string     `json:"statusDetail,omitempty"`
	Overtime       bool       `json:"overtime"`
	Shootout       bool       `json:"shootout"`
	FreshAsOf      time.Time  `json:"freshAsOf"`
	Stale          bool       `json:"stale"`
	AwayRecord     string     `json:"awayRecord,omitempty"`
	HomeRecord     string     `json:"homeRecord,omitempty"`
}

type SportsSnapshot struct {
	League         LeagueID   `json:"league"`
	Provider       ProviderID `json:"provider"`
	DeviceTimezone string     `json:"deviceTimezone"`
	Games          []Game     `json:"games"`
	NextGame       *Game      `json:"nextGame,omitempty"`
	UpcomingGames  []Game     `json:"upcomingGames,omitempty"`
	FreshAsOf      time.Time  `json:"freshAsOf"`
	Stale          bool       `json:"stale"`
}

type SportsScheduleRequest struct {
	League   LeagueID
	TeamID   ProviderTeamID
	Timezone string
	Limit    int
}

func (request SportsScheduleRequest) Validate() error {
	if strings.TrimSpace(string(request.League)) == "" || strings.TrimSpace(string(request.TeamID)) == "" {
		return errors.New("sports schedule request is invalid")
	}
	if _, err := time.LoadLocation(request.Timezone); err != nil {
		return errors.New("sports schedule timezone is invalid")
	}
	return nil
}

type SportsLiveRequest struct {
	League   LeagueID
	Timezone string
}

func (request SportsLiveRequest) Validate() error {
	if strings.TrimSpace(string(request.League)) == "" {
		return errors.New("sports live request is invalid")
	}
	if _, err := time.LoadLocation(request.Timezone); err != nil {
		return errors.New("sports live timezone is invalid")
	}
	return nil
}

type SportsProvider interface {
	Teams(context.Context, LeagueID) ([]Team, error)
	Schedule(context.Context, SportsScheduleRequest) (SportsSnapshot, error)
	LiveGames(context.Context, SportsLiveRequest) (SportsSnapshot, error)
}

func SportsUnavailable() error {
	return SanitizedError{Code: "sports_provider_unavailable", Message: "Sports data is temporarily unavailable", Retryable: true}
}

func UnknownSportsTeam(league LeagueID, _ ProviderTeamID) error {
	return SanitizedError{Code: "sports_team_invalid", Message: strings.ToUpper(string(league)) + " team selection is not available", Retryable: false}
}

// SportsRegistry keeps league routing out of rendering and provider adapters.
// Adding another league does not require a multi-sport provider abstraction or
// expose one provider's response model to another.
type SportsRegistry struct {
	providers map[LeagueID]SportsProvider
}

func NewSportsRegistry(values map[LeagueID]SportsProvider) *SportsRegistry {
	providers := make(map[LeagueID]SportsProvider, len(values))
	for league, provider := range values {
		if provider != nil {
			providers[league] = provider
		}
	}
	return &SportsRegistry{providers: providers}
}

func (registry *SportsRegistry) provider(league LeagueID) (SportsProvider, error) {
	if registry == nil || registry.providers[league] == nil {
		return nil, SanitizedError{Code: "sports_league_unsupported", Message: "The sports league is not supported", Retryable: false}
	}
	return registry.providers[league], nil
}

func (registry *SportsRegistry) Teams(ctx context.Context, league LeagueID) ([]Team, error) {
	provider, err := registry.provider(league)
	if err != nil {
		return nil, err
	}
	return provider.Teams(ctx, league)
}

func (registry *SportsRegistry) Schedule(ctx context.Context, request SportsScheduleRequest) (SportsSnapshot, error) {
	provider, err := registry.provider(request.League)
	if err != nil {
		return SportsSnapshot{}, err
	}
	return provider.Schedule(ctx, request)
}

func (registry *SportsRegistry) LiveGames(ctx context.Context, request SportsLiveRequest) (SportsSnapshot, error) {
	provider, err := registry.provider(request.League)
	if err != nil {
		return SportsSnapshot{}, err
	}
	return provider.LiveGames(ctx, request)
}
