package providers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"image"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func quotaFixture(symbol string, open bool) map[string]any {
	return map[string]any{"symbol": symbol, "exchange": "NASDAQ", "mic_code": "XNAS", "currency": "USD", "close": "212.48", "change": "2.15", "percent_change": "1.02", "timestamp": 1786028400, "is_market_open": open}
}
func quotaAdapter(t *testing.T, open bool) (*TwelveDataAdapter, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	a := NewTwelveDataAdapter(fakeResolver{secret: "secret"}, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		symbols := strings.Split(r.URL.Query().Get("symbol"), ",")
		if len(symbols) == 1 {
			return jsonResponse(200, quotaFixture(symbols[0], open)), nil
		}
		batch := map[string]any{}
		for _, s := range symbols {
			batch[s] = quotaFixture(s, open)
		}
		return jsonResponse(200, batch), nil
	})})
	return a, calls
}
func quotaRequest(symbols ...string) MarketRequest {
	return MarketRequest{Symbols: symbols, CredentialID: "primary", ScopeType: "server_owner", ScopeID: "owner"}
}
func expireQuotes(a *TwelveDataAdapter, age time.Duration) {
	a.Cache.mu.Lock()
	defer a.Cache.mu.Unlock()
	for k, e := range a.Cache.entries {
		e.freshUntil = time.Now().Add(-age)
		a.Cache.entries[k] = e
		delete(a.Cache.lastRun, k)
	}
}
func TestMarketQuotaSharedListingBatchAndConcurrentRenders(t *testing.T) {
	a, calls := quotaAdapter(t, true)
	r := quotaRequest("AAPL", "MSFT", "NVDA", "GOOG", "AMZN")
	q, err := a.Quotes(t.Context(), r)
	require.NoError(t, err)
	require.Len(t, q, 5)
	require.EqualValues(t, 1, calls.Load())
	d := a.Diagnostics()
	require.Equal(t, 5, d.LastBatchSize)
	require.EqualValues(t, 5, d.EstimatedQuoteCredits)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { _, err := a.Quotes(t.Context(), quotaRequest("MSFT", "AAPL")); require.NoError(t, err) })
	}
	wg.Wait()
	require.EqualValues(t, 1, calls.Load())
	require.GreaterOrEqual(t, a.Diagnostics().CacheHits, uint64(40))
	// Canonical listing cache is independent of ordering and descriptive metadata.
	a2, calls2 := quotaAdapter(t, true)
	l := MarketListing{Symbol: "AAPL", MIC: "XNAS", Currency: "USD"}
	r = quotaRequest()
	r.Listings = []MarketListing{l}
	_, err = a2.Quotes(t.Context(), r)
	require.NoError(t, err)
	r.Listings[0].Name = "different label"
	_, err = a2.Quotes(t.Context(), r)
	require.NoError(t, err)
	require.EqualValues(t, 1, calls2.Load())
	r.ScopeID = "other owner"
	_, err = a2.Quotes(t.Context(), r)
	require.NoError(t, err)
	require.EqualValues(t, 2, calls2.Load())
}
func TestMarketQuotaOpenClosedAndUnknownFreshness(t *testing.T) {
	for _, open := range []bool{true, false} {
		a, calls := quotaAdapter(t, open)
		_, err := a.Quotes(t.Context(), quotaRequest("AAPL"))
		require.NoError(t, err)
		e := a.Cache.entries["server_owner:owner:primary:AAPL"]
		ttl := marketClosedFreshTTL
		if open {
			ttl = marketOpenFreshTTL
		}
		require.WithinDuration(t, time.Now().Add(ttl), e.freshUntil, time.Second)
		require.Greater(t, e.staleUntil.Sub(e.freshUntil), 24*time.Hour)
		for range 10 {
			_, err = a.Quotes(t.Context(), quotaRequest("AAPL"))
			require.NoError(t, err)
		}
		require.EqualValues(t, 1, calls.Load())
	}
	require.Equal(t, 5*time.Minute, marketOpenFreshTTL)
	require.Equal(t, time.Hour, marketClosedFreshTTL)
	require.Equal(t, time.Hour, marketQuoteTTL(MarketQuote{MarketStatus: MarketUnknown}, time.Now()))
}
func TestMarketQuotaLowHeadroomLKGAndReset(t *testing.T) {
	a, calls := quotaAdapter(t, true)
	r := quotaRequest("AAPL", "MSFT", "NVDA", "GOOG", "AMZN")
	_, err := a.Quotes(t.Context(), r)
	require.NoError(t, err)
	expireQuotes(a, time.Second)
	a.observeCredits("secret", http.Header{"Api-Credits-Left": {"2"}, "Api-Credits-Used": {"6"}})
	for range 10 {
		q, err := a.Quotes(t.Context(), r)
		require.NoError(t, err)
		for _, v := range q {
			require.True(t, v.Stale)
		}
	}
	require.EqualValues(t, 1, calls.Load())
	require.Greater(t, a.Diagnostics().QuotaDefers, uint64(0))
	a.usageMu.Lock()
	k := sha256.Sum256([]byte("secret"))
	u := a.usage[k]
	u.Reservations = nil
	u.MinuteUntil = time.Now().Add(-time.Second)
	a.usage[k] = u
	a.usageMu.Unlock()
	a.failureMu.Lock()
	clear(a.failures)
	a.failureMu.Unlock()
	expireQuotes(a, time.Second)
	q, err := a.Quotes(t.Context(), r)
	require.NoError(t, err)
	require.False(t, q[0].Stale)
	require.EqualValues(t, 2, calls.Load())
}
func TestMarketQuotaBatchPartialFailureAndRateLimitLKG(t *testing.T) {
	a, calls := quotaAdapter(t, true)
	r := quotaRequest("AAPL", "MSFT")
	_, err := a.Quotes(t.Context(), r)
	require.NoError(t, err)
	expireQuotes(a, time.Second)
	a.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(200, map[string]any{"AAPL": quotaFixture("AAPL", true), "MSFT": map[string]any{"code": 429, "status": "error"}}), nil
	})
	q, err := a.Quotes(t.Context(), r)
	require.NoError(t, err)
	require.False(t, q[0].Stale)
	require.True(t, q[1].Stale)
	for range 20 {
		_, err = a.Quotes(t.Context(), r)
		require.NoError(t, err)
	}
	require.EqualValues(t, 2, calls.Load())
	// Cold bad symbol must not discard a successful quote.
	b := NewTwelveDataAdapter(fakeResolver{secret: "secret"}, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, map[string]any{"AAPL": quotaFixture("AAPL", true), "BAD": map[string]any{"status": "error", "code": 404}}), nil
	})})
	q, err = b.Quotes(t.Context(), quotaRequest("AAPL", "BAD"))
	require.NoError(t, err)
	require.Empty(t, q[0].ErrorCode)
	require.Equal(t, "invalid_symbol", q[1].ErrorCode)
}
func TestMarketQuotaCreditHeadersSafeAndLocalDailyBound(t *testing.T) {
	a, _ := quotaAdapter(t, true)
	require.True(t, a.reserveCredits("secret", 5, true))
	a.observeCredits("secret", http.Header{"Api-Credits-Left": {"1"}, "Api-Credits-Used": {"7"}})
	k := sha256.Sum256([]byte("secret"))
	u := a.usage[k]
	require.Equal(t, 1, u.Remaining)
	require.Equal(t, 8, u.ObservedLimit)
	require.False(t, u.ObservedAt.IsZero())
	for _, bad := range []string{"", "-1", "NaN", "999999999999999999999999"} {
		a.observeCredits("secret", http.Header{"Api-Credits-Left": {bad}})
		require.Equal(t, u, a.usage[k])
	}
	u.LocalQuoteCredits = 699
	u.Remaining = 8
	a.usage[k] = u
	require.False(t, a.reserveCredits("secret", 5, true))
	// Ten symbols at five minutes is 780 credits, so congested watchlists use six minutes (~650).
	require.Equal(t, 780, 10*int((390*time.Minute)/marketOpenFreshTTL))
	require.Equal(t, 650, 10*int((390*time.Minute)/(6*time.Minute)))
	require.Less(t, 650, 800)
}
func TestMarketQuotaClosingSnapshotReusedThroughWeekend(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	closing := time.Date(2026, 9, 11, 16, 0, 0, 0, loc)
	now := closing.Add(time.Minute)
	q := MarketQuote{MarketStatus: MarketClosed, MIC: "XNAS", QuoteTimestamp: closing, EOD: true}
	require.Equal(t, time.Date(2026, 9, 14, 9, 30, 0, 0, loc).Sub(now), marketQuoteTTL(q, now))
	saturday := time.Date(2026, 9, 12, 12, 0, 0, 0, loc)
	require.Equal(t, time.Date(2026, 9, 14, 9, 30, 0, 0, loc).Sub(saturday), marketQuoteTTL(q, saturday))
	for _, mic := range []string{"XNGS", "XNMS", "XNCM", "XTSE"} {
		q.MIC = mic
		require.Equal(t, time.Date(2026, 9, 14, 9, 30, 0, 0, loc).Sub(saturday), marketQuoteTTL(q, saturday))
	}
	q.QuoteTimestamp = closing.AddDate(0, 0, -1)
	require.Equal(t, time.Hour, marketQuoteTTL(q, saturday))
}
func TestMarketQuotaSearchCacheLastsDayDuringBackoff(t *testing.T) {
	a, calls := quotaAdapter(t, true)
	a.Client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(200, map[string]any{"data": []any{}}), nil
	})
	r := MarketSearchRequest{Query: "Apple", CredentialID: "primary", ScopeType: "server_owner", ScopeID: "owner"}
	_, err := a.Search(t.Context(), r)
	require.NoError(t, err)
	a.recordBackoff("server_owner:owner:primary", SanitizedError{Code: "provider_rate_limited", Retryable: true})
	r.Query = "apple"
	_, err = a.Search(t.Context(), r)
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load())
	e := a.SearchCache.entries["server_owner:owner:primary:APPLE"]
	require.WithinDuration(t, time.Now().Add(24*time.Hour), e.freshUntil, time.Second)
}
func TestMarketApprovedLogosTransparencyAndNoProviderCalls(t *testing.T) {
	a, calls := quotaAdapter(t, true)
	for _, symbol := range []string{"AAPL", "MSFT"} {
		q := []MarketQuote{{Symbol: symbol, MIC: "XNAS", Exchange: "NASDAQ", Currency: "USD"}}
		result := a.HydrateQuotes(t.Context(), quotaRequest(symbol), q)
		for range 10 {
			require.Equal(t, result, a.HydrateQuotes(t.Context(), quotaRequest(symbol), q))
		}
		data, err := base64.StdEncoding.DecodeString(result[0].LogoData)
		require.NoError(t, err)
		img, _, err := image.Decode(bytes.NewReader(data))
		require.NoError(t, err)
		_, _, _, alpha := img.At(0, 0).RGBA()
		require.Zero(t, alpha)
		colors := map[[3]uint32]bool{}
		for y := 0; y < 20; y++ {
			for x := 0; x < 20; x++ {
				r, g, b, a := img.At(x, y).RGBA()
				if a == 65535 {
					colors[[3]uint32{r, g, b}] = true
					if symbol == "AAPL" {
						require.Equal(t, r, g)
						require.Equal(t, g, b)
						require.EqualValues(t, 65535, r)
					}
				}
			}
		}
		if symbol == "MSFT" {
			require.GreaterOrEqual(t, len(colors), 4)
			for color := range colors {
				require.False(t, color[0] > 60000 && color[1] > 60000 && color[2] > 60000, "no white border or separators")
			}
			for _, point := range []image.Point{{4, 4}, {15, 4}, {4, 15}, {15, 15}} {
				_, _, _, alpha = img.At(point.X, point.Y).RGBA()
				require.EqualValues(t, 65535, alpha)
			}
			for i := 0; i < 20; i++ {
				_, _, _, alpha = img.At(9, i).RGBA()
				require.Zero(t, alpha)
				_, _, _, alpha = img.At(i, 9).RGBA()
				require.Zero(t, alpha)
			}
		}
		if dir := os.Getenv("TRONBYT_VISUAL_OUTPUT_DIR"); dir != "" {
			require.NoError(t, os.WriteFile(filepath.Join(dir, symbol+".png"), data, 0600))
		}
	}
	require.Zero(t, calls.Load())
}

