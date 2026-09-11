package providers

import (
	"context"
	"strconv"
	"strings"
	"time"
)

func (adapter *ESPNAdapter) scheduleNFL(ctx context.Context, request SportsScheduleRequest) (SportsSnapshot, error) {
	if _, ok := nflTeams[request.TeamID]; !ok {
		return SportsSnapshot{}, UnknownSportsTeam(LeagueNFL, request.TeamID)
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
	key := "espn:nfl:schedule:" + string(request.TeamID) + ":" + request.Timezone + ":" + localDate + ":" + strconv.Itoa(limit)
	snapshot, stale, err := adapter.Cache.GetWithPolicy(ctx, key, func(ctx context.Context) (SportsSnapshot, CachePolicy, error) {
		localNow := now.In(location)
		games, err := adapter.fetchNFLGames(ctx, localNow.AddDate(0, 0, -1), localNow.AddDate(0, 0, 45))
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
		result := newSportsSnapshotFor(LeagueNFL, ProviderESPN, request.Timezone, selected, next, now)
		result.UpcomingGames = upcoming
		addDeviceLocalTimes(&result)
		return result, espnCachePolicy(result), nil
	})
	if err != nil {
		return SportsSnapshot{}, err
	}
	return markSportsSnapshotStale(snapshot, stale), nil
}

func (adapter *ESPNAdapter) liveNFLGames(ctx context.Context, request SportsLiveRequest) (SportsSnapshot, error) {
	now := adapter.now().UTC()
	location, _ := time.LoadLocation(request.Timezone)
	localDate := now.In(location).Format("20060102")
	key := "espn:nfl:live:" + request.Timezone + ":" + localDate
	snapshot, stale, err := adapter.Cache.GetWithPolicy(ctx, key, func(ctx context.Context) (SportsSnapshot, CachePolicy, error) {
		localNow := now.In(location)
		games, err := adapter.fetchNFLGames(ctx, localNow.AddDate(0, 0, -1), localNow.AddDate(0, 0, 2))
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
		result := newSportsSnapshotFor(LeagueNFL, ProviderESPN, request.Timezone, active, selectNextGame(games, now), now)
		return result, espnCachePolicy(result), nil
	})
	if err != nil {
		return SportsSnapshot{}, err
	}
	return markSportsSnapshotStale(snapshot, stale), nil
}

func (adapter *ESPNAdapter) fetchNFLGames(ctx context.Context, from, through time.Time) ([]Game, error) {
	payload, err := adapter.fetchESPNEvents(ctx, adapter.NFLBaseURL, from, through, "NFL")
	if err != nil {
		return nil, err
	}
	fetchedAt := adapter.now().UTC()
	games := make([]Game, 0, len(payload))
	for _, raw := range payload {
		if game, ok := normalizeNFLGame(raw, fetchedAt); ok {
			games = append(games, game)
		}
	}
	sortGames(games)
	return games, nil
}

func normalizeNFLGame(raw espnEventPayload, fetchedAt time.Time) (Game, bool) {
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
	homeTeam := nflTeamFromPayload(home.Team)
	awayTeam := nflTeamFromPayload(away.Team)
	if homeTeam.ID == "" || awayTeam.ID == "" || homeTeam.ID == awayTeam.ID {
		return Game{}, false
	}
	status := normalizeNFLStatus(raw.Status.Type.State, raw.Status.Type.Name, raw.Status.Type.Description, raw.Status.Type.Detail, raw.Status.Type.ShortDetail)
	overtime := raw.Status.Period > 4 || containsAny(strings.ToUpper(raw.Status.Type.Name+" "+raw.Status.Type.Detail+" "+raw.Status.Type.ShortDetail), "OVERTIME", " OT")
	tie := status == GameFinal && strings.TrimSpace(away.Score) != "" && strings.TrimSpace(home.Score) != "" && parseESPNScore(away.Score) == parseESPNScore(home.Score)
	periodLabel := nflPeriodLabel(raw.Status.Period, overtime)
	providerState := strings.ToUpper(strings.TrimSpace(raw.Status.Type.State + "/" + raw.Status.Type.Name))
	if len(providerState) > 64 {
		providerState = providerState[:64]
	}
	return Game{
		ID: NewGameID(ProviderESPN, LeagueNFL, raw.ID), ProviderGameID: raw.ID, League: LeagueNFL, Provider: ProviderESPN,
		AwayTeam: awayTeam, HomeTeam: homeTeam, AwayScore: parseESPNScore(away.Score), HomeScore: parseESPNScore(home.Score),
		AwayRecord: espnRecord(away), HomeRecord: espnRecord(home), ScheduledAt: start.UTC(), ProviderState: providerState,
		Status: status, Period: raw.Status.Period, PeriodLabel: periodLabel, Clock: strings.TrimSpace(raw.Status.DisplayClock),
		StatusDetail: nflStatusDetail(status, periodLabel, raw.Status.DisplayClock, raw.Status.Type.ShortDetail, overtime, tie),
		Overtime:     overtime, Tie: tie, FreshAsOf: fetchedAt,
	}, true
}

func normalizeNFLStatus(state, name, description, detail, shortDetail string) GameStatus {
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

func nflPeriodLabel(period int, overtime bool) string {
	if overtime {
		return "OT"
	}
	if period > 0 && period <= 4 {
		return "Q" + strconv.Itoa(period)
	}
	return ""
}

func nflStatusDetail(status GameStatus, period, clock, providerDetail string, overtime, tie bool) string {
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
		if tie {
			return "FINAL/TIE"
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

func nflTeamFromPayload(raw espnTeamPayload) Team {
	team, ok := nflTeams[ProviderTeamID(strings.TrimSpace(raw.ID))]
	if !ok {
		return Team{}
	}
	return team
}

func makeNFLTeam(id ProviderTeamID, abbreviation, displayName, shortName, primary, secondary string) Team {
	return Team{ID: NewCanonicalTeamID(ProviderESPN, LeagueNFL, id), ProviderID: id, League: LeagueNFL,
		DisplayName: displayName, ShortName: shortName, Abbreviation: abbreviation, PrimaryColor: primary, SecondaryColor: secondary}
}

var nflTeams = map[ProviderTeamID]Team{
	"1":  makeNFLTeam("1", "ATL", "Atlanta Falcons", "Falcons", "#A71930", "#000000"),
	"2":  makeNFLTeam("2", "BUF", "Buffalo Bills", "Bills", "#00338D", "#C60C30"),
	"3":  makeNFLTeam("3", "CHI", "Chicago Bears", "Bears", "#0B162A", "#C83803"),
	"4":  makeNFLTeam("4", "CIN", "Cincinnati Bengals", "Bengals", "#FB4F14", "#000000"),
	"5":  makeNFLTeam("5", "CLE", "Cleveland Browns", "Browns", "#311D00", "#FF3C00"),
	"6":  makeNFLTeam("6", "DAL", "Dallas Cowboys", "Cowboys", "#003594", "#869397"),
	"7":  makeNFLTeam("7", "DEN", "Denver Broncos", "Broncos", "#FB4F14", "#002244"),
	"8":  makeNFLTeam("8", "DET", "Detroit Lions", "Lions", "#0076B6", "#B0B7BC"),
	"9":  makeNFLTeam("9", "GB", "Green Bay Packers", "Packers", "#203731", "#FFB612"),
	"10": makeNFLTeam("10", "TEN", "Tennessee Titans", "Titans", "#0C2340", "#4B92DB"),
	"11": makeNFLTeam("11", "IND", "Indianapolis Colts", "Colts", "#002C5F", "#A2AAAD"),
	"12": makeNFLTeam("12", "KC", "Kansas City Chiefs", "Chiefs", "#E31837", "#FFB81C"),
	"13": makeNFLTeam("13", "LV", "Las Vegas Raiders", "Raiders", "#000000", "#A5ACAF"),
	"14": makeNFLTeam("14", "LAR", "Los Angeles Rams", "Rams", "#003594", "#FFA300"),
	"15": makeNFLTeam("15", "MIA", "Miami Dolphins", "Dolphins", "#008E97", "#FC4C02"),
	"16": makeNFLTeam("16", "MIN", "Minnesota Vikings", "Vikings", "#4F2683", "#FFC62F"),
	"17": makeNFLTeam("17", "NE", "New England Patriots", "Patriots", "#002244", "#C60C30"),
	"18": makeNFLTeam("18", "NO", "New Orleans Saints", "Saints", "#D3BC8D", "#101820"),
	"19": makeNFLTeam("19", "NYG", "New York Giants", "Giants", "#0B2265", "#A71930"),
	"20": makeNFLTeam("20", "NYJ", "New York Jets", "Jets", "#125740", "#000000"),
	"21": makeNFLTeam("21", "PHI", "Philadelphia Eagles", "Eagles", "#004C54", "#A5ACAF"),
	"22": makeNFLTeam("22", "ARI", "Arizona Cardinals", "Cardinals", "#97233F", "#000000"),
	"23": makeNFLTeam("23", "PIT", "Pittsburgh Steelers", "Steelers", "#FFB612", "#101820"),
	"24": makeNFLTeam("24", "LAC", "Los Angeles Chargers", "Chargers", "#0080C6", "#FFC20E"),
	"25": makeNFLTeam("25", "SF", "San Francisco 49ers", "49ers", "#AA0000", "#B3995D"),
	"26": makeNFLTeam("26", "SEA", "Seattle Seahawks", "Seahawks", "#002244", "#69BE28"),
	"27": makeNFLTeam("27", "TB", "Tampa Bay Buccaneers", "Buccaneers", "#D50A0A", "#34302B"),
	"28": makeNFLTeam("28", "WSH", "Washington Commanders", "Commanders", "#5A1414", "#FFB612"),
	"29": makeNFLTeam("29", "CAR", "Carolina Panthers", "Panthers", "#0085CA", "#101820"),
	"30": makeNFLTeam("30", "JAX", "Jacksonville Jaguars", "Jaguars", "#006778", "#D7A22A"),
	"33": makeNFLTeam("33", "BAL", "Baltimore Ravens", "Ravens", "#241773", "#000000"),
	"34": makeNFLTeam("34", "HOU", "Houston Texans", "Texans", "#03202F", "#A71930"),
}
