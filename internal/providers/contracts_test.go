package providers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarketContractLimitsSymbols(t *testing.T) {
	assert.NoError(t, (MarketRequest{Symbols: []string{"AAPL", "TSX:RY"}}).Validate())
	assert.Error(t, (MarketRequest{Symbols: nil}).Validate())
	assert.Error(t, (MarketRequest{Symbols: []string{"AAPL", "aapl"}}).Validate())
	assert.Error(t, (MarketRequest{Symbols: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}}).Validate())
}

func TestProviderCacheSharesFreshDataAndPreservesStaleData(t *testing.T) {
	cache := NewCache[string](10 * time.Millisecond)
	calls := 0
	value, stale, err := cache.Get(context.Background(), "same-inputs", time.Millisecond, time.Minute, func(context.Context) (string, error) {
		calls++
		return "last-known-good", nil
	})
	require.NoError(t, err)
	assert.False(t, stale)
	assert.Equal(t, "last-known-good", value)

	value, stale, err = cache.Get(context.Background(), "same-inputs", time.Minute, time.Minute, func(context.Context) (string, error) {
		calls++
		return "unexpected", nil
	})
	require.NoError(t, err)
	assert.False(t, stale)
	assert.Equal(t, "last-known-good", value)
	assert.Equal(t, 1, calls)

	time.Sleep(2 * time.Millisecond)
	value, stale, err = cache.Get(context.Background(), "same-inputs", time.Minute, time.Minute, func(context.Context) (string, error) {
		calls++
		return "", errors.New("provider down")
	})
	require.NoError(t, err)
	assert.True(t, stale)
	assert.Equal(t, "last-known-good", value)
}