func TestMarketQuotaReservationsPreventMinuteBoundaryBurst(t *testing.T) {
	a, _ := quotaAdapter(t, true)
	require.True(t, a.reserveCredits("secret", 5, true))
	k := sha256.Sum256([]byte("secret"))
	u := a.usage[k]
	// Even after the provider observation window resets, recent local calls count.
	u.MinuteUntil = time.Now().Add(-time.Second)
	a.usage[k] = u
	require.False(t, a.reserveCredits("secret", 5, true))
	require.True(t, a.reserveCredits("secret", 3, false))
	require.False(t, a.reserveCredits("secret", 1, false))
}
func TestMarketQuotaFiveCanonicalListingsUseOneBatch(t *testing.T) {
	a, calls := quotaAdapter(t, true)
	r := quotaRequest()
	for _, symbol := range []string{"AAPL", "MSFT", "NVDA", "GOOG", "AMZN"} {
		r.Listings = append(r.Listings, MarketListing{Symbol: symbol, MIC: "XNAS", Currency: "USD"})
	}
	q, err := a.Quotes(t.Context(), r)
	require.NoError(t, err)
	require.Len(t, q, 5)
	require.EqualValues(t, 1, calls.Load())
	require.EqualValues(t, 5, a.Diagnostics().EstimatedQuoteCredits)
}

