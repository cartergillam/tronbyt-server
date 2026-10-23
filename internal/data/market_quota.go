package data

import (
	"context"
	"fmt"
	"sort"
	"time"

	"tronbyt-server/internal/providers"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MarketQuotaLedger stores only a one-way provider-key fingerprint and
// aggregate reservations. It is intentionally not exposed through API models.
type MarketQuotaLedger struct {
	KeyHash            string `gorm:"primaryKey;size:64"`
	MinuteBucket       int64
	MinuteUsed         int
	MinuteObservedLeft int
	MinuteObservedAt   int64
	CreditsObservedAt  int64
	MinuteProviderUsed int
	Day                string `gorm:"size:10;index"`
	DayUsed            int
	BlockedRequests    uint64
	NextEligibleUnix   int64
	UpdatedAt          time.Time
}

type MarketQuotaSymbol struct {
	KeyHash   string `gorm:"primaryKey;size:64"`
	Day       string `gorm:"primaryKey;size:10;index"`
	Symbol    string `gorm:"primaryKey;size:64"`
	CreatedAt time.Time
}

type MarketQuotaStore struct{ DB *gorm.DB }

func (s MarketQuotaStore) ReserveMarketCredits(ctx context.Context, key string, cost int, symbols []string, policy providers.MarketQuotaPolicy, now time.Time) (snapshot providers.MarketQuotaSnapshot, allowed bool, err error) {
	if s.DB == nil || key == "" || cost <= 0 {
		return snapshot, false, fmt.Errorf("quota store unavailable")
	}
	policy = policy.Normalize()
	minute := now.UTC().Truncate(time.Minute).Unix()
	day := now.UTC().Format("2006-01-02")
	symbols = distinctSorted(symbols)
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row := MarketQuotaLedger{KeyHash: key}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		// The write is the serialization point for this key across processes.
		if err := tx.Model(&MarketQuotaLedger{}).Where("key_hash = ?", key).Update("updated_at", now.UTC()).Error; err != nil {
			return err
		}
		if err := tx.Where("key_hash = ?", key).First(&row).Error; err != nil {
			return err
		}
		if row.MinuteBucket != minute {
			row.MinuteBucket, row.MinuteUsed, row.MinuteObservedLeft, row.MinuteObservedAt, row.MinuteProviderUsed, row.CreditsObservedAt = minute, 0, 0, 0, 0, 0
		}
		if row.Day != day {
			row.Day, row.DayUsed = day, 0
		}
		currentSymbols := int64(0)
		if len(symbols) > 0 {
			if err := tx.Model(&MarketQuotaSymbol{}).Where("key_hash = ? AND day = ?", key, day).Count(&currentSymbols).Error; err != nil {
				return err
			}
			var known []string
			if err := tx.Model(&MarketQuotaSymbol{}).Where("key_hash = ? AND day = ? AND symbol IN ?", key, day, symbols).Pluck("symbol", &known).Error; err != nil {
				return err
			}
			set := map[string]bool{}
			for _, v := range known {
				set[v] = true
			}
			newCount := len(symbols) - len(known)
			if int(currentSymbols)+newCount > policy.NewSymbolsPerDay {
				row.BlockedRequests++
				row.NextEligibleUnix = nextUTCDay(now).Unix()
				snapshot = quotaSnapshot(row, int(currentSymbols), now, minute, day, policy)
				snapshot.NextEligibleAt = nextUTCDay(now)
				return tx.Save(&row).Error
			}
			for _, symbol := range symbols {
				if set[symbol] {
					continue
				}
				entry := MarketQuotaSymbol{KeyHash: key, Day: day, Symbol: symbol, CreatedAt: now.UTC()}
				if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&entry).Error; err != nil {
					return err
				}
			}
			currentSymbols += int64(newCount)
		}
		minuteCap := policy.MinuteCredits
		if row.MinuteObservedAt == minute && row.MinuteObservedLeft < minuteCap {
			minuteCap = row.MinuteObservedLeft
		}
		if row.MinuteUsed+cost > minuteCap || row.DayUsed+cost > policy.DailyCredits {
			row.BlockedRequests++
			if row.MinuteUsed+cost > minuteCap {
				row.NextEligibleUnix = time.Unix(minute, 0).UTC().Add(time.Minute).Unix()
			}
			if row.DayUsed+cost > policy.DailyCredits {
				row.NextEligibleUnix = nextUTCDay(now).Unix()
			}
			snapshot = quotaSnapshot(row, int(currentSymbols), now, minute, day, policy)
			if row.MinuteUsed+cost > minuteCap {
				snapshot.NextEligibleAt = time.Unix(minute, 0).UTC().Add(time.Minute)
			}
			if row.DayUsed+cost > policy.DailyCredits {
				snapshot.NextEligibleAt = nextUTCDay(now)
			}
			return tx.Save(&row).Error
		}
		row.MinuteBucket, row.MinuteUsed = minute, row.MinuteUsed+cost
		row.Day, row.DayUsed = day, row.DayUsed+cost
		if row.NextEligibleUnix <= now.Unix() {
			row.NextEligibleUnix = 0
		}
		snapshot = quotaSnapshot(row, int(currentSymbols), now, minute, day, policy)
		allowed = true
		return tx.Save(&row).Error
	})
	if err != nil {
		allowed = false
	}
	return snapshot, allowed, err
}

