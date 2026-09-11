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

const mlbMaximumBodyBytes = 2 << 20

// MLBAdapter consumes the Stats API schedule plus its hydrated linescore. It
// deliberately keeps baseball's inning/count/doubleheader data in the shared
// contract instead of asking a Pixlet app to understand provider payloads.
type MLBAdapter struct {
	Client  *http.Client
	BaseURL string
	Cache   *Cache[SportsSnapshot]
	Now     func() time.Time
}

func NewMLBAdapter(client *http.Client) *MLBAdapter {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &MLBAdapter{
		Client: client, BaseURL: "https://statsapi.mlb.com/api/v1/schedule",
		Cache: NewCache[SportsSnapshot](espnMinimumFetchPeriod), Now: time.Now,
	}
}

func (adapter *MLBAdapter) Teams(_ context.Context, league LeagueID) ([]Team, error) {
	if league != LeagueMLB {
		return nil, errorsForUnsupportedLeague()
	}
	teams := make([]Team, 0, len(mlbTeams))
	for _, team := range mlbTeams {
		teams = append(teams, team)
	}
	sort.Slice(teams, func(i, j int) bool { return teams[i].DisplayName < teams[j].DisplayName })
	return teams, nil
}

func (adapter *MLBAdapter) Schedule(ctx context.Context, request SportsScheduleRequest) (SportsSnapshot, error) {
	if err := request.Validate(); err != nil {
		return SportsSnapshot{}, err
	}
	if request.League != LeagueMLB {
		return SportsSnapshot{}, errorsForUnsupportedLeague()
	}
	if _, ok := mlbTeams[request.TeamID]; !ok {
		return SportsSnapshot{}, UnknownSportsTeam(LeagueMLB, request.TeamID)
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
	key := "mlb:schedule:" + string(request.TeamID) + ":" + request.Timezone + ":" + localDate + ":" + strconv.Itoa(limit) + ":" + strconv.FormatBool(request.IncludeExhibitionOpponents)
	snapshot, stale, err := adapter.Cache.GetWithPolicy(ctx, key, func(ctx context.Context) (SportsSnapshot, CachePolicy, error) {
		from, through := mlbScheduleWindow(now.In(location))
		games, err := adapter.fetchGames(ctx, request.TeamID, from, through, request.IncludeExhibitionOpponents)
		if err != nil {
			return SportsSnapshot{}, CachePolicy{}, err
		}
		selected := selectMLBCurrentGames(games, request.TeamID, request.Timezone, now)
		upcoming := []Game{}
		if len(selected) == 0 {
			upcoming = selectUpcomingTeamGames(games, request.TeamID, now, limit)
		}
		var next *Game
		if len(upcoming) > 0 {
			value := upcoming[0]
			next = &value
		} else {
			next = selectNextTeamGame(games, request.TeamID, now)
		}
		result := newSportsSnapshotFor(LeagueMLB, ProviderMLB, request.Timezone, selected, next, now)
		result.UpcomingGames = upcoming
		addDeviceLocalTimes(&result)
		return result, espnCachePolicy(result), nil
	})
	if err != nil {
		return SportsSnapshot{}, err
	}
	return markSportsSnapshotStale(snapshot, stale), nil
}

func (adapter *MLBAdapter) LiveGames(ctx context.Context, request SportsLiveRequest) (SportsSnapshot, error) {
	if err := request.Validate(); err != nil {
		return SportsSnapshot{}, err
	}
	if request.League != LeagueMLB {
		return SportsSnapshot{}, errorsForUnsupportedLeague()
	}
	now := adapter.now().UTC()
	location, _ := time.LoadLocation(request.Timezone)
	localDate := now.In(location).Format("20060102")
	key := "mlb:live:" + request.Timezone + ":" + localDate
	snapshot, stale, err := adapter.Cache.GetWithPolicy(ctx, key, func(ctx context.Context) (SportsSnapshot, CachePolicy, error) {
		from, through := mlbScheduleWindow(now.In(location))
		games, err := adapter.fetchGames(ctx, "", from, through, false)
		if err != nil {
			return SportsSnapshot{}, CachePolicy{}, err
		}
		active := make([]Game, 0, len(games))
		for _, game := range games {
			if game.Status.Active() {
				active = append(active, game)
			}
		}
		sortGames(active)
		result := newSportsSnapshotFor(LeagueMLB, ProviderMLB, request.Timezone, active, selectNextGame(games, now), now)
		return result, espnCachePolicy(result), nil
	})
	if err != nil {
		return SportsSnapshot{}, err
	}
	return markSportsSnapshotStale(snapshot, stale), nil
}

func (adapter *MLBAdapter) now() time.Time {
	if adapter.Now != nil {
		return adapter.Now()
	}
	return time.Now()
}

// mlbScheduleWindow mirrors the incumbent app's ±36-hour device-local
// request buffer. Filtering still uses the exact local date.
func mlbScheduleWindow(localNow time.Time) (time.Time, time.Time) {
	return localNow.Add(-36 * time.Hour), localNow.Add(36 * time.Hour)
}

type mlbSchedulePayload struct {
	Dates []struct {
		Games []mlbGamePayload `json:"games"`
	} `json:"dates"`
}

type mlbGamePayload struct {
	GamePK       int       `json:"gamePk"`
	GameDate     string    `json:"gameDate"`
	OfficialDate string    `json:"officialDate"`
	GameType     string    `json:"gameType"`
	GameNumber   int       `json:"gameNumber"`
	DoubleHeader string    `json:"doubleHeader"`
	PublicFacing *bool     `json:"publicFacing"`
	Status       mlbStatus `json:"status"`
	Teams        struct {
		Away mlbTeamGamePayload `json:"away"`
		Home mlbTeamGamePayload `json:"home"`
	} `json:"teams"`
	Linescore mlbLinescore `json:"linescore"`
}

type mlbStatus struct {
	AbstractGameState string `json:"abstractGameState"`
	DetailedState     string `json:"detailedState"`
	StatusCode        string `json:"statusCode"`
}

type mlbTeamGamePayload struct {
	Score        int `json:"score"`
	LeagueRecord struct {
		Wins   int `json:"wins"`
		Losses int `json:"losses"`
	} `json:"leagueRecord"`
	Team mlbTeamPayload `json:"team"`
}

type mlbTeamPayload struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	TeamName     string `json:"teamName"`
	Abbreviation string `json:"abbreviation"`
}

