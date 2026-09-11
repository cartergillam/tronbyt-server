package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	espnLiveFreshTTL       = 15 * time.Second
	espnPregameFreshTTL    = time.Minute
	espnScheduledFreshTTL  = 5 * time.Minute
	espnOffDayFreshTTL     = 20 * time.Minute
	espnFinalFreshTTL      = 5 * time.Minute
	espnLiveStaleTTL       = 2 * time.Minute
	espnScheduleStaleTTL   = 45 * time.Minute
	espnMaximumBodyBytes   = 2 << 20
	espnMinimumFetchPeriod = 10 * time.Second
)

// ESPNAdapter contains the small portion of ESPN's site-scoreboard response
// needed by CFL Live. League paths and state translation remain replaceable;
// normalized sports data is the only contract exposed to renderers.
type ESPNAdapter struct {
	Client  *http.Client
	BaseURL string
	Cache   *Cache[SportsSnapshot]
	Now     func() time.Time
}

func NewESPNAdapter(client *http.Client) *ESPNAdapter {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &ESPNAdapter{
		Client: client, BaseURL: "https://site.api.espn.com/apis/site/v2/sports/football/cfl/scoreboard",
		Cache: NewCache[SportsSnapshot](espnMinimumFetchPeriod), Now: time.Now,
	}
}

func (adapter *ESPNAdapter) Teams(_ context.Context, league LeagueID) ([]Team, error) {
	if league != LeagueCFL {
		return nil, errorsForUnsupportedLeague()
	}
	teams := make([]Team, 0, len(cflTeams))
	for _, team := range cflTeams {
		teams = append(teams, team)
	}
	sort.Slice(teams, func(i, j int) bool { return teams[i].DisplayName < teams[j].DisplayName })
	return teams, nil
}

func (adapter *ESPNAdapter) Schedule(ctx context.Context, request SportsScheduleRequest) (SportsSnapshot, error) {
	if err := request.Validate(); err != nil {
		return SportsSnapshot{}, err
	}
	if request.League != LeagueCFL {
		return SportsSnapshot{}, errorsForUnsupportedLeague()
	}
	if _, ok := cflTeams[request.TeamID]; !ok {
		return SportsSnapshot{}, UnknownSportsTeam(LeagueCFL, request.TeamID)
	}
	limit := request.Limit
	if limit < 1 {
		limit = 1
	}
	if limit > 3 {
		limit = 3
	}
	now := adapter.now().UTC()
	location, _ := time.LoadLocation(request.Timezone)
	localDate := now.In(location).Format("20060102")
	key := "espn:cfl:schedule:" + string(request.TeamID) + ":" + request.Timezone + ":" + localDate + ":" + strconv.Itoa(limit)
	snapshot, stale, err := adapter.Cache.GetWithPolicy(ctx, key, func(ctx context.Context) (SportsSnapshot, CachePolicy, error) {
		currentNow := now
		localNow := currentNow.In(location)
		games, err := adapter.fetchCFLGames(ctx, localNow.AddDate(0, 0, -1), localNow.AddDate(0, 0, 180))
		if err != nil {
			return SportsSnapshot{}, CachePolicy{}, err
		}
		selected := selectTeamGames(games, request.TeamID, request.Timezone, currentNow)
		upcoming := []Game{}
		if len(selected) == 0 {
			upcoming = selectUpcomingTeamGames(games, request.TeamID, currentNow, limit)
		}
		var next *Game
		if len(upcoming) > 0 {
			copy := upcoming[0]
			next = &copy
		} else {
			next = selectNextTeamGame(games, request.TeamID, currentNow)
		}
		result := newSportsSnapshot(request.Timezone, selected, next, currentNow)
		result.UpcomingGames = upcoming
		return result, espnCachePolicy(result), nil
	})
	if err != nil {
		return SportsSnapshot{}, err
	}
	return markSportsSnapshotStale(snapshot, stale), nil
}

