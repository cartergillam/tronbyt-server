package server

import (
	"time"

	"tronbyt-server/internal/config"
	"tronbyt-server/internal/providers"
)

func marketQuotaPolicyFromConfig(cfg *config.Settings) providers.MarketQuotaPolicy {
	policy := providers.DefaultMarketQuotaPolicy()
	if cfg == nil {
		return policy
	}
	policy.MaxSymbols = cfg.MarketMaxSymbols
	policy.OpenRefresh = time.Duration(cfg.MarketOpenRefreshMinutes) * time.Minute
	policy.ClosedRefresh = time.Duration(cfg.MarketClosedRefreshMinutes) * time.Minute
	policy.DailyCredits = cfg.MarketDailyCreditBudget
	policy.MinuteCredits = cfg.MarketMinuteCreditBudget
	policy.NewSymbolsPerDay = cfg.MarketNewSymbolsDaily
	return policy.Normalize()
}

func (s *Server) marketMaxSymbols() int {
	if s == nil {
		return providers.MaxMarketSymbols
	}
	return marketQuotaPolicyFromConfig(s.Config).MaxSymbols
}