type mlbLinescore struct {
	CurrentInning int  `json:"currentInning"`
	IsTopInning   bool `json:"isTopInning"`
	Balls         int  `json:"balls"`
	Strikes       int  `json:"strikes"`
	Outs          int  `json:"outs"`
	Offense       struct {
		First  map[string]any `json:"first"`
		Second map[string]any `json:"second"`
		Third  map[string]any `json:"third"`
	} `json:"offense"`
}

func (adapter *MLBAdapter) fetchGames(ctx context.Context, teamID ProviderTeamID, from, through time.Time, includeExhibitions bool) ([]Game, error) {
	endpoint, err := url.Parse(adapter.BaseURL)
	if err != nil {
		return nil, SportsUnavailable()
	}
	query := endpoint.Query()
	query.Set("sportId", "1")
	query.Set("startDate", from.Format("2006-01-02"))
	query.Set("endDate", through.Format("2006-01-02"))
	query.Set("hydrate", "linescore")
	if teamID != "" {
		query.Set("teamId", string(teamID))
	}
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, mlbMaximumBodyBytes+1))
	if err != nil || len(body) > mlbMaximumBodyBytes {
		return nil, SportsUnavailable()
	}
	var payload mlbSchedulePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, SanitizedError{Code: "sports_response_invalid", Message: "MLB data could not be read", Retryable: true}
	}
	fetchedAt := adapter.now().UTC()
	games := make([]Game, 0)
	for _, date := range payload.Dates {
		for _, raw := range date.Games {
			game, ok := normalizeMLBGame(raw, fetchedAt, includeExhibitions)
			if ok {
				games = append(games, game)
			}
		}
	}
	sortGames(games)
	return games, nil
}