func (adapter *ESPNAdapter) LiveGames(ctx context.Context, request SportsLiveRequest) (SportsSnapshot, error) {
	if err := request.Validate(); err != nil {
		return SportsSnapshot{}, err
	}
	if request.League != LeagueCFL {
		return SportsSnapshot{}, errorsForUnsupportedLeague()
	}
	now := adapter.now().UTC()
	location, _ := time.LoadLocation(request.Timezone)
	localDate := now.In(location).Format("20060102")
	key := "espn:cfl:live:" + request.Timezone + ":" + localDate
	snapshot, stale, err := adapter.Cache.GetWithPolicy(ctx, key, func(ctx context.Context) (SportsSnapshot, CachePolicy, error) {
		currentNow := now
		localNow := currentNow.In(location)
		games, err := adapter.fetchCFLGames(ctx, localNow.AddDate(0, 0, -1), localNow.AddDate(0, 0, 2))
		if err != nil {
			return SportsSnapshot{}, CachePolicy{}, err
		}
		live := make([]Game, 0, len(games))
		seen := make(map[GameID]bool, len(games))
		for _, game := range games {
			if game.Status.Active() && !seen[game.ID] {
				seen[game.ID] = true
				live = append(live, game)
			}
		}
		sortGames(live)
		result := newSportsSnapshot(request.Timezone, live, selectNextGame(games, currentNow), currentNow)
		return result, espnCachePolicy(result), nil
	})
	if err != nil {
		return SportsSnapshot{}, err
	}
	return markSportsSnapshotStale(snapshot, stale), nil
}

func (adapter *ESPNAdapter) now() time.Time {
	if adapter.Now != nil {
		return adapter.Now()
	}
	return time.Now()
}

type espnTeamPayload struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	ShortDisplayName string `json:"shortDisplayName"`
	Abbreviation     string `json:"abbreviation"`
	Color            string `json:"color"`
	AlternateColor   string `json:"alternateColor"`
	Logo             string `json:"logo"`
}

type espnCompetitorPayload struct {
	ID       string          `json:"id"`
	HomeAway string          `json:"homeAway"`
	Score    string          `json:"score"`
	Team     espnTeamPayload `json:"team"`
	Records  []struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		Summary string `json:"summary"`
	} `json:"records"`
}

type espnEventPayload struct {
	ID     string `json:"id"`
	Date   string `json:"date"`
	Status struct {
		Period       int    `json:"period"`
		DisplayClock string `json:"displayClock"`
		Type         struct {
			State       string `json:"state"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Detail      string `json:"detail"`
			ShortDetail string `json:"shortDetail"`
			Completed   bool   `json:"completed"`
		} `json:"type"`
	} `json:"status"`
	Competitions []struct {
		Competitors []espnCompetitorPayload `json:"competitors"`
	} `json:"competitions"`
}

func (adapter *ESPNAdapter) fetchCFLGames(ctx context.Context, from, through time.Time) ([]Game, error) {
	endpoint, err := url.Parse(adapter.BaseURL)
	if err != nil {
		return nil, SportsUnavailable()
	}
	query := endpoint.Query()
	query.Set("limit", "200")
	query.Set("dates", from.Format("20060102")+"-"+through.Format("20060102"))
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, SportsUnavailable()
	}
	resp, err := adapter.Client.Do(req)
	if err != nil {
		return nil, SportsUnavailable()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, SportsUnavailable()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, espnMaximumBodyBytes+1))
	if err != nil || len(body) > espnMaximumBodyBytes {
		return nil, SportsUnavailable()
	}
	var payload struct {
		Events []espnEventPayload `json:"events"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, SanitizedError{Code: "sports_response_invalid", Message: "CFL data could not be read", Retryable: true}
	}
	fetchedAt := adapter.now().UTC()
	games := make([]Game, 0, len(payload.Events))
	for _, raw := range payload.Events {
		game, ok := normalizeCFLGame(raw, fetchedAt)
		if ok {
			games = append(games, game)
		}
	}
	sortGames(games)
	return games, nil
}

func normalizeCFLGame(raw espnEventPayload, fetchedAt time.Time) (Game, bool) {
	if strings.TrimSpace(raw.ID) == "" || len(raw.Competitions) == 0 || len(raw.Competitions[0].Competitors) < 2 {
		return Game{}, false
	}
	start, ok := parseESPNTime(raw.Date)
	if !ok {
		return Game{}, false
	}
	competitors := raw.Competitions[0].Competitors
	home, away := competitors[0], competitors[1]
	for _, competitor := range competitors {
		switch strings.ToLower(competitor.HomeAway) {
		case "home":
			home = competitor
		case "away":
			away = competitor
		}
	}
	homeTeam := cflTeamFromPayload(home.Team)
	awayTeam := cflTeamFromPayload(away.Team)
	if homeTeam.ID == "" || awayTeam.ID == "" || homeTeam.ID == awayTeam.ID {
		return Game{}, false
	}
	status := normalizeESPNStatus(raw.Status.Type.State, raw.Status.Type.Name, raw.Status.Type.Description, raw.Status.Type.Detail, raw.Status.Type.ShortDetail, raw.Status.Period)
	overtime := raw.Status.Period > 4 || containsAny(strings.ToUpper(raw.Status.Type.Name+" "+raw.Status.Type.Detail+" "+raw.Status.Type.ShortDetail), "OVERTIME", " OT")
	periodLabel := cflPeriodLabel(raw.Status.Period, overtime)
	detail := cflStatusDetail(status, periodLabel, raw.Status.DisplayClock, raw.Status.Type.ShortDetail, overtime)
	providerState := strings.ToUpper(strings.TrimSpace(raw.Status.Type.State + "/" + raw.Status.Type.Name))
	if len(providerState) > 64 {
		providerState = providerState[:64]
	}
	return Game{
		ID: NewGameID(ProviderESPN, LeagueCFL, raw.ID), ProviderGameID: raw.ID, League: LeagueCFL, Provider: ProviderESPN,
		AwayTeam: awayTeam, HomeTeam: homeTeam, AwayScore: parseESPNScore(away.Score), HomeScore: parseESPNScore(home.Score),
		AwayRecord: espnRecord(away), HomeRecord: espnRecord(home), ScheduledAt: start.UTC(), ProviderState: providerState,
		Status: status, Period: raw.Status.Period, PeriodLabel: periodLabel, Clock: strings.TrimSpace(raw.Status.DisplayClock),
		StatusDetail: detail, Overtime: overtime, FreshAsOf: fetchedAt,
	}, true
}

func normalizeESPNStatus(state, name, description, detail, shortDetail string, period int) GameStatus {
	combined := strings.ToUpper(strings.Join([]string{state, name, description, detail, shortDetail}, " "))
	switch {
	case containsAny(combined, "CANCEL"):
		return GameCancelled
	case containsAny(combined, "POSTPON"):
		return GamePostponed
	case containsAny(combined, "SUSPEND"):
		return GameSuspended
	case containsAny(combined, "DELAY"):
		return GameDelayed
	}
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "pre":
		if containsAny(combined, "PREGAME", "WARMUP") {
			return GamePregame
		}
		return GameScheduled
	case "in":
		if containsAny(combined, "HALFTIME", "HALF TIME", "END OF") {
			return GameIntermission
		}
		return GameLive
	case "post":
		return GameFinal
	default:
		if period > 0 && !containsAny(combined, "FINAL") {
			return GameLive
		}
		return GameUnknown
	}
}

