package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	nhlLiveFreshTTL       = 15 * time.Second
	nhlPregameFreshTTL    = time.Minute
	nhlScheduledFreshTTL  = 5 * time.Minute
	nhlOffDayFreshTTL     = 20 * time.Minute
	nhlFinalFreshTTL      = 5 * time.Minute
	nhlLiveStaleTTL       = 2 * time.Minute
	nhlScheduleStaleTTL   = 45 * time.Minute
	nhlMaximumBodyBytes   = 2 << 20
	nhlMinimumFetchPeriod = 10 * time.Second
)

// NHLAdapter translates the public NHL web API into the shared sports
// contract. The adapter is the only layer that understands NHL response
// shapes, state codes, or abbreviation-based endpoint paths.
type NHLAdapter struct {
	Client  *http.Client
	BaseURL string
	Cache   *Cache[SportsSnapshot]
	Now     func() time.Time
}

func NewNHLAdapter(client *http.Client) *NHLAdapter {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &NHLAdapter{
		Client: client, BaseURL: "https://api-web.nhle.com/v1",
		Cache: NewCache[SportsSnapshot](nhlMinimumFetchPeriod), Now: time.Now,
	}
}

func (adapter *NHLAdapter) Teams(_ context.Context, league LeagueID) ([]Team, error) {
	if league != LeagueNHL {
		return nil, errorsForUnsupportedLeague()
	}
	teams := make([]Team, 0, len(nhlTeams))
	for _, team := range nhlTeams {
		teams = append(teams, team)
	}
	sort.Slice(teams, func(i, j int) bool { return teams[i].DisplayName < teams[j].DisplayName })
	return teams, nil
}

func (adapter *NHLAdapter) Schedule(ctx context.Context, request SportsScheduleRequest) (SportsSnapshot, error) {
	if err := request.Validate(); err != nil {
		return SportsSnapshot{}, err
	}
	team, ok := nhlTeams[request.TeamID]
	if !ok {
		return SportsSnapshot{}, UnknownSportsTeam(request.TeamID)
	}
	key := "nhl:schedule:" + string(request.TeamID) + ":" + request.Timezone
	snapshot, stale, err := adapter.Cache.GetWithPolicy(ctx, key, func(ctx context.Context) (SportsSnapshot, CachePolicy, error) {
		games, err := adapter.fetchGames(ctx, "/score/now")
		if err != nil {
			return SportsSnapshot{}, CachePolicy{}, err
		}
		now := adapter.now().UTC()
		selected := selectTeamGames(games, request.TeamID, request.Timezone, now)
		var next *Game
		if !hasCurrentOrFuture(selected, now) {
			seasonGames, scheduleErr := adapter.fetchGames(ctx, "/club-schedule-season/"+url.PathEscape(team.Abbreviation)+"/now")
			if scheduleErr != nil && len(selected) == 0 {
				return SportsSnapshot{}, CachePolicy{}, scheduleErr
			}
			candidate := selectNextTeamGame(seasonGames, request.TeamID, now)
			if candidate != nil {
				next = candidate
			}
		} else {
			next = selectNextGame(selected, now)
		}
		result := newSportsSnapshot(request.Timezone, selected, next, now)
		return result, sportsCachePolicy(result), nil
	})
	if err != nil {
		return SportsSnapshot{}, err
	}
	return markSportsSnapshotStale(snapshot, stale), nil
}

func (adapter *NHLAdapter) LiveGames(ctx context.Context, request SportsLiveRequest) (SportsSnapshot, error) {
	if err := request.Validate(); err != nil {
		return SportsSnapshot{}, err
	}
	key := "nhl:live:" + request.Timezone
	snapshot, stale, err := adapter.Cache.GetWithPolicy(ctx, key, func(ctx context.Context) (SportsSnapshot, CachePolicy, error) {
		games, err := adapter.fetchGames(ctx, "/score/now")
		if err != nil {
			return SportsSnapshot{}, CachePolicy{}, err
		}
		now := adapter.now().UTC()
		live := make([]Game, 0, len(games))
		seen := make(map[GameID]bool, len(games))
		for _, game := range games {
			if game.Status.Active() && !seen[game.ID] {
				seen[game.ID] = true
				live = append(live, game)
			}
		}
		sortGames(live)
		result := newSportsSnapshot(request.Timezone, live, selectNextGame(games, now), now)
		return result, sportsCachePolicy(result), nil
	})
	if err != nil {
		return SportsSnapshot{}, err
	}
	return markSportsSnapshotStale(snapshot, stale), nil
}