func normalizeMLBGame(raw mlbGamePayload, fetchedAt time.Time, includeExhibitions bool) (Game, bool) {
	if raw.GamePK <= 0 || strings.TrimSpace(raw.GameDate) == "" {
		return Game{}, false
	}
	if raw.PublicFacing != nil && !*raw.PublicFacing {
		return Game{}, false
	}
	start, ok := parseESPNTime(raw.GameDate)
	if !ok {
		return Game{}, false
	}
	away := mlbTeamFromPayload(raw.Teams.Away.Team, includeExhibitions)
	home := mlbTeamFromPayload(raw.Teams.Home.Team, includeExhibitions)
	if away.ID == "" || home.ID == "" || away.ID == home.ID {
		return Game{}, false
	}
	status := normalizeMLBStatus(raw.Status.AbstractGameState, raw.Status.DetailedState)
	inningHalf := mlbInningHalf(raw.Status.DetailedState, raw.Linescore.IsTopInning, raw.Linescore.CurrentInning)
	if status == GameLive && (inningHalf == "middle" || inningHalf == "end") {
		status = GameIntermission
	}
	if status == GameFinal && !strings.EqualFold(strings.TrimSpace(raw.Status.AbstractGameState), "Final") {
		return Game{}, false
	}
	detail := mlbStatusDetail(status, raw.Status.DetailedState, raw.Linescore.CurrentInning, inningHalf)
	return Game{
		ID: NewGameID(ProviderMLB, LeagueMLB, strconv.Itoa(raw.GamePK)), ProviderGameID: strconv.Itoa(raw.GamePK), League: LeagueMLB, Provider: ProviderMLB,
		AwayTeam: away, HomeTeam: home, AwayScore: raw.Teams.Away.Score, HomeScore: raw.Teams.Home.Score,
		AwayRecord: mlbRecord(raw.Teams.Away), HomeRecord: mlbRecord(raw.Teams.Home), ScheduledAt: start.UTC(),
		ProviderState: mlbProviderState(raw.Status), Status: status, Inning: raw.Linescore.CurrentInning, InningHalf: inningHalf,
		Balls: clampMLB(raw.Linescore.Balls, 0, 3), Strikes: clampMLB(raw.Linescore.Strikes, 0, 2), Outs: clampMLB(raw.Linescore.Outs, 0, 2),
		RunnerOnFirst: len(raw.Linescore.Offense.First) > 0, RunnerOnSecond: len(raw.Linescore.Offense.Second) > 0, RunnerOnThird: len(raw.Linescore.Offense.Third) > 0,
		StatusDetail: detail, Overtime: raw.Linescore.CurrentInning > 9, GameNumber: raw.GameNumber,
		Doubleheader: strings.EqualFold(raw.DoubleHeader, "Y") || strings.EqualFold(raw.DoubleHeader, "S"), GameType: strings.TrimSpace(raw.GameType), FreshAsOf: fetchedAt,
	}, true
}

func normalizeMLBStatus(abstract, detailed string) GameStatus {
	combined := strings.ToUpper(strings.TrimSpace(abstract + " " + detailed))
	switch {
	case containsAny(combined, "CANCEL"):
		return GameCancelled
	case containsAny(combined, "POSTPON"):
		return GamePostponed
	case containsAny(combined, "SUSPEND"):
		return GameSuspended
	case containsAny(combined, "DELAY"):
		return GameDelayed
	case strings.EqualFold(strings.TrimSpace(abstract), "Final"):
		return GameFinal
	case strings.EqualFold(strings.TrimSpace(abstract), "Live"):
		return GameLive
	case strings.EqualFold(strings.TrimSpace(abstract), "Preview"):
		if containsAny(combined, "PRE-GAME", "PREGAME", "WARMUP") {
			return GamePregame
		}
		return GameScheduled
	default:
		return GameUnknown
	}
}

func mlbInningHalf(detail string, top bool, inning int) string {
	upper := strings.ToUpper(strings.TrimSpace(detail))
	if containsAny(upper, "MIDDLE OF", "MIDDLE") {
		return "middle"
	}
	if containsAny(upper, "END OF", "END ") {
		return "end"
	}
	if inning <= 0 {
		return ""
	}
	if top {
		return "top"
	}
	return "bottom"
}

