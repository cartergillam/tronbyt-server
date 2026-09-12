package providers

import (
	"context"
	"strconv"
	"strings"
	"time"
)

func (adapter *ESPNAdapter) scheduleNBA(ctx context.Context, request SportsScheduleRequest) (SportsSnapshot, error) {
	if _, ok := nbaTeams[request.TeamID]; !ok {
		return SportsSnapshot{}, UnknownSportsTeam(LeagueNBA, request.TeamID)
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
	key := "espn:nba:schedule:" + string(request.TeamID) + ":" + request.Timezone + ":" + localDate + ":" + strconv.Itoa(limit)
	snapshot, stale, err := adapter.Cache.GetWithPolicy(ctx, key, func(ctx context.Context) (SportsSnapshot, CachePolicy, error) {
		localNow := now.In(location)
		games, err := adapter.fetchNBAGames(ctx, localNow.AddDate(0, 0, -1), localNow.AddDate(0, 0, 45))
		if err != nil {
			return SportsSnapshot{}, CachePolicy{}, err
		}
		selected := selectTeamGames(games, request.TeamID, request.Timezone, now)
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
		result := newSportsSnapshotFor(LeagueNBA, ProviderESPN, request.Timezone, selected, next, now)
		result.UpcomingGames = upcoming
		addDeviceLocalTimes(&result)
		return result, espnCachePolicy(result), nil
	})
	if err != nil {
		return SportsSnapshot{}, err
	}
	return markSportsSnapshotStale(snapshot, stale), nil
}

func (adapter *ESPNAdapter) liveNBAGames(ctx context.Context, request SportsLiveRequest) (SportsSnapshot, error) {
	now := adapter.now().UTC()
	location, _ := time.LoadLocation(request.Timezone)
	localDate := now.In(location).Format("20060102")
	key := "espn:nba:live:" + request.Timezone + ":" + localDate
	snapshot, stale, err := adapter.Cache.GetWithPolicy(ctx, key, func(ctx context.Context) (SportsSnapshot, CachePolicy, error) {
		localNow := now.In(location)
		games, err := adapter.fetchNBAGames(ctx, localNow.AddDate(0, 0, -1), localNow.AddDate(0, 0, 2))
		if err != nil {
			return SportsSnapshot{}, CachePolicy{}, err
		}
		active := make([]Game, 0, len(games))
		seen := make(map[GameID]bool, len(games))
		for _, game := range games {
			if game.Status.Active() && !seen[game.ID] {
				seen[game.ID] = true
				active = append(active, game)
			}
		}
		sortGames(active)
		result := newSportsSnapshotFor(LeagueNBA, ProviderESPN, request.Timezone, active, selectNextGame(games, now), now)
		return result, espnCachePolicy(result), nil
	})
	if err != nil {
		return SportsSnapshot{}, err
	}
	return markSportsSnapshotStale(snapshot, stale), nil
}

func (adapter *ESPNAdapter) fetchNBAGames(ctx context.Context, from, through time.Time) ([]Game, error) {
	payload, err := adapter.fetchESPNEvents(ctx, adapter.NBABaseURL, from, through, "NBA")
	if err != nil {
		return nil, err
	}
	fetchedAt := adapter.now().UTC()
	games := make([]Game, 0, len(payload))
	for _, raw := range payload {
		if game, ok := normalizeNBAGame(raw, fetchedAt); ok {
			games = append(games, game)
		}
	}
	sortGames(games)
	return games, nil
}

func normalizeNBAGame(raw espnEventPayload, fetchedAt time.Time) (Game, bool) {
	if strings.TrimSpace(raw.ID) == "" || len(raw.Competitions) == 0 || len(raw.Competitions[0].Competitors) < 2 {
		return Game{}, false
	}
	start, ok := parseESPNTime(raw.Date)
	if !ok {
		return Game{}, false
	}
	home, away := raw.Competitions[0].Competitors[0], raw.Competitions[0].Competitors[1]
	for _, competitor := range raw.Competitions[0].Competitors {
		switch strings.ToLower(competitor.HomeAway) {
		case "home":
			home = competitor
		case "away":
			away = competitor
		}
	}
	homeTeam := nbaTeamFromPayload(home.Team)
	awayTeam := nbaTeamFromPayload(away.Team)
	if homeTeam.ID == "" || awayTeam.ID == "" || homeTeam.ID == awayTeam.ID {
		return Game{}, false
	}
	status := normalizeNBAStatus(raw.Status.Type.State, raw.Status.Type.Name, raw.Status.Type.Description, raw.Status.Type.Detail, raw.Status.Type.ShortDetail)
	overtime := raw.Status.Period > 4 || containsAny(strings.ToUpper(raw.Status.Type.Name+" "+raw.Status.Type.Detail+" "+raw.Status.Type.ShortDetail), "OVERTIME", " OT")
	periodLabel := nbaPeriodLabel(raw.Status.Period, overtime)
	providerState := strings.ToUpper(strings.TrimSpace(raw.Status.Type.State + "/" + raw.Status.Type.Name))
	if len(providerState) > 64 {
		providerState = providerState[:64]
	}
	return Game{
		ID: NewGameID(ProviderESPN, LeagueNBA, raw.ID), ProviderGameID: raw.ID, League: LeagueNBA, Provider: ProviderESPN,
		AwayTeam: awayTeam, HomeTeam: homeTeam, AwayScore: parseESPNScore(away.Score), HomeScore: parseESPNScore(home.Score),
		AwayRecord: espnRecord(away), HomeRecord: espnRecord(home), ScheduledAt: start.UTC(), ProviderState: providerState,
		Status: status, Period: raw.Status.Period, PeriodLabel: periodLabel, Clock: strings.TrimSpace(raw.Status.DisplayClock),
		StatusDetail: nbaStatusDetail(status, periodLabel, raw.Status.DisplayClock, raw.Status.Type.ShortDetail, overtime),
		Overtime:     overtime, FreshAsOf: fetchedAt,
	}, true
}

func normalizeNBAStatus(state, name, description, detail, shortDetail string) GameStatus {
	combined := strings.ToUpper(strings.Join([]string{name, description, detail, shortDetail}, " "))
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
		return GameUnknown
	}
}