func TestMarketQuotaNoRefreshBeforeFiveMinutes(t *testing.T) {
	a, calls := quotaAdapter(t, true)
	r := quotaRequest("AAPL")
	_, err := a.Quotes(t.Context(), r)
	require.NoError(t, err)
	k := "server_owner:owner:primary:AAPL"
	a.Cache.mu.Lock()
	e := a.Cache.entries[k]
	// Simulate 4:59 elapsed, leaving one second of the actual five-minute policy.
	e.freshUntil = e.freshUntil.Add(-179 * time.Second)
	a.Cache.entries[k] = e
	a.Cache.lastRun[k] = time.Now().Add(-179 * time.Second)
	a.Cache.mu.Unlock()
	_, err = a.Quotes(t.Context(), r)
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load())
	expireQuotes(a, time.Second)
	_, err = a.Quotes(t.Context(), r)
	require.NoError(t, err)
	require.EqualValues(t, 2, calls.Load())
}
func TestMarketRemoteLogoCacheIndependentOfQuoteRefresh(t *testing.T) {
	a, _ := quotaAdapter(t, true)
	var quotes, metadata, images int
	body, err := os.ReadFile("testdata/logos/aapl.jpg")
	require.NoError(t, err)
	a.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/quote":
			quotes++
			return jsonResponse(200, quotaFixture("NVDA", true)), nil
		case "/logo":
			metadata++
			return jsonResponse(200, map[string]any{"url": "https://logo.twelvedata.com/nvidia.png"}), nil
		default:
			images++
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{"Content-Type": {"image/jpeg"}}, Request: r}, nil
		}
	})
	// LogoImages owns a secured copy of the HTTP client; share this offline transport.
	a.LogoImages = newMarketLogoImages(a.Client)
	r := quotaRequest("NVDA")
	q, err := a.Quotes(t.Context(), r)
	require.NoError(t, err)
	first := a.HydrateQuotes(t.Context(), r, q)
	require.NotEmpty(t, first[0].LogoData)
	expireQuotes(a, time.Second)
	q, err = a.Quotes(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, first, a.HydrateQuotes(t.Context(), r, q))
	require.Equal(t, 2, quotes)
	require.Equal(t, 1, metadata)
	require.Equal(t, 1, images)
}