func mlbStatusDetail(status GameStatus, providerDetail string, inning int, half string) string {
	switch status {
	case GameLive:
		if inning > 0 && half != "" {
			return strings.ToUpper(half[:1]) + strconv.Itoa(inning)
		}
		return "LIVE"
	case GameIntermission:
		if inning > 0 && half != "" {
			return strings.ToUpper(half) + " " + strconv.Itoa(inning)
		}
		return "INNING BREAK"
	case GameFinal:
		if inning > 9 {
			return "FINAL/" + strconv.Itoa(inning)
		}
		return "FINAL"
	case GamePregame:
		return "PREGAME"
	case GameScheduled:
		return "SCHEDULED"
	default:
		if strings.TrimSpace(providerDetail) != "" {
			return strings.ToUpper(strings.TrimSpace(providerDetail))
		}
		return strings.ToUpper(string(status))
	}
}

func mlbProviderState(status mlbStatus) string {
	value := strings.ToUpper(strings.TrimSpace(status.AbstractGameState + "/" + status.DetailedState))
	if len(value) > 64 {
		return value[:64]
	}
	return value
}

func mlbRecord(team mlbTeamGamePayload) string {
	if team.LeagueRecord.Wins == 0 && team.LeagueRecord.Losses == 0 {
		return ""
	}
	return strconv.Itoa(team.LeagueRecord.Wins) + "-" + strconv.Itoa(team.LeagueRecord.Losses)
}

