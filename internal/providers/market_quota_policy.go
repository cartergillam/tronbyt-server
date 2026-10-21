package providers

import (
	"context"
	"time"
)

// MarketQuotaPolicy sets administrator-controlled ceilings. Normalize never
// permits a configuration to exceed the requested Basic-plan safety limits.
type MarketQuotaPolicy struct {
	MaxSymbols       int
	OpenRefresh      time.Duration
	ClosedRefresh    time.Duration
	DailyCredits     int
	MinuteCredits    int
	NewSymbolsPerDay int
}

func DefaultMarketQuotaPolicy() MarketQuotaPolicy {
	return MarketQuotaPolicy{MaxSymbols: 8, OpenRefresh: 15 * time.Minute, ClosedRefresh: time.Hour, DailyCredits: 500, MinuteCredits: 6, NewSymbolsPerDay: 16}
}

func (p MarketQuotaPolicy) Normalize() MarketQuotaPolicy {
	d := DefaultMarketQuotaPolicy()
	if p.MaxSymbols <= 0 || p.MaxSymbols > d.MaxSymbols {
		p.MaxSymbols = d.MaxSymbols
	}
	if p.OpenRefresh < time.Minute || p.OpenRefresh < d.OpenRefresh {
		p.OpenRefresh = d.OpenRefresh
	}
	if p.ClosedRefresh < d.ClosedRefresh {
		p.ClosedRefresh = d.ClosedRefresh
	}
	if p.DailyCredits <= 0 || p.DailyCredits > d.DailyCredits {
		p.DailyCredits = d.DailyCredits
	}
	if p.MinuteCredits <= 0 || p.MinuteCredits > d.MinuteCredits {
		p.MinuteCredits = d.MinuteCredits
	}
	if p.NewSymbolsPerDay <= 0 || p.NewSymbolsPerDay > d.NewSymbolsPerDay {
		p.NewSymbolsPerDay = d.NewSymbolsPerDay
	}
	return p
}

// MarketQuotaSnapshot is safe for owner diagnostics: it contains no secret,
// credential hash, or provider request details.
type MarketQuotaSnapshot struct {
	MinuteCreditsUsed      int       `json:"minuteReservedCredits"`
	ProviderCreditsUsed    int       `json:"providerCreditsUsed,omitempty"`
	DailyCreditsUsed       int       `json:"dailyReservedCredits"`
	MinuteCreditsRemaining int       `json:"minuteBudgetRemaining"`
	DailyCreditsRemaining  int       `json:"dailyBudgetRemaining"`
	NewSymbolsToday        int       `json:"newSymbolsToday"`
	BlockedRequests        uint64    `json:"blockedRequests"`
	ObservedCreditsLeft    int       `json:"providerCreditsLeft,omitempty"`
	ObservedAt             time.Time `json:"providerCreditsObservedAt,omitempty"`
	NextEligibleAt         time.Time `json:"nextEligibleAt,omitempty"`
}

// MarketQuotaStore coordinates reservations across server instances and
// survives restarts. keyHash is SHA-256 of the underlying provider key.
type MarketQuotaStore interface {
	ReserveMarketCredits(context.Context, string, int, []string, MarketQuotaPolicy, time.Time) (MarketQuotaSnapshot, bool, error)
	ObserveMarketCredits(context.Context, string, int, int, time.Time) error
	ReadMarketQuota(context.Context, string, MarketQuotaPolicy, time.Time) (MarketQuotaSnapshot, error)
}

type MarketQuotaDiagnosticProvider interface {
	CredentialQuotaDiagnostics(context.Context, string, string, string) (MarketQuotaSnapshot, MarketQuotaPolicy, error)
}