func TestMarketTenSymbolsStaggeredFiveCreditGroups(t *testing.T) {
	a, calls := quotaAdapter(t, true)
	r := quotaRequest("AAPL", "MSFT", "NVDA", "GOOG", "AMZN", "META", "AMD", "INTC", "IBM", "ORCL")
	q, err := a.Quotes(t.Context(), r)
	require.NoError(t, err)
	require.Len(t, q, 10)
	require.EqualValues(t, 1, calls.Load())
	require.EqualValues(t, 5, a.Diagnostics().EstimatedQuoteCredits)
	for i := 0; i < 5; i++ {
		require.Empty(t, q[i].ErrorCode)
	}
	for i := 5; i < 10; i++ {
		require.Equal(t, "provider_rate_limited", q[i].ErrorCode)
	}
	for range 10 {
		_, err = a.Quotes(t.Context(), r)
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, calls.Load())
	a.usageMu.Lock()
	k := sha256.Sum256([]byte("secret"))
	u := a.usage[k]
	u.MinuteUntil = time.Now().Add(-time.Second)
	u.Reservations = nil
	a.usage[k] = u
	a.usageMu.Unlock()
	a.failureMu.Lock()
	for k, v := range a.failures {
		v.until = time.Now().Add(-time.Second)
		a.failures[k] = v
	}
	a.failureMu.Unlock()
	q, err = a.Quotes(t.Context(), r)
	require.NoError(t, err)
	require.EqualValues(t, 2, calls.Load())
	for _, quote := range q {
		require.Empty(t, quote.ErrorCode)
	}
	require.EqualValues(t, 10, a.Diagnostics().EstimatedQuoteCredits)
	for _, e := range a.Cache.entries {
		require.WithinDuration(t, time.Now().Add(6*time.Minute), e.freshUntil, time.Second)
	}
}