func clampMLB(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func mlbTeamFromPayload(raw mlbTeamPayload, includeExhibitions bool) Team {
	id := ProviderTeamID(strconv.Itoa(raw.ID))
	team, ok := mlbTeams[id]
	if ok {
		return team
	}
	if !includeExhibitions || raw.ID <= 0 || strings.TrimSpace(raw.Abbreviation) == "" {
		return Team{}
	}
	abbreviation := strings.ToUpper(strings.TrimSpace(raw.Abbreviation))
	name := strings.TrimSpace(raw.Name)
	if name == "" {
		name = strings.TrimSpace(raw.TeamName)
	}
	return Team{ID: NewCanonicalTeamID(ProviderMLB, LeagueMLB, id), ProviderID: id, League: LeagueMLB, DisplayName: name, ShortName: name, Abbreviation: abbreviation, PrimaryColor: "#333333", SecondaryColor: "#FFFFFF"}
}

func selectMLBCurrentGames(games []Game, teamID ProviderTeamID, timezone string, now time.Time) []Game {
	location, _ := time.LoadLocation(timezone)
	today := now.In(location).Format("2006-01-02")
	current := make([]Game, 0, 2)
	for _, game := range games {
		if game.AwayTeam.ProviderID != teamID && game.HomeTeam.ProviderID != teamID {
			continue
		}
		if game.ScheduledAt.In(location).Format("2006-01-02") == today {
			current = append(current, game)
		}
	}
	sort.SliceStable(current, func(i, j int) bool {
		if current[i].ScheduledAt.Equal(current[j].ScheduledAt) {
			return current[i].ID < current[j].ID
		}
		return current[i].ScheduledAt.Before(current[j].ScheduledAt)
	})
	if len(current) > 1 {
		for index := range current {
			if current[index].GameType == "R" {
				current[index].Doubleheader = true
				current[index].GameLabel = "G" + strconv.Itoa(index+1)
			}
		}
	}
	sort.SliceStable(current, func(i, j int) bool {
		left, right := mlbPriority(current[i]), mlbPriority(current[j])
		if left != right {
			return left > right
		}
		if left <= 1 {
			return current[i].ScheduledAt.After(current[j].ScheduledAt)
		}
		return current[i].ScheduledAt.Before(current[j].ScheduledAt)
	})
	return current
}

func mlbPriority(game Game) int {
	switch game.Status {
	case GameLive, GameIntermission:
		return 4
	case GameScheduled, GamePregame:
		return 3
	case GameDelayed, GamePostponed, GameSuspended, GameCancelled:
		return 2
	case GameFinal:
		return 1
	default:
		return 0
	}
}

func makeMLBTeam(id ProviderTeamID, abbreviation, displayName, shortName, primary, secondary string) Team {
	return Team{ID: NewCanonicalTeamID(ProviderMLB, LeagueMLB, id), ProviderID: id, League: LeagueMLB, DisplayName: displayName, ShortName: shortName, Abbreviation: abbreviation, PrimaryColor: primary, SecondaryColor: secondary}
}

var mlbTeams = map[ProviderTeamID]Team{
	"108": makeMLBTeam("108", "LAA", "Los Angeles Angels", "Angels", "#BA0021", "#003263"),
	"109": makeMLBTeam("109", "ARI", "Arizona Diamondbacks", "Diamondbacks", "#A71930", "#E3D4AD"),
	"110": makeMLBTeam("110", "BAL", "Baltimore Orioles", "Orioles", "#DF4601", "#000000"),
	"111": makeMLBTeam("111", "BOS", "Boston Red Sox", "Red Sox", "#BD3039", "#0D2B56"),
	"112": makeMLBTeam("112", "CHC", "Chicago Cubs", "Cubs", "#0E3386", "#CC3433"),
	"113": makeMLBTeam("113", "CIN", "Cincinnati Reds", "Reds", "#C6011F", "#000000"),
	"114": makeMLBTeam("114", "CLE", "Cleveland Guardians", "Guardians", "#0C2340", "#E31937"),
	"115": makeMLBTeam("115", "COL", "Colorado Rockies", "Rockies", "#333366", "#C4CED4"),
	"116": makeMLBTeam("116", "DET", "Detroit Tigers", "Tigers", "#0C2340", "#FA4616"),
	"117": makeMLBTeam("117", "HOU", "Houston Astros", "Astros", "#002D62", "#EB6E1F"),
	"118": makeMLBTeam("118", "KC", "Kansas City Royals", "Royals", "#004687", "#BD9B60"),
	"119": makeMLBTeam("119", "LAD", "Los Angeles Dodgers", "Dodgers", "#005A9C", "#EF3E42"),
	"120": makeMLBTeam("120", "WSH", "Washington Nationals", "Nationals", "#AB0003", "#14225A"),
	"121": makeMLBTeam("121", "NYM", "New York Mets", "Mets", "#002D72", "#FF5910"),
	"133": makeMLBTeam("133", "ATH", "Athletics", "Athletics", "#003831", "#EFB21E"),
	"134": makeMLBTeam("134", "PIT", "Pittsburgh Pirates", "Pirates", "#27251F", "#FDB827"),
	"135": makeMLBTeam("135", "SD", "San Diego Padres", "Padres", "#2F241D", "#FFC425"),
	"136": makeMLBTeam("136", "SEA", "Seattle Mariners", "Mariners", "#0C2C56", "#005C5C"),
	"137": makeMLBTeam("137", "SF", "San Francisco Giants", "Giants", "#FD5A1E", "#27251F"),
	"138": makeMLBTeam("138", "STL", "St. Louis Cardinals", "Cardinals", "#C41E3A", "#0C2340"),
	"139": makeMLBTeam("139", "TB", "Tampa Bay Rays", "Rays", "#092C5C", "#8FBCE6"),
	"140": makeMLBTeam("140", "TEX", "Texas Rangers", "Rangers", "#003278", "#C0111F"),
	"141": makeMLBTeam("141", "TOR", "Toronto Blue Jays", "Blue Jays", "#134A8E", "#1D2D5C"),
	"142": makeMLBTeam("142", "MIN", "Minnesota Twins", "Twins", "#002B5C", "#D31145"),
	"143": makeMLBTeam("143", "PHI", "Philadelphia Phillies", "Phillies", "#E81828", "#002D72"),
	"144": makeMLBTeam("144", "ATL", "Atlanta Braves", "Braves", "#CE1141", "#13274F"),
	"145": makeMLBTeam("145", "CWS", "Chicago White Sox", "White Sox", "#27251F", "#C4CED4"),
	"146": makeMLBTeam("146", "MIA", "Miami Marlins", "Marlins", "#00A3E0", "#EF3340"),
	"147": makeMLBTeam("147", "NYY", "New York Yankees", "Yankees", "#132448", "#FFFFFF"),
	"158": makeMLBTeam("158", "MIL", "Milwaukee Brewers", "Brewers", "#12284B", "#FFC52F"),
}
