package data

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"tronbyt-server/internal/providers"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func quotaDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?_busy_timeout=5000", t.TempDir()+"/quota.db")), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&MarketQuotaLedger{}, &MarketQuotaSymbol{}))
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func TestMarketQuotaConcurrentReservationsAreAtomic(t *testing.T) {
	store := MarketQuotaStore{DB: quotaDB(t)}
	policy := providers.DefaultMarketQuotaPolicy()
	now := time.Date(2026, 10, 20, 15, 42, 13, 0, time.UTC)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 20 {
		wg.Go(func() {
			_, ok, err := store.ReserveMarketCredits(t.Context(), "a9c7288b324892b0-keyhash", 1, nil, policy, now)
			require.NoError(t, err)
			if ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	require.Equal(t, 6, allowed)
	snapshot, err := store.ReadMarketQuota(t.Context(), "a9c7288b324892b0-keyhash", policy, now)
	require.NoError(t, err)
	require.Equal(t, 6, snapshot.MinuteCreditsUsed)
	require.EqualValues(t, 14, snapshot.BlockedRequests)
}

func TestMarketQuotaWeightsChurnRestartAndUTCReset(t *testing.T) {
	db := quotaDB(t)
	store := MarketQuotaStore{DB: db}
	policy := providers.DefaultMarketQuotaPolicy()
	keyHash := "same-underlying-provider-key"
	now := time.Date(2026, 10, 20, 15, 0, 10, 0, time.UTC)

	// A five-symbol batch is five credits, even though it is one HTTP call.
	snapshot, ok, err := store.ReserveMarketCredits(t.Context(), keyHash, 5, []string{"AAPL", "MSFT", "NVDA", "SHOP", "PLZ.UN"}, policy, now)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 5, snapshot.MinuteCreditsUsed)
	_, ok, err = store.ReserveMarketCredits(t.Context(), keyHash, 2, nil, policy, now)
	require.NoError(t, err)
	require.False(t, ok)

	// Reopening the store against the same database preserves the shared key budget.
	restarted := MarketQuotaStore{DB: db}
	_, ok, err = restarted.ReserveMarketCredits(t.Context(), keyHash, 1, nil, policy, now)
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = restarted.ReserveMarketCredits(t.Context(), keyHash, 1, nil, policy, now)
	require.NoError(t, err)
	require.False(t, ok)

	// Churn is unique symbols per key/day, independent of watchlist add/remove cycles.
	for i := range 11 {
		minute := now.Add(time.Duration(i+1) * time.Minute)
		_, ok, err = store.ReserveMarketCredits(t.Context(), keyHash, 1, []string{fmt.Sprintf("S%02d", i)}, policy, minute)
		require.NoError(t, err)
		require.True(t, ok)
	}
	_, ok, err = store.ReserveMarketCredits(t.Context(), keyHash, 1, []string{"SEVENTEEN"}, policy, now.Add(12*time.Minute))
	require.NoError(t, err)
	require.False(t, ok)

	// Basic's published daily reset is midnight UTC; a new UTC day has fresh budget.
	nextDay := time.Date(2026, 10, 21, 0, 0, 1, 0, time.UTC)
	_, ok, err = store.ReserveMarketCredits(t.Context(), keyHash, 1, []string{"NEXTDAY"}, policy, nextDay)
	require.NoError(t, err)
	require.True(t, ok)
}

func TestMarketQuotaTwoDisplaysReuseOrderAndRejectSeventeenthListing(t *testing.T) {
	store := MarketQuotaStore{DB: quotaDB(t)}
	policy := providers.DefaultMarketQuotaPolicy()
	key := "shared-household-provider-key"
	day := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	first := []string{"AAPL", "MSFT", "NVDA", "SHOP", "AMZN", "META", "AMD", "INTC"}
	second := []string{"BNS", "RY", "TD", "ENB", "CNQ", "SU", "BMO", "CM"}

	// Each display may have eight unique listings. The initial requests reserve
	// five credits; retries of known listings do not consume more daily symbols.
	_, ok, err := store.ReserveMarketCredits(t.Context(), key, 5, first, policy, day)
	require.NoError(t, err)
	require.True(t, ok)
	reordered := []string{"INTC", "AMD", "META", "AMZN", "SHOP", "NVDA", "MSFT", "AAPL"}
	snapshot, ok, err := store.ReserveMarketCredits(t.Context(), key, 3, reordered, policy, day.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 8, snapshot.NewSymbolsToday, "display order must not spend new-symbol allowance")

	_, ok, err = store.ReserveMarketCredits(t.Context(), key, 5, second, policy, day.Add(2*time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	snapshot, ok, err = store.ReserveMarketCredits(t.Context(), key, 3, second, policy, day.Add(3*time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 16, snapshot.NewSymbolsToday, "two eight-symbol displays fit the shared daily listing allowance")

	snapshot, ok, err = store.ReserveMarketCredits(t.Context(), key, 1, []string{"SEVENTEENTH"}, policy, day.Add(4*time.Minute))
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, 16, snapshot.NewSymbolsToday)
	require.Equal(t, time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC), snapshot.NextEligibleAt)
	// Reusing a previously allowed listing remains valid even after the cap.
	_, ok, err = store.ReserveMarketCredits(t.Context(), key, 1, []string{"AAPL"}, policy, day.Add(5*time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
}

func TestMarketQuotaSharedMinuteBudgetLetsDeferredHouseholdProgress(t *testing.T) {
	store := MarketQuotaStore{DB: quotaDB(t)}
	policy := providers.DefaultMarketQuotaPolicy()
	key := "same-underlying-provider-key"
	minute := time.Date(2026, 10, 20, 15, 42, 0, 0, time.UTC)

	// Household A's five-symbol batch uses five credits; household B's three
	// symbol tail is deferred rather than exceeding the shared six-credit cap.
	_, allowed, err := store.ReserveMarketCredits(t.Context(), key, 5, []string{"A1", "A2", "A3", "A4", "A5"}, policy, minute)
	require.NoError(t, err)
	require.True(t, allowed)
	snapshot, allowed, err := store.ReserveMarketCredits(t.Context(), key, 3, []string{"B6", "B7", "B8"}, policy, minute)
	require.NoError(t, err)
	require.False(t, allowed)
	require.Equal(t, 5, snapshot.MinuteCreditsUsed)

	// On the next minute B can proceed, then A can use the remaining three
	// credits. Neither household permanently owns the provider-key budget.
	_, allowed, err = store.ReserveMarketCredits(t.Context(), key, 3, []string{"B6", "B7", "B8"}, policy, minute.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, allowed)
	snapshot, allowed, err = store.ReserveMarketCredits(t.Context(), key, 3, []string{"A6", "A7", "A8"}, policy, minute.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, allowed)
	require.Equal(t, 6, snapshot.MinuteCreditsUsed)
}

func TestMarketQuotaDailyLimitAndResetAreUTC(t *testing.T) {
	store := MarketQuotaStore{DB: quotaDB(t)}
	policy := providers.DefaultMarketQuotaPolicy()
	key := "daily-cap-hash"
	start := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for i := range 83 {
		_, allowed, err := store.ReserveMarketCredits(t.Context(), key, 6, nil, policy, start.Add(time.Duration(i)*time.Minute))
		require.NoError(t, err)
		require.True(t, allowed)
	}
	_, allowed, err := store.ReserveMarketCredits(t.Context(), key, 6, nil, policy, start.Add(83*time.Minute))
	require.NoError(t, err)
	require.False(t, allowed, "the 84th batch would reserve 504 of the 500 allowed daily credits")
	snapshot, err := store.ReadMarketQuota(t.Context(), key, policy, start.Add(83*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 498, snapshot.DailyCreditsUsed)
	require.Equal(t, 2, snapshot.DailyCreditsRemaining)
	require.Equal(t, start.Add(24*time.Hour), snapshot.NextEligibleAt)
	_, allowed, err = store.ReserveMarketCredits(t.Context(), key, 1, nil, policy, start.Add(24*time.Hour))
	require.NoError(t, err)
	require.True(t, allowed)
}

func TestMarketQuotaProviderCreditHeadersAreConservative(t *testing.T) {
	store := MarketQuotaStore{DB: quotaDB(t)}
	policy := providers.DefaultMarketQuotaPolicy()
	now := time.Date(2026, 10, 20, 15, 7, 12, 0, time.UTC)
	keyHash := "header-test-hash"
	require.NoError(t, store.ObserveMarketCredits(t.Context(), keyHash, 1, 7, now))
	snapshot, allowed, err := store.ReserveMarketCredits(t.Context(), keyHash, 2, nil, policy, now)
	require.NoError(t, err)
	require.False(t, allowed)
	require.Equal(t, 1, snapshot.ObservedCreditsLeft)
	require.Equal(t, 7, snapshot.ProviderCreditsUsed)
	require.True(t, snapshot.NextEligibleAt.After(now))
	// Invalid or absent headers are ignored by the adapter and cannot increase headroom.
	require.NoError(t, store.ObserveMarketCredits(t.Context(), keyHash, -1, -1, now))
	snapshot, err = store.ReadMarketQuota(t.Context(), keyHash, policy, now)
	require.NoError(t, err)
	require.Equal(t, 1, snapshot.ObservedCreditsLeft)
}