type householdMarketResolver struct{}

func (householdMarketResolver) Resolve(_ context.Context, id, _, _ string) (string, error) {
	return "offline-" + id, nil
}
func TestMarketHouseholdKeysIsolateRateLimitAndInvalidKey(t *testing.T) {
	for _, code := range []int{401, 429} {
		var callsA, callsB int
		a := NewTwelveDataAdapter(householdMarketResolver{}, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") == "apikey offline-a" {
				callsA++
				return jsonResponse(200, map[string]any{"status": "error", "code": code}), nil
			}
			callsB++
			return jsonResponse(200, quotaFixture("AAPL", true)), nil
		})})
		ra := quotaRequest("AAPL")
		ra.CredentialID = "a"
		rb := ra
		rb.CredentialID = "b"
		_, err := a.Quotes(t.Context(), ra)
		require.Error(t, err)
		q, err := a.Quotes(t.Context(), rb)
		require.NoError(t, err)
		require.Empty(t, q[0].ErrorCode)
		for range 10 {
			_, err = a.Quotes(t.Context(), ra)
			require.Error(t, err)
			_, err = a.Quotes(t.Context(), rb)
			require.NoError(t, err)
		}
		require.Equal(t, 1, callsA)
		require.Equal(t, 1, callsB)
		require.Len(t, a.usage, 2)
	}
}
func TestMarketPlazaAndDottedSymbolsPreserveListingAndClassifyEntitlement(t *testing.T) {
	for _, symbol := range []string{"PLZ.UN", "BAM.A", "BRK.B"} {
		s, err := NormalizeMarketSymbol(strings.ToLower(symbol) + ":TSX")
		require.NoError(t, err)
		require.Equal(t, symbol+":TSX", s)
	}
	for _, mode := range []string{"good", "plan400", "plan401", "invalid"} {
		a := NewTwelveDataAdapter(fakeResolver{secret: "secret"}, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			require.Equal(t, "PLZ.UN", r.URL.Query().Get("symbol"))
			require.Equal(t, "XTSE", r.URL.Query().Get("mic_code"))
			if mode == "plan400" {
				return jsonResponse(200, map[string]any{"code": 400, "status": "error", "message": "Symbol is not available on your current plan. Upgrade subscription."}), nil
			}
			if mode == "plan401" {
				return jsonResponse(200, map[string]any{"code": 401, "status": "error", "message": "Canadian market access requires an upgraded plan"}), nil
			}
			if mode == "invalid" {
				return jsonResponse(200, map[string]any{"code": 404, "status": "error", "message": "Invalid symbol"}), nil
			}
			q := quotaFixture("PLZ.UN", true)
			q["exchange"] = "TSX"
			q["mic_code"] = "XTSE"
			q["currency"] = "CAD"
			return jsonResponse(200, q), nil
		})})
		r := quotaRequest()
		r.Listings = []MarketListing{{Symbol: "PLZ.UN", Exchange: "TSX", MIC: "XTSE", Currency: "CAD", RequiredPlan: "Grow"}}
		q, err := a.Quotes(t.Context(), r)
		require.NoError(t, err)
		require.Equal(t, "PLZ.UN", q[0].Symbol)
		if strings.HasPrefix(mode, "plan") {
			require.Equal(t, "provider_entitlement_required", q[0].ErrorCode)
		} else if mode == "invalid" {
			require.Equal(t, "invalid_symbol", q[0].ErrorCode)
		} else {
			require.Empty(t, q[0].ErrorCode)
		}
	}
}
func TestMarketCuratedLogosWinOverOldRemoteCache(t *testing.T) {
	a, calls := quotaAdapter(t, true)
	for _, symbol := range []string{"AAPL", "MSFT"} {
		oldKey := "server_owner:owner:primary:twelvedata:" + symbol + ":XNAS"
		_, _, err := a.LogoCache.Get(t.Context(), oldKey, 24*time.Hour, 0, func(context.Context) (string, error) { return "old-bad-square", nil })
		require.NoError(t, err)
		q := a.HydrateQuotes(t.Context(), quotaRequest(symbol), []MarketQuote{{Symbol: symbol, MIC: "XNAS", Currency: "USD"}})
		require.NotEqual(t, "old-bad-square", q[0].LogoData)
		require.Equal(t, MarketLogoRevision(), q[0].LogoVersion)
		q2 := a.HydrateQuotes(t.Context(), quotaRequest(symbol), []MarketQuote{{Symbol: symbol, MIC: "XNGS", Currency: "USD"}})
		require.Equal(t, q[0].LogoData, q2[0].LogoData)
	}
	require.Zero(t, calls.Load())
}