func (s MarketQuotaStore) ObserveMarketCredits(ctx context.Context, key string, left, used int, now time.Time) error {
	if s.DB == nil || key == "" || left < 0 || left > 1000000 || used < 0 || used > 1000000 {
		return nil
	}
	minute := now.UTC().Truncate(time.Minute).Unix()
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row := MarketQuotaLedger{KeyHash: key}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		if err := tx.Model(&MarketQuotaLedger{}).Where("key_hash = ?", key).Update("updated_at", now.UTC()).Error; err != nil {
			return err
		}
		if err := tx.Where("key_hash = ?", key).First(&row).Error; err != nil {
			return err
		}
		if row.MinuteBucket != minute {
			row.MinuteBucket, row.MinuteUsed, row.MinuteObservedLeft, row.MinuteObservedAt, row.MinuteProviderUsed, row.CreditsObservedAt = minute, 0, 0, 0, 0, 0
		}
		if row.MinuteObservedAt != minute || left < row.MinuteObservedLeft {
			row.MinuteObservedLeft = left
		}
		row.MinuteObservedAt, row.MinuteProviderUsed, row.CreditsObservedAt = minute, used, now.UTC().Unix()
		return tx.Save(&row).Error
	})
}

func (s MarketQuotaStore) ReadMarketQuota(ctx context.Context, key string, policy providers.MarketQuotaPolicy, now time.Time) (providers.MarketQuotaSnapshot, error) {
	var row MarketQuotaLedger
	if s.DB == nil {
		return providers.MarketQuotaSnapshot{}, fmt.Errorf("quota store unavailable")
	}
	err := s.DB.WithContext(ctx).Where("key_hash = ?", key).First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return providers.MarketQuotaSnapshot{}, nil
	}
	if err != nil {
		return providers.MarketQuotaSnapshot{}, err
	}
	minute, day := now.UTC().Truncate(time.Minute).Unix(), now.UTC().Format("2006-01-02")
	if row.MinuteBucket != minute {
		row.MinuteUsed, row.MinuteObservedLeft = 0, 0
	}
	if row.Day != day {
		row.DayUsed = 0
	}
	var symbols int64
	if err := s.DB.WithContext(ctx).Model(&MarketQuotaSymbol{}).Where("key_hash = ? AND day = ?", key, day).Count(&symbols).Error; err != nil {
		return providers.MarketQuotaSnapshot{}, err
	}
	return quotaSnapshot(row, int(symbols), now, minute, day, policy.Normalize()), nil
}

func quotaSnapshot(row MarketQuotaLedger, symbols int, now time.Time, minute int64, day string, policy providers.MarketQuotaPolicy) providers.MarketQuotaSnapshot {
	policy = policy.Normalize()
	s := providers.MarketQuotaSnapshot{BlockedRequests: row.BlockedRequests, DailyCreditsRemaining: policy.DailyCredits}
	if row.MinuteBucket == minute {
		s.MinuteCreditsUsed = row.MinuteUsed
		s.MinuteCreditsRemaining = max(0, policy.MinuteCredits-row.MinuteUsed)
		if row.MinuteObservedAt == minute {
			s.ObservedCreditsLeft = row.MinuteObservedLeft
			s.ProviderCreditsUsed = row.MinuteProviderUsed
			s.ObservedAt = time.Unix(row.CreditsObservedAt, 0).UTC()
			s.MinuteCreditsRemaining = min(s.MinuteCreditsRemaining, row.MinuteObservedLeft)
		}
	} else {
		s.MinuteCreditsRemaining = policy.MinuteCredits
	}
	if row.Day == day {
		s.DailyCreditsUsed = row.DayUsed
		s.NewSymbolsToday = symbols
		s.DailyCreditsRemaining = max(0, policy.DailyCredits-row.DayUsed)
	}
	if row.NextEligibleUnix > now.Unix() {
		s.NextEligibleAt = time.Unix(row.NextEligibleUnix, 0).UTC()
	}
	if s.MinuteCreditsRemaining == 0 {
		s.NextEligibleAt = time.Unix(minute, 0).UTC().Add(time.Minute)
	}
	if s.DailyCreditsRemaining == 0 || s.NewSymbolsToday >= policy.NewSymbolsPerDay {
		tomorrow := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day()+1, 0, 0, 0, 0, time.UTC)
		if s.NextEligibleAt.IsZero() || tomorrow.After(s.NextEligibleAt) {
			s.NextEligibleAt = tomorrow
		}
	}
	return s
}

func nextUTCDay(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
}

func distinctSorted(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