func (adapter *NHLAdapter) now() time.Time {
	if adapter.Now != nil {
		return adapter.Now()
	}
	return time.Now()
}

type nhlLocalized struct {
	Default string `json:"default"`
}

type nhlTeamPayload struct {
	ID         int          `json:"id"`
	Score      int          `json:"score"`
	Abbrev     string       `json:"abbrev"`
	Logo       string       `json:"logo"`
	PlaceName  nhlLocalized `json:"placeName"`
	CommonName nhlLocalized `json:"commonName"`
}

type nhlGamePayload struct {
	ID                int64          `json:"id"`
	GameDate          string         `json:"gameDate"`
	StartTimeUTC      string         `json:"startTimeUTC"`
	GameState         string         `json:"gameState"`
	GameScheduleState string         `json:"gameScheduleState"`
	AwayTeam          nhlTeamPayload `json:"awayTeam"`
	HomeTeam          nhlTeamPayload `json:"homeTeam"`
	Clock             struct {
		TimeRemaining  string `json:"timeRemaining"`
		InIntermission bool   `json:"inIntermission"`
	} `json:"clock"`
	PeriodDescriptor struct {
		Number     int    `json:"number"`
		PeriodType string `json:"periodType"`
	} `json:"periodDescriptor"`
	GameOutcome struct {
		LastPeriodType string `json:"lastPeriodType"`
	} `json:"gameOutcome"`
}

