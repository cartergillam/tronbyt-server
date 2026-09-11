package providers

import (
	"context"
	"errors"
	"fmt"
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
	ProviderNHLWeb ProviderID = "nhl-web"
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
}

type SportsSnapshot struct {
	League         LeagueID   `json:"league"`
	Provider       ProviderID `json:"provider"`
	DeviceTimezone string     `json:"deviceTimezone"`
	Games          []Game     `json:"games"`
	NextGame       *Game      `json:"nextGame,omitempty"`
	FreshAsOf      time.Time  `json:"freshAsOf"`
	Stale          bool       `json:"stale"`
}

type SportsScheduleRequest struct {
	League   LeagueID
	TeamID   ProviderTeamID
	Timezone string
}

func (request SportsScheduleRequest) Validate() error {
	if request.League != LeagueNHL || strings.TrimSpace(string(request.TeamID)) == "" {
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
	if request.League != LeagueNHL {
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
	return SanitizedError{Code: "sports_provider_unavailable", Message: "NHL data is temporarily unavailable", Retryable: true}
}

func UnknownSportsTeam(teamID ProviderTeamID) error {
	return SanitizedError{Code: "sports_team_invalid", Message: fmt.Sprintf("NHL team %s is not available", teamID), Retryable: false}
}