func nbaPeriodLabel(period int, overtime bool) string {
	if overtime {
		if period > 5 {
			return strconv.Itoa(period-4) + "OT"
		}
		return "OT"
	}
	if period > 0 && period <= 4 {
		return "Q" + strconv.Itoa(period)
	}
	return ""
}

func nbaStatusDetail(status GameStatus, period, clock, providerDetail string, overtime bool) string {
	switch status {
	case GameLive:
		return strings.TrimSpace(period + " " + clock)
	case GameIntermission:
		if containsAny(strings.ToUpper(providerDetail), "HALF") {
			return "HALFTIME"
		}
		if period != "" {
			return "END " + period
		}
		return "BREAK"
	case GameFinal:
		if overtime {
			return "FINAL/" + period
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

func nbaTeamFromPayload(raw espnTeamPayload) Team {
	providerID := ProviderTeamID(strings.TrimSpace(raw.ID))
	team, ok := nbaTeams[providerID]
	if !ok {
		return Team{}
	}
	if strings.HasPrefix(raw.Logo, "https://") {
		team.ProviderLogoURL = raw.Logo
	}
	return team
}

func makeNBATeam(id ProviderTeamID, abbreviation, displayName, shortName, primary, secondary string) Team {
	return Team{ID: NewCanonicalTeamID(ProviderESPN, LeagueNBA, id), ProviderID: id, League: LeagueNBA,
		DisplayName: displayName, ShortName: shortName, Abbreviation: abbreviation, PrimaryColor: primary, SecondaryColor: secondary}
}

var nbaTeams = map[ProviderTeamID]Team{
	"1":  makeNBATeam("1", "ATL", "Atlanta Hawks", "Hawks", "#E03A3E", "#C1D32F"),
	"2":  makeNBATeam("2", "BOS", "Boston Celtics", "Celtics", "#007A33", "#BA9653"),
	"3":  makeNBATeam("3", "NOP", "New Orleans Pelicans", "Pelicans", "#0C2340", "#C8102E"),
	"4":  makeNBATeam("4", "CHI", "Chicago Bulls", "Bulls", "#CE1141", "#000000"),
	"5":  makeNBATeam("5", "CLE", "Cleveland Cavaliers", "Cavaliers", "#860038", "#FDBB30"),
	"6":  makeNBATeam("6", "DAL", "Dallas Mavericks", "Mavericks", "#00538C", "#B8C4CA"),
	"7":  makeNBATeam("7", "DEN", "Denver Nuggets", "Nuggets", "#0E2240", "#FEC524"),
	"8":  makeNBATeam("8", "DET", "Detroit Pistons", "Pistons", "#C8102E", "#1D42BA"),
	"9":  makeNBATeam("9", "GSW", "Golden State Warriors", "Warriors", "#1D428A", "#FFC72C"),
	"10": makeNBATeam("10", "HOU", "Houston Rockets", "Rockets", "#CE1141", "#000000"),
	"11": makeNBATeam("11", "IND", "Indiana Pacers", "Pacers", "#002D62", "#FDBB30"),
	"12": makeNBATeam("12", "LAC", "LA Clippers", "Clippers", "#C8102E", "#1D428A"),
	"13": makeNBATeam("13", "LAL", "Los Angeles Lakers", "Lakers", "#552583", "#FDB927"),
	"14": makeNBATeam("14", "MIA", "Miami Heat", "Heat", "#98002E", "#F9A01B"),
	"15": makeNBATeam("15", "MIL", "Milwaukee Bucks", "Bucks", "#00471B", "#EEE1C6"),
	"16": makeNBATeam("16", "MIN", "Minnesota Timberwolves", "Timberwolves", "#0C2340", "#78BE20"),
	"17": makeNBATeam("17", "BKN", "Brooklyn Nets", "Nets", "#000000", "#FFFFFF"),
	"18": makeNBATeam("18", "NYK", "New York Knicks", "Knicks", "#006BB6", "#F58426"),
	"19": makeNBATeam("19", "ORL", "Orlando Magic", "Magic", "#0077C0", "#C4CED4"),
	"20": makeNBATeam("20", "PHI", "Philadelphia 76ers", "76ers", "#006BB6", "#ED174C"),
	"21": makeNBATeam("21", "PHX", "Phoenix Suns", "Suns", "#1D1160", "#E56020"),
	"22": makeNBATeam("22", "POR", "Portland Trail Blazers", "Trail Blazers", "#E03A3E", "#000000"),
	"23": makeNBATeam("23", "SAC", "Sacramento Kings", "Kings", "#5A2D81", "#63727A"),
	"24": makeNBATeam("24", "SAS", "San Antonio Spurs", "Spurs", "#C4CED4", "#000000"),
	"25": makeNBATeam("25", "OKC", "Oklahoma City Thunder", "Thunder", "#007AC1", "#EF3B24"),
	"26": makeNBATeam("26", "UTA", "Utah Jazz", "Jazz", "#002B5C", "#F9A01B"),
	"27": makeNBATeam("27", "WAS", "Washington Wizards", "Wizards", "#002B5C", "#E31837"),
	"28": makeNBATeam("28", "TOR", "Toronto Raptors", "Raptors", "#CE1141", "#000000"),
	"29": makeNBATeam("29", "MEM", "Memphis Grizzlies", "Grizzlies", "#5D76A9", "#12173F"),
	"30": makeNBATeam("30", "CHA", "Charlotte Hornets", "Hornets", "#1D1160", "#00788C"),
}