func cflTeamFromPayload(raw espnTeamPayload) Team {
	providerID := ProviderTeamID(strings.TrimSpace(raw.ID))
	team, ok := cflTeams[providerID]
	if !ok {
		abbr := strings.ToUpper(strings.TrimSpace(raw.Abbreviation))
		numericID, numericErr := strconv.Atoi(string(providerID))
		if numericErr != nil || numericID <= 0 || abbr == "" {
			return Team{}
		}
		team = makeCFLTeam(providerID, abbr, strings.TrimSpace(raw.DisplayName), strings.TrimSpace(raw.ShortDisplayName), normalizeHexColor(raw.Color, "#333333"), normalizeHexColor(raw.AlternateColor, "#FFFFFF"))
	}
	if strings.HasPrefix(raw.Logo, "https://") {
		team.ProviderLogoURL = raw.Logo
	}
	return team
}

func parseESPNTime(value string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04Z"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func parseESPNScore(value string) int {
	result, _ := strconv.Atoi(strings.TrimSpace(value))
	return result
}

func espnRecord(competitor espnCompetitorPayload) string {
	for _, record := range competitor.Records {
		if strings.EqualFold(record.Type, "total") && strings.TrimSpace(record.Summary) != "" {
			return strings.TrimSpace(record.Summary)
		}
	}
	for _, record := range competitor.Records {
		if strings.TrimSpace(record.Summary) != "" {
			return strings.TrimSpace(record.Summary)
		}
	}
	return ""
}

func selectUpcomingTeamGames(games []Game, teamID ProviderTeamID, now time.Time, limit int) []Game {
	result := make([]Game, 0, limit)
	for _, game := range games {
		if game.AwayTeam.ProviderID != teamID && game.HomeTeam.ProviderID != teamID {
			continue
		}
		if (game.Status == GameScheduled || game.Status == GamePregame) && !game.ScheduledAt.Before(now.Add(-time.Minute)) {
			result = append(result, game)
			if len(result) == limit {
				break
			}
		}
	}
	return result
}