func TestMarketLogoFingerprintAndCachedQuoteRevisionIndependentOfProvider(t *testing.T) {
	require.NotEqual(t, marketLogoFingerprint("v1", []byte("asset-a")), marketLogoFingerprint("v2", []byte("asset-a")))
	require.NotEqual(t, marketLogoFingerprint("v1", []byte("asset-a")), marketLogoFingerprint("v1", []byte("asset-b")))
	a, calls := quotaAdapter(t, true)
	r := quotaRequest("AAPL")
	q, err := a.Quotes(t.Context(), r)
	require.NoError(t, err)
	revision := a.CachedQuoteRevision(t.Context(), r)
	hydrated := a.HydrateQuotes(t.Context(), r, q)
	require.NotEmpty(t, hydrated[0].LogoVersion)
	require.Equal(t, revision, a.CachedQuoteRevision(t.Context(), r))
	require.EqualValues(t, 1, calls.Load())
	a.Cache.mu.Lock()
	key := "server_owner:owner:primary:AAPL"
	entry := a.Cache.entries[key]
	entry.value = append([]MarketQuote(nil), entry.value...)
	entry.value[0].Price++
	a.Cache.entries[key] = entry
	a.Cache.mu.Unlock()
	require.NotEqual(t, revision, a.CachedQuoteRevision(t.Context(), r))
	require.EqualValues(t, 1, calls.Load())
}

func TestMarketTwoTenStockDevicesHaveIndependentBudgets(t *testing.T) {
	counts := map[string]int{}
	a := NewTwelveDataAdapter(householdMarketResolver{}, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		counts[r.Header.Get("Authorization")]++
		batch := map[string]any{}
		for _, s := range strings.Split(r.URL.Query().Get("symbol"), ",") {
			batch[s] = quotaFixture(s, true)
		}
		return jsonResponse(200, batch), nil
	})})
	r := quotaRequest("AAPL", "MSFT", "NVDA", "GOOG", "AMZN", "META", "AMD", "INTC", "IBM", "ORCL")
	for _, id := range []string{"a", "b"} {
		r.CredentialID = id
		q, err := a.Quotes(t.Context(), r)
		require.NoError(t, err)
		require.Len(t, q, 10)
		require.Equal(t, 1, counts["apikey offline-"+id])
	}
	require.EqualValues(t, 10, a.Diagnostics().EstimatedQuoteCredits)
	a.usageMu.Lock()
	for k, u := range a.usage {
		u.Reservations = nil
		u.MinuteUntil = time.Now().Add(-time.Second)
		a.usage[k] = u
	}
	a.usageMu.Unlock()
	a.failureMu.Lock()
	for k, f := range a.failures {
		f.until = time.Now().Add(-time.Second)
		a.failures[k] = f
	}
	a.failureMu.Unlock()
	for _, id := range []string{"a", "b"} {
		r.CredentialID = id
		q, err := a.Quotes(t.Context(), r)
		require.NoError(t, err)
		for _, v := range q {
			require.Empty(t, v.ErrorCode)
		}
		require.Equal(t, 2, counts["apikey offline-"+id])
	}
	require.EqualValues(t, 20, a.Diagnostics().EstimatedQuoteCredits)
}
