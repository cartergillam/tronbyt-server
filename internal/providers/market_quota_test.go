package providers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

func TestMarketQuoteDiagnosticsRetainSanitizedStaleFailure(t *testing.T) {
	calls := &atomic.Int32{}
	a := NewTwelveDataAdapter(fakeResolver{secret: "secret-never-exposed"}, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return jsonResponse(200, quotaFixture("AAPL", true)), nil
		}
		return nil, errors.New("https://private.example/?apikey=secret-never-exposed: connection reset")
	})})
	request := quotaRequest("AAPL")
	quotes, err := a.Quotes(t.Context(), request)
	require.NoError(t, err)
	require.False(t, quotes[0].Stale)

	expireQuotes(a, 6*time.Minute)
	quotes, err = a.Quotes(t.Context(), request)
	require.NoError(t, err)
	require.True(t, quotes[0].Stale)
	require.EqualValues(t, 2, calls.Load())

	diagnostics, err := a.QuoteDiagnostics(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "AAPL", diagnostics[0].Symbol)
	assert.Equal(t, "NETWORK_ERROR", diagnostics[0].FailureReason)
	assert.Equal(t, "twelve-data", diagnostics[0].ProviderID)
	assert.Equal(t, "primary", diagnostics[0].CredentialID)
	assert.True(t, diagnostics[0].Stale)
	assert.True(t, diagnostics[0].RetryBackoffActive)
	assert.False(t, diagnostics[0].LastSuccessfulQuoteTimestamp.IsZero())
	assert.False(t, diagnostics[0].LastRefreshAttempt.IsZero())
	assert.EqualValues(t, 2, calls.Load(), "diagnostics must never refresh the provider")
	encoded, err := json.Marshal(diagnostics)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "secret-never-exposed")
	assert.NotContains(t, string(encoded), "private.example")
}

func TestMarketDiagnosticFailureReasonCategories(t *testing.T) {
	cases := []struct{ code, want string }{
		{"provider_rate_limited", "RATE_LIMITED"},
		{"provider_entitlement_required", "PLAN_REQUIRED"},
		{"provider_credential_invalid", "INVALID_CREDENTIAL"},
		{"provider_credential_missing", "INVALID_CREDENTIAL"},
		{"invalid_symbol", "INVALID_SYMBOL"},
		{"provider_response_invalid", "BAD_RESPONSE"},
		{"listing_mismatch", "BAD_RESPONSE"},
		{"provider_network_error", "NETWORK_ERROR"},
		{"other", "PROVIDER_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			assert.Equal(t, tc.want, marketDiagnosticFailureReason(SanitizedError{Code: tc.code, Message: "secret raw details"}))
		})
	}
}

func TestNetworkDiagnosticReasonPreservesRendererErrorCode(t *testing.T) {
	a := NewTwelveDataAdapter(fakeResolver{secret: "secret"}, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("private transport details")
	})})
	request := quotaRequest()
	request.Listings = []MarketListing{{Symbol: "AAPL", MIC: "XNAS", Currency: "USD"}}
	quotes, err := a.Quotes(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, quotes, 1)
	assert.Equal(t, "provider_temporarily_unavailable", quotes[0].ErrorCode)
	diagnostics, err := a.QuoteDiagnostics(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "NETWORK_ERROR", diagnostics[0].FailureReason)
}