func espnCachePolicy(snapshot SportsSnapshot) CachePolicy {
	for _, game := range snapshot.Games {
		if game.Status.Active() {
			return CachePolicy{FreshTTL: espnLiveFreshTTL, StaleTTL: espnLiveStaleTTL}
		}
	}
	for _, game := range snapshot.Games {
		if game.Status == GamePregame || game.Status == GameDelayed || game.Status == GameSuspended {
			return CachePolicy{FreshTTL: espnPregameFreshTTL, StaleTTL: espnLiveStaleTTL}
		}
	}
	if snapshot.NextGame != nil && snapshot.NextGame.ScheduledAt.After(snapshot.FreshAsOf) && snapshot.NextGame.ScheduledAt.Sub(snapshot.FreshAsOf) < 30*time.Minute {
		return CachePolicy{FreshTTL: espnPregameFreshTTL, StaleTTL: espnLiveStaleTTL}
	}
	for _, game := range snapshot.Games {
		if game.Status == GameFinal || game.Status == GamePostponed || game.Status == GameCancelled {
			return CachePolicy{FreshTTL: espnFinalFreshTTL, StaleTTL: espnScheduleStaleTTL}
		}
	}
	if len(snapshot.Games) > 0 || len(snapshot.UpcomingGames) > 0 {
		return CachePolicy{FreshTTL: espnScheduledFreshTTL, StaleTTL: espnScheduleStaleTTL}
	}
	return CachePolicy{FreshTTL: espnOffDayFreshTTL, StaleTTL: espnScheduleStaleTTL}
}

func cflPeriodLabel(period int, overtime bool) string {
	if overtime {
		return "OT"
	}
	if period > 0 {
		return "Q" + strconv.Itoa(period)
	}
	return ""
}

func cflStatusDetail(status GameStatus, period, clock, providerDetail string, overtime bool) string {
	switch status {
	case GameLive:
		return strings.TrimSpace(period + " " + clock)
	case GameIntermission:
		if containsAny(strings.ToUpper(providerDetail), "HALF") {
			return "HALFTIME"
		}
		return "BREAK " + period
	case GameFinal:
		if overtime {
			return "FINAL/OT"
		}
		return "FINAL"
	case GamePregame:
		return "PREGAME"
	case GameScheduled:
		return "SCHEDULED"
	default:
		return strings.ToUpper(string(status))
	}
}

func normalizeHexColor(value, fallback string) string {
	value = strings.TrimPrefix(strings.TrimSpace(value), "#")
	if len(value) != 6 {
		return fallback
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return fallback
		}
	}
	return "#" + strings.ToUpper(value)
}

func makeCFLTeam(id ProviderTeamID, abbreviation, displayName, shortName, primary, secondary string) Team {
	return Team{ID: NewCanonicalTeamID(ProviderESPN, LeagueCFL, id), ProviderID: id, League: LeagueCFL,
		DisplayName: displayName, ShortName: shortName, Abbreviation: abbreviation, PrimaryColor: primary, SecondaryColor: secondary}
}

var cflTeams = map[ProviderTeamID]Team{
	"79": makeCFLTeam("79", "BC", "BC Lions", "Lions", "#F15A22", "#000000"),
	"80": makeCFLTeam("80", "CGY", "Calgary Stampeders", "Stampeders", "#C8102E", "#FFFFFF"),
	"81": makeCFLTeam("81", "EDM", "Edmonton Elks", "Elks", "#006341", "#FFB81C"),
	"82": makeCFLTeam("82", "HAM", "Hamilton Tiger-Cats", "Tiger-Cats", "#FFB81C", "#000000"),
	"83": makeCFLTeam("83", "MTL", "Montréal Alouettes", "Alouettes", "#002D72", "#D50032"),
	"84": makeCFLTeam("84", "SSK", "Saskatchewan Roughriders", "Roughriders", "#006341", "#FFFFFF"),
	"85": makeCFLTeam("85", "TOR", "Toronto Argonauts", "Argonauts", "#051C3E", "#5BC2E7"),
	"86": makeCFLTeam("86", "WPG", "Winnipeg Blue Bombers", "Blue Bombers", "#1D3D7A", "#C99700"),
	"87": makeCFLTeam("87", "OTT", "Ottawa Redblacks", "Redblacks", "#D71920", "#000000"),
}
