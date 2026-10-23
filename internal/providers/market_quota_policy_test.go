package providers

import (
	"testing"
	"time"
)

func TestMarketQuotaPolicyDefaultsAndOnlyAllowsSaferOverrides(t *testing.T) {
	defaults := DefaultMarketQuotaPolicy()
	if defaults.MaxSymbols != 8 || defaults.OpenRefresh != 15*time.Minute || defaults.ClosedRefresh != time.Hour || defaults.DailyCredits != 500 || defaults.MinuteCredits != 6 || defaults.NewSymbolsPerDay != 16 {
		t.Fatalf("unexpected safe defaults: %+v", defaults)
	}

	policy := (MarketQuotaPolicy{
		MaxSymbols:       6,
		OpenRefresh:      30 * time.Minute,
		ClosedRefresh:    2 * time.Hour,
		DailyCredits:     300,
		MinuteCredits:    4,
		NewSymbolsPerDay: 8,
	}).Normalize()
	if policy.MaxSymbols != 6 || policy.OpenRefresh != 30*time.Minute || policy.ClosedRefresh != 2*time.Hour || policy.DailyCredits != 300 || policy.MinuteCredits != 4 || policy.NewSymbolsPerDay != 8 {
		t.Fatalf("safer administrator overrides were not retained: %+v", policy)
	}

	unsafe := (MarketQuotaPolicy{
		MaxSymbols:       10,
		OpenRefresh:      5 * time.Minute,
		ClosedRefresh:    10 * time.Minute,
		DailyCredits:     800,
		MinuteCredits:    8,
		NewSymbolsPerDay: 50,
	}).Normalize()
	if unsafe != defaults {
		t.Fatalf("unsafe overrides must fall back to defaults; got %+v want %+v", unsafe, defaults)
	}
}