func TestMarketQuotaOpenClosedAndUnknownFreshness(t *testing.T) {
	for _, open := range []bool{true, false} {
		a, calls := quotaAdapter(t, open)
		_, err := a.Quotes(t.Context(), quotaRequest("AAPL"))
		require.NoError(t, err)
		e := a.Cache.entries["server_owner:owner:primary:AAPL"]
		quote := MarketQuote{MarketStatus: MarketClosed, MIC: "XNAS", QuoteTimestamp: time.Unix(1786028400, 0)}
		if open {
			quote.MarketStatus = MarketOpen
		}
		ttl := marketQuoteTTL(quote, time.Now())
		require.WithinDuration(t, time.Now().Add(ttl), e.freshUntil, time.Second)
		require.Greater(t, e.staleUntil.Sub(e.freshUntil), 24*time.Hour)
		for range 10 {
			_, err = a.Quotes(t.Context(), quotaRequest("AAPL"))
			require.NoError(t, err)
		}
		require.EqualValues(t, 1, calls.Load())
	}
	require.Equal(t, 15*time.Minute, marketOpenFreshTTL)
	require.Equal(t, time.Hour, marketClosedFreshTTL)
	require.Equal(t, time.Hour, marketQuoteTTL(MarketQuote{MarketStatus: MarketUnknown}, time.Now()))
	// Use a fixed pre-open time so the assertion is independent of the CI
	// runner's timezone and time of day. At 09:00 ET, a previous-close quote
	// should be refreshed when the market opens in 30 minutes.
	preOpen := time.Date(2026, time.October, 21, 13, 0, 0, 0, time.UTC)
	previousClose := time.Date(2026, time.October, 20, 20, 0, 0, 0, time.UTC)
	closedTTL := marketQuoteTTL(MarketQuote{MarketStatus: MarketClosed, MIC: "XNAS", QuoteTimestamp: previousClose}, preOpen)
	require.Equal(t, 30*time.Minute, closedTTL, "a known pre-open quote is refreshed at the opening bell if sooner than the closed TTL")
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
	u.MinuteBucket = 0
	u.ObservedAt = time.Time{}
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
	require.Equal(t, 1, u.ObservedLeft)
	require.Equal(t, 8, u.ObservedLimit)
	require.False(t, u.ObservedAt.IsZero())
	for _, bad := range []string{"", "-1", "NaN", "999999999999999999999999"} {
		a.observeCredits("secret", http.Header{"Api-Credits-Left": {bad}})
		require.Equal(t, u, a.usage[k])
	}
	u.LocalCredits = 500
	a.usage[k] = u
	require.False(t, a.reserveCredits("secret", 5, true))
	// Two eight-symbol displays use at most 416 quote credits over 6.5 open hours.
	require.Equal(t, 416, 2*8*int((390*time.Minute)/marketOpenFreshTTL))
	require.Less(t, 416, 500)
}

func TestMarketSearchAndCredentialValidationReserveProviderCredits(t *testing.T) {
	var paths []string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/symbol_search":
			return jsonResponse(200, map[string]any{"status": "ok", "data": []any{}}), nil
		case "/time_series":
			return jsonResponse(200, map[string]any{"meta": map[string]any{"symbol": "AAPL"}, "values": []any{map[string]any{"close": "1.00"}}}), nil
		default:
			t.Fatalf("unexpected provider endpoint %s", r.URL.Path)
			return nil, nil
		}
	})
	a := NewTwelveDataAdapter(fakeResolver{secret: "shared-key"}, &http.Client{Transport: transport})
	_, err := a.Search(t.Context(), MarketSearchRequest{Query: "Apple", CredentialID: "cred-a", ScopeType: "server_owner", ScopeID: "owner-a"})
	require.NoError(t, err)
	require.NoError(t, a.ValidateCredential(t.Context(), "cred-b", "server_owner", "owner-b"))
	require.Equal(t, []string{"/symbol_search", "/time_series"}, paths)
	u := a.usage[sha256.Sum256([]byte("shared-key"))]
	require.Equal(t, 2, u.LocalCredits, "search and validation each cost one provider credit")
	require.Equal(t, 2, u.MinuteUsed)

	blockedCalls := 0
	b := NewTwelveDataAdapter(fakeResolver{secret: "shared-key"}, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { blockedCalls++; return nil, nil })})
	require.True(t, b.reserveCredits("shared-key", 6, false))
	err = b.ValidateCredential(t.Context(), "cred-b", "server_owner", "owner-b")
	var sanitized SanitizedError
	require.ErrorAs(t, err, &sanitized)
	require.Equal(t, "provider_rate_limited", sanitized.Code)
	require.Zero(t, blockedCalls, "the request must be stopped before Twelve Data")
}

type duplicateKeyResolver struct{}

func (duplicateKeyResolver) Resolve(context.Context, string, string, string) (string, error) {
	return "same-underlying-key", nil
}