func (adapter *NHLAdapter) fetchGames(ctx context.Context, path string) ([]Game, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(adapter.BaseURL, "/")+path, nil)
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
	limited := io.LimitReader(resp.Body, nhlMaximumBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil || len(body) > nhlMaximumBodyBytes {
		return nil, SportsUnavailable()
	}
	var payload struct {
		Games []nhlGamePayload `json:"games"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, SanitizedError{Code: "sports_response_invalid", Message: "NHL data could not be read", Retryable: true}
	}
	now := adapter.now().UTC()
	games := make([]Game, 0, len(payload.Games))
	for _, raw := range payload.Games {
		game, ok := normalizeNHLGame(raw, now)
		if ok {
			games = append(games, game)
		}
	}
	sortGames(games)
	return games, nil
}

func normalizeNHLGame(raw nhlGamePayload, fetchedAt time.Time) (Game, bool) {
	if raw.ID <= 0 || raw.AwayTeam.ID <= 0 || raw.HomeTeam.ID <= 0 {
		return Game{}, false
	}
	start, err := time.Parse(time.RFC3339, raw.StartTimeUTC)
	if err != nil {
		return Game{}, false
	}
	away := teamFromPayload(raw.AwayTeam)
	home := teamFromPayload(raw.HomeTeam)
	if away.ID == "" || home.ID == "" {
		return Game{}, false
	}
	status := normalizeNHLStatus(raw.GameState, raw.GameScheduleState, raw.Clock.InIntermission)
	periodType := strings.ToUpper(raw.PeriodDescriptor.PeriodType)
	lastPeriodType := strings.ToUpper(raw.GameOutcome.LastPeriodType)
	overtime := periodType == "OT" || lastPeriodType == "OT"
	shootout := periodType == "SO" || lastPeriodType == "SO"
	periodLabel := nhlPeriodLabel(raw.PeriodDescriptor.Number, periodType)
	detail := nhlStatusDetail(status, periodLabel, raw.Clock.TimeRemaining, overtime, shootout)
	providerID := strconv.FormatInt(raw.ID, 10)
	providerState := strings.ToUpper(strings.TrimSpace(raw.GameState))
	scheduleState := strings.ToUpper(strings.TrimSpace(raw.GameScheduleState))
	if scheduleState != "" && scheduleState != "OK" {
		providerState += "/" + scheduleState
	}
	if len(providerState) > 48 {
		providerState = providerState[:48]
	}
	return Game{
		ID: NewGameID(ProviderNHLWeb, LeagueNHL, providerID), ProviderGameID: providerID, League: LeagueNHL, Provider: ProviderNHLWeb,
		AwayTeam: away, HomeTeam: home, AwayScore: raw.AwayTeam.Score, HomeScore: raw.HomeTeam.Score,
		ScheduledAt: start.UTC(), ProviderState: providerState, Status: status,
		Period: raw.PeriodDescriptor.Number, PeriodLabel: periodLabel, Clock: strings.TrimSpace(raw.Clock.TimeRemaining),
		StatusDetail: detail, Overtime: overtime, Shootout: shootout, FreshAsOf: fetchedAt,
	}, true
}

func normalizeNHLStatus(gameState, scheduleState string, intermission bool) GameStatus {
	combined := strings.ToUpper(strings.TrimSpace(gameState + " " + scheduleState))
	switch {
	case containsAny(combined, "CANCEL", "CNCL"):
		return GameCancelled
	case containsAny(combined, "POSTPON", "PPD"):
		return GamePostponed
	case containsAny(combined, "SUSP"):
		return GameSuspended
	case containsAny(combined, "DELAY"):
		return GameDelayed
	}
	switch strings.ToUpper(strings.TrimSpace(gameState)) {
	case "FUT":
		return GameScheduled
	case "PRE":
		return GamePregame
	case "LIVE", "CRIT":
		if intermission {
			return GameIntermission
		}
		return GameLive
	case "FINAL", "OFF", "OVER":
		return GameFinal
	default:
		return GameUnknown
	}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func teamFromPayload(raw nhlTeamPayload) Team {
	id := ProviderTeamID(strconv.Itoa(raw.ID))
	team, ok := nhlTeams[id]
	if !ok {
		abbr := strings.ToUpper(strings.TrimSpace(raw.Abbrev))
		if abbr == "" {
			return Team{}
		}
		name := strings.TrimSpace(raw.PlaceName.Default + " " + raw.CommonName.Default)
		team = makeNHLTeam(raw.ID, abbr, name, raw.CommonName.Default, "#333333", "#FFFFFF")
	}
	if strings.HasPrefix(raw.Logo, "https://") {
		team.ProviderLogoURL = raw.Logo
	}
	return team
}

func selectTeamGames(games []Game, teamID ProviderTeamID, timezone string, now time.Time) []Game {
	location, _ := time.LoadLocation(timezone)
	today := now.In(location).Format("2006-01-02")
	selected := make([]Game, 0, 2)
	for _, game := range games {
		plays := game.AwayTeam.ProviderID == teamID || game.HomeTeam.ProviderID == teamID
		if !plays {
			continue
		}
		if game.Status.Active() || game.ScheduledAt.In(location).Format("2006-01-02") == today {
			selected = append(selected, game)
		}
	}
	sortGames(selected)
	return selected
}

func selectNextTeamGame(games []Game, teamID ProviderTeamID, now time.Time) *Game {
	filtered := make([]Game, 0, len(games))
	for _, game := range games {
		if (game.AwayTeam.ProviderID == teamID || game.HomeTeam.ProviderID == teamID) &&
			(game.Status == GameScheduled || game.Status == GamePregame) && !game.ScheduledAt.Before(now.Add(-time.Minute)) {
			filtered = append(filtered, game)
		}
	}
	return selectNextGame(filtered, now)
}

func selectNextGame(games []Game, now time.Time) *Game {
	var next *Game
	for index := range games {
		game := games[index]
		if game.Status != GameScheduled && game.Status != GamePregame {
			continue
		}
		if game.ScheduledAt.Before(now.Add(-time.Minute)) {
			continue
		}
		if next == nil || game.ScheduledAt.Before(next.ScheduledAt) {
			copy := game
			next = &copy
		}
	}
	return next
}

func hasCurrentOrFuture(games []Game, now time.Time) bool {
	for _, game := range games {
		if game.Status.Active() || ((game.Status == GameScheduled || game.Status == GamePregame) && !game.ScheduledAt.Before(now.Add(-time.Minute))) {
			return true
		}
	}
	return false
}

func sortGames(games []Game) {
	sort.SliceStable(games, func(i, j int) bool {
		if games[i].ScheduledAt.Equal(games[j].ScheduledAt) {
			return games[i].ID < games[j].ID
		}
		return games[i].ScheduledAt.Before(games[j].ScheduledAt)
	})
}

func newSportsSnapshot(timezone string, games []Game, next *Game, now time.Time) SportsSnapshot {
	if games == nil {
		games = []Game{}
	}
	return SportsSnapshot{League: LeagueNHL, Provider: ProviderNHLWeb, DeviceTimezone: timezone, Games: games, NextGame: next, FreshAsOf: now.UTC()}
}

func sportsCachePolicy(snapshot SportsSnapshot) CachePolicy {
	// Choose the shortest policy required by any game. A completed first game
	// in a doubleheader must never slow refreshes for the later active game.
	for _, game := range snapshot.Games {
		if game.Status == GameLive || game.Status == GameIntermission {
			return CachePolicy{FreshTTL: nhlLiveFreshTTL, StaleTTL: nhlLiveStaleTTL}
		}
	}
	for _, game := range snapshot.Games {
		if game.Status == GamePregame || game.Status == GameDelayed || game.Status == GameSuspended {
			return CachePolicy{FreshTTL: nhlPregameFreshTTL, StaleTTL: nhlLiveStaleTTL}
		}
	}
	for _, game := range snapshot.Games {
		if game.Status == GameScheduled && game.ScheduledAt.After(snapshot.FreshAsOf) && game.ScheduledAt.Sub(snapshot.FreshAsOf) < 30*time.Minute {
			return CachePolicy{FreshTTL: nhlPregameFreshTTL, StaleTTL: nhlLiveStaleTTL}
		}
	}
	if snapshot.NextGame != nil && snapshot.NextGame.ScheduledAt.Sub(snapshot.FreshAsOf) < 30*time.Minute {
		return CachePolicy{FreshTTL: nhlPregameFreshTTL, StaleTTL: nhlLiveStaleTTL}
	}
	for _, game := range snapshot.Games {
		if game.Status == GameFinal || game.Status == GamePostponed || game.Status == GameCancelled {
			return CachePolicy{FreshTTL: nhlFinalFreshTTL, StaleTTL: nhlScheduleStaleTTL}
		}
	}
	if len(snapshot.Games) > 0 {
		return CachePolicy{FreshTTL: nhlScheduledFreshTTL, StaleTTL: nhlScheduleStaleTTL}
	}
	return CachePolicy{FreshTTL: nhlOffDayFreshTTL, StaleTTL: nhlScheduleStaleTTL}
}

func markSportsSnapshotStale(snapshot SportsSnapshot, stale bool) SportsSnapshot {
	if !stale {
		return snapshot
	}
	snapshot.Stale = true
	snapshot.Games = append([]Game(nil), snapshot.Games...)
	for index := range snapshot.Games {
		snapshot.Games[index].Stale = true
	}
	if snapshot.NextGame != nil {
		copy := *snapshot.NextGame
		copy.Stale = true
		snapshot.NextGame = &copy
	}
	return snapshot
}

func nhlPeriodLabel(number int, periodType string) string {
	switch periodType {
	case "OT":
		return "OT"
	case "SO":
		return "SO"
	}
	if number > 0 {
		return fmt.Sprintf("P%d", number)
	}
	return ""
}

func nhlStatusDetail(status GameStatus, periodLabel, clock string, overtime, shootout bool) string {
	switch status {
	case GameLive:
		return strings.TrimSpace(periodLabel + " " + clock)
	case GameIntermission:
		if periodLabel != "" {
			return "INT " + periodLabel
		}
		return "INTERMISSION"
	case GameFinal:
		if shootout {
			return "FINAL/SO"
		}
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

func errorsForUnsupportedLeague() error {
	return SanitizedError{Code: "sports_league_unsupported", Message: "The sports league is not supported", Retryable: false}
}

func makeNHLTeam(id int, abbreviation, displayName, shortName, primary, secondary string) Team {
	providerID := ProviderTeamID(strconv.Itoa(id))
	return Team{ID: NewCanonicalTeamID(ProviderNHLWeb, LeagueNHL, providerID), ProviderID: providerID, League: LeagueNHL,
		DisplayName: displayName, ShortName: shortName, Abbreviation: abbreviation, PrimaryColor: primary, SecondaryColor: secondary}
}

var nhlTeams = map[ProviderTeamID]Team{
	"1":  makeNHLTeam(1, "NJD", "New Jersey Devils", "Devils", "#CE1126", "#FFFFFF"),
	"2":  makeNHLTeam(2, "NYI", "New York Islanders", "Islanders", "#00539B", "#F47D30"),
	"3":  makeNHLTeam(3, "NYR", "New York Rangers", "Rangers", "#0038A8", "#CE1126"),
	"4":  makeNHLTeam(4, "PHI", "Philadelphia Flyers", "Flyers", "#F74902", "#000000"),
	"5":  makeNHLTeam(5, "PIT", "Pittsburgh Penguins", "Penguins", "#FCB514", "#000000"),
	"6":  makeNHLTeam(6, "BOS", "Boston Bruins", "Bruins", "#FFB81C", "#000000"),
	"7":  makeNHLTeam(7, "BUF", "Buffalo Sabres", "Sabres", "#003087", "#FFB81C"),
	"8":  makeNHLTeam(8, "MTL", "Montréal Canadiens", "Canadiens", "#AF1E2D", "#192168"),
	"9":  makeNHLTeam(9, "OTT", "Ottawa Senators", "Senators", "#C52032", "#C2912C"),
	"10": makeNHLTeam(10, "TOR", "Toronto Maple Leafs", "Maple Leafs", "#003E7E", "#FFFFFF"),
	"12": makeNHLTeam(12, "CAR", "Carolina Hurricanes", "Hurricanes", "#CE1126", "#FFFFFF"),
	"13": makeNHLTeam(13, "FLA", "Florida Panthers", "Panthers", "#041E42", "#C8102E"),
	"14": makeNHLTeam(14, "TBL", "Tampa Bay Lightning", "Lightning", "#002868", "#FFFFFF"),
	"15": makeNHLTeam(15, "WSH", "Washington Capitals", "Capitals", "#041E42", "#C8102E"),
	"16": makeNHLTeam(16, "CHI", "Chicago Blackhawks", "Blackhawks", "#CF0A2C", "#000000"),
	"17": makeNHLTeam(17, "DET", "Detroit Red Wings", "Red Wings", "#CE1126", "#FFFFFF"),
	"18": makeNHLTeam(18, "NSH", "Nashville Predators", "Predators", "#FFB81C", "#041E42"),
	"19": makeNHLTeam(19, "STL", "St. Louis Blues", "Blues", "#002F87", "#FCB514"),
	"20": makeNHLTeam(20, "CGY", "Calgary Flames", "Flames", "#D2001C", "#FAAF19"),
	"21": makeNHLTeam(21, "COL", "Colorado Avalanche", "Avalanche", "#6F263D", "#236192"),
	"22": makeNHLTeam(22, "EDM", "Edmonton Oilers", "Oilers", "#041E42", "#FF4C00"),
	"23": makeNHLTeam(23, "VAN", "Vancouver Canucks", "Canucks", "#00205B", "#00843D"),
	"24": makeNHLTeam(24, "ANA", "Anaheim Ducks", "Ducks", "#FC4C02", "#B9975B"),
	"25": makeNHLTeam(25, "DAL", "Dallas Stars", "Stars", "#006847", "#8F8F8C"),
	"26": makeNHLTeam(26, "LAK", "Los Angeles Kings", "Kings", "#111111", "#A2AAAD"),
	"28": makeNHLTeam(28, "SJS", "San Jose Sharks", "Sharks", "#006D75", "#EA7200"),
	"29": makeNHLTeam(29, "CBJ", "Columbus Blue Jackets", "Blue Jackets", "#041E42", "#CE1126"),
	"30": makeNHLTeam(30, "MIN", "Minnesota Wild", "Wild", "#154734", "#A6192E"),
	"52": makeNHLTeam(52, "WPG", "Winnipeg Jets", "Jets", "#041E42", "#004C97"),
	"54": makeNHLTeam(54, "VGK", "Vegas Golden Knights", "Golden Knights", "#B4975A", "#333F42"),
	"55": makeNHLTeam(55, "SEA", "Seattle Kraken", "Kraken", "#001628", "#99D9D9"),
	"68": makeNHLTeam(68, "UTA", "Utah Mammoth", "Mammoth", "#69B3E7", "#000000"),
}