func TestMarketDuplicateCredentialRecordsShareUnderlyingMinuteBudget(t *testing.T) {
	var calls atomic.Int32
	a := NewTwelveDataAdapter(duplicateKeyResolver{}, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		batch := map[string]any{}
		for symbol := range strings.SplitSeq(r.URL.Query().Get("symbol"), ",") {
			batch[symbol] = quotaFixture(symbol, true)
		}
		return jsonResponse(200, batch), nil
	})})
	first := quotaRequest("AAPL", "MSFT", "NVDA", "GOOG", "AMZN")
	first.CredentialID, first.ScopeID = "credential-record-one", "ben-frame-owner"
	quotes, err := a.Quotes(t.Context(), first)
	require.NoError(t, err)
	require.Len(t, quotes, 5)
	second := quotaRequest("SHOP", "PLZ.UN")
	second.CredentialID, second.ScopeID = "credential-record-two", "carter-frame-owner"
	quotes, err = a.Quotes(t.Context(), second)
	require.NoError(t, err)
	require.Len(t, quotes, 2)
	for _, quote := range quotes {
		require.Equal(t, "provider_rate_limited", quote.ErrorCode)
	}
	require.EqualValues(t, 1, calls.Load(), "two records with the same secret share six credits/minute")
}

func TestMarketProductionQuotaLedgerFailureFailsClosed(t *testing.T) {
	calls := atomic.Int32{}
	a := NewTwelveDataAdapter(fakeResolver{secret: "shared-key"}, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("provider transport must not be reached")
	})})
	a.SetQuotaProtection(nil, DefaultMarketQuotaPolicy())
	require.False(t, a.reserveCredits("shared-key", 1, false))
	_, _, err := a.CredentialQuotaDiagnostics(t.Context(), "primary", "server_owner", "owner")
	require.Error(t, err)
	require.Zero(t, calls.Load())
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
		for y := range 20 {
			for x := range 20 {
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
	// Every endpoint weight shares the same fixed UTC minute bucket.
	require.False(t, a.reserveCredits("secret", 2, false))
	require.True(t, a.reserveCredits("secret", 1, false))
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

func TestMarketQuotaNoRefreshBeforeFifteenMinutes(t *testing.T) {
	a, calls := quotaAdapter(t, true)
	r := quotaRequest("AAPL")
	_, err := a.Quotes(t.Context(), r)
	require.NoError(t, err)
	k := "server_owner:owner:primary:AAPL"
	a.Cache.mu.Lock()
	e := a.Cache.entries[k]
	// Simulate 14:59 elapsed, leaving one second of the open-market policy.
	e.freshUntil = time.Now().Add(time.Second)
	a.Cache.entries[k] = e
	a.Cache.lastRun[k] = time.Now().Add(-14*time.Minute - 59*time.Second)
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

func TestMarketEightSymbolsStaggeredWithinSixCreditMinuteBudget(t *testing.T) {
	a, calls := quotaAdapter(t, true)
	r := quotaRequest("AAPL", "MSFT", "NVDA", "GOOG", "AMZN", "META", "AMD", "INTC")
	q, err := a.Quotes(t.Context(), r)
	require.NoError(t, err)
	require.Len(t, q, 8)
	require.EqualValues(t, 1, calls.Load())
	require.EqualValues(t, 5, a.Diagnostics().EstimatedQuoteCredits)
	for i := range 5 {
		require.Empty(t, q[i].ErrorCode)
	}
	for i := 5; i < 8; i++ {
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
	u.MinuteBucket = 0
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
	require.EqualValues(t, 8, a.Diagnostics().EstimatedQuoteCredits)
	for _, e := range a.Cache.entries {
		require.WithinDuration(t, time.Now().Add(15*time.Minute), e.freshUntil, time.Second)
	}
}

func TestMarketEightSymbolQuotaDeferralKeepsLastKnownGoodVisible(t *testing.T) {
	a, calls := quotaAdapter(t, true)
	symbols := []string{"AAPL", "MSFT", "NVDA", "GOOG", "AMZN", "META", "AMD", "INTC"}
	request := quotaRequest(symbols...)
	now := time.Now()
	a.Cache.mu.Lock()
	for _, symbol := range symbols {
		key := "server_owner:owner:primary:" + symbol
		quote := MarketQuote{Symbol: symbol, Price: 100, Currency: "USD", MarketStatus: MarketOpen, QuoteTimestamp: now.Add(-time.Minute)}
		a.Cache.entries[key] = cacheEntry[[]MarketQuote]{value: []MarketQuote{quote}, freshUntil: now.Add(-time.Second), staleUntil: now.Add(72 * time.Hour)}
	}
	a.Cache.mu.Unlock()
	// Simulate one already-reserved credit. The first five-name batch now fits
	// exactly; the last three are deferred until the following UTC minute.
	require.True(t, a.reserveCredits("secret", 1, true))
	quotes, err := a.Quotes(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, quotes, 8)
	for i := range 5 {
		require.Empty(t, quotes[i].ErrorCode)
		require.False(t, quotes[i].Stale)
	}
	for i := range 3 {
		index := i + 5
		require.Empty(t, quotes[index].ErrorCode, "quota deferral must serve cached data rather than a false PLAN/invalid-symbol error")
		require.True(t, quotes[index].Stale)
		require.Equal(t, 100.0, quotes[index].Price)
	}
	firstCallCount := calls.Load()
	for range 10 {
		quotes, err = a.Quotes(t.Context(), request)
		require.NoError(t, err)
		require.True(t, quotes[7].Stale)
	}
	require.Equal(t, firstCallCount, calls.Load(), "polling must not defeat the one-minute defer")
	require.EqualValues(t, 6, a.Diagnostics().EstimatedQuoteCredits)
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

func TestPlazaLogoRemainsVisibleForTwelveDataPlanError(t *testing.T) {
	adapter := NewTwelveDataAdapter(nil, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("curated logo for an entitlement error must not call the provider")
		return nil, nil
	})})
	request := quotaRequest()
	request.Listings = []MarketListing{{Symbol: "PLZ.UN", Exchange: "TSX", MIC: "XTSE", Currency: "CAD"}}
	listing, err := request.Listings[0].Normalize()
	require.NoError(t, err)
	assert.Equal(t, "twelvedata:PLZ.UN:XTSE", listing.ID)
	normal := adapter.HydrateQuotes(t.Context(), request, []MarketQuote{{Symbol: "PLZ.UN", Exchange: "TSX", MIC: "XTSE", Currency: "CAD"}})
	require.Len(t, normal, 1)
	assert.NotEmpty(t, normal[0].LogoData, "normal Twelve Data quotes use the canonical curated logo")
	quotes := []MarketQuote{{Symbol: "PLZ.UN", ErrorCode: "provider_entitlement_required"}}
	hydrated := adapter.HydrateQuotes(t.Context(), request, quotes)
	require.Len(t, hydrated, 1)
	assert.Equal(t, "provider_entitlement_required", hydrated[0].ErrorCode)
	assert.NotEmpty(t, hydrated[0].LogoData)
	assert.Equal(t, MarketLogoRevision(), hydrated[0].LogoVersion)
	assert.Empty(t, quotes[0].LogoData, "hydration returns a copy")
	data, err := base64.StdEncoding.DecodeString(hydrated[0].LogoData)
	require.NoError(t, err)
	logo, err := png.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, image.Rect(0, 0, 20, 20), logo.Bounds())
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

func TestMarketTwoEightStockDevicesHaveSeparateUnderlyingKeys(t *testing.T) {
	counts := map[string]int{}
	a := NewTwelveDataAdapter(householdMarketResolver{}, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		counts[r.Header.Get("Authorization")]++
		batch := map[string]any{}
		for s := range strings.SplitSeq(r.URL.Query().Get("symbol"), ",") {
			batch[s] = quotaFixture(s, true)
		}
		return jsonResponse(200, batch), nil
	})})
	r := quotaRequest("AAPL", "MSFT", "NVDA", "GOOG", "AMZN", "META", "AMD", "INTC")
	for _, id := range []string{"a", "b"} {
		r.CredentialID = id
		q, err := a.Quotes(t.Context(), r)
		require.NoError(t, err)
		require.Len(t, q, 8)
		require.Equal(t, 1, counts["apikey offline-"+id])
	}
	require.EqualValues(t, 10, a.Diagnostics().EstimatedQuoteCredits)
	a.usageMu.Lock()
	for k, u := range a.usage {
		u.MinuteBucket = 0
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
	require.EqualValues(t, 16, a.Diagnostics().EstimatedQuoteCredits)
}
