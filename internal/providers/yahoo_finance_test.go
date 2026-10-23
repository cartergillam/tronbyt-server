package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func yahooFixture(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/yahoo_plz_un_chart.json")
	require.NoError(t, err)
	return body
}

func TestDecodeYahooPLZUNQuoteFixture(t *testing.T) {
	quote, err := DecodeYahooFinanceChart(yahooFixture(t))
	require.NoError(t, err)
	assert.Equal(t, "PLZ.UN", quote.Symbol)
	assert.Equal(t, "CAD", quote.Currency)
	assert.Equal(t, "XTSE", quote.MIC)
	assert.Equal(t, 5.27, quote.Price)
	assert.Equal(t, 5.28, quote.PreviousClose)
	assert.InDelta(t, -0.01, quote.AbsoluteChange, 1e-9)
	assert.InDelta(t, -0.1893939, quote.PercentageChange, 0.00001)
	assert.Equal(t, time.Unix(1792432759, 0).UTC(), quote.QuoteTimestamp)
	assert.Equal(t, MarketUnknown, quote.MarketStatus)
	assert.True(t, quote.Delayed, "unknown Yahoo delay must be labelled conservatively")
	encoded, err := json.Marshal(quote)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"previousClose":5.28`)
}

func TestDecodeYahooRejectsWrongIdentityAndBadNumbers(t *testing.T) {
	var payload map[string]any
	require.NoError(t, json.Unmarshal(yahooFixture(t), &payload))
	result := payload["chart"].(map[string]any)["result"].([]any)[0].(map[string]any)
	_ = result["meta"].(map[string]any)
	for name, change := range map[string]func(map[string]any){
		"wrong symbol":      func(m map[string]any) { m["symbol"] = "PLZ-UN.V" },
		"wrong currency":    func(m map[string]any) { m["currency"] = "USD" },
		"zero price":        func(m map[string]any) { m["regularMarketPrice"] = 0 },
		"missing timestamp": func(m map[string]any) { m["regularMarketTime"] = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			clone, _ := json.Marshal(payload)
			var copyPayload map[string]any
			_ = json.Unmarshal(clone, &copyPayload)
			copyMeta := copyPayload["chart"].(map[string]any)["result"].([]any)[0].(map[string]any)["meta"].(map[string]any)
			change(copyMeta)
			bad, _ := json.Marshal(copyPayload)
			_, err := DecodeYahooFinanceChart(bad)
			assert.Error(t, err)
		})
	}
	_, err := DecodeYahooFinanceChart([]byte(`{"chart":{"result":null,"error":{"code":"Not Found"}}}`))
	assert.Error(t, err)
}

func TestYahooAdapterCachesAndServesLastKnownGoodAfterRateLimit(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		assert.Equal(t, "/v8/finance/chart/PLZ-UN.TO", r.URL.Path)
		assert.Equal(t, "1d", r.URL.Query().Get("range"))
		assert.Equal(t, "1m", r.URL.Query().Get("interval"))
		status := http.StatusOK
		body := yahooFixture(t)
		if calls.Load() == 1 {
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
		}
		status = http.StatusTooManyRequests
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(nil)), Request: r}, nil
	})}
	adapter := NewYahooFinanceAdapter(client)
	adapter.BaseURL = "https://yahoo.fixture.invalid"
	request := MarketRequest{Listings: []MarketListing{{Symbol: "PLZ.UN", Exchange: "TSX", MIC: "XTSE", Currency: "CAD"}}}
	one, err := adapter.Quotes(context.Background(), request)
	require.NoError(t, err)
	assert.False(t, one[0].Stale)
	two, err := adapter.Quotes(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, int32(1), calls.Load(), "fresh cache prevents repeat request")
	assert.False(t, two[0].Stale)

	key := "yahoo:XTSE:PLZ.UN:CAD"
	adapter.Cache.mu.Lock()
	adapter.Cache.minimumInterval = 0
	entry := adapter.Cache.entries[key]
	entry.freshUntil = time.Now().Add(-time.Second)
	entry.staleUntil = time.Now().Add(time.Hour)
	adapter.Cache.entries[key] = entry
	adapter.Cache.mu.Unlock()
	three, err := adapter.Quotes(context.Background(), request)
	require.NoError(t, err)
	assert.True(t, three[0].Stale)
	assert.Equal(t, int32(2), calls.Load())
	diagnostics, err := adapter.QuoteDiagnostics(context.Background(), request)
	require.NoError(t, err)
	require.Len(t, diagnostics, 1)
	assert.Equal(t, yahooProviderID, diagnostics[0].ProviderID)
	assert.Equal(t, "RATE_LIMITED", diagnostics[0].FailureReason)
	assert.True(t, diagnostics[0].Stale)
	assert.True(t, diagnostics[0].RetryBackoffActive)
	assert.NotContains(t, diagnostics[0].FailureReason, "yahoo.fixture.invalid")
}

func TestYahooAdapterRejectsUnsupportedListings(t *testing.T) {
	adapter := NewYahooFinanceAdapter(nil)
	_, err := adapter.Quotes(context.Background(), MarketRequest{Listings: []MarketListing{{Symbol: "PLZ.UN", Exchange: "NYSE", MIC: "XNYS", Currency: "USD"}}})
	assert.Error(t, err)
	_, err = adapter.Quotes(context.Background(), MarketRequest{Symbols: []string{"AAPL"}})
	assert.Error(t, err)
}

func TestLocalYahooRouterUsesYahooOnlyForPLZAndKeepsTwelveDataForOtherSymbols(t *testing.T) {
	var yahooCalls, twelveCalls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "yahoo.fixture.invalid" {
			yahooCalls.Add(1)
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(yahooFixture(t))), Request: r}, nil
		}
		twelveCalls.Add(1)
		return jsonResponse(http.StatusOK, map[string]any{"symbol": "AAPL", "name": "Apple Inc.", "exchange": "NASDAQ", "mic_code": "XNAS", "currency": "USD", "close": "250.00", "change": "1.00", "percent_change": "0.40", "timestamp": 1792432759, "is_market_open": true}), nil
	})}
	td := NewTwelveDataAdapter(fakeResolver{secret: "offline-test"}, client)
	yahoo := NewYahooFinanceAdapter(client)
	yahoo.BaseURL = "https://yahoo.fixture.invalid"
	router := NewLocalYahooMarketRouter(td, yahoo)
	quotes, err := router.Quotes(context.Background(), MarketRequest{
		Listings: []MarketListing{
			{Symbol: "PLZ.UN", Exchange: "TSX", MIC: "XTSE", Currency: "CAD"},
			{Symbol: "AAPL", Exchange: "NASDAQ", MIC: "XNAS", Currency: "USD"},
		}, CredentialID: "primary", ScopeType: "server_owner", ScopeID: "local-test", MaxSymbols: 8,
	})
	require.NoError(t, err)
	require.Len(t, quotes, 2)
	assert.Equal(t, "PLZ.UN", quotes[0].Symbol)
	assert.Equal(t, "CAD", quotes[0].Currency)
	assert.True(t, quotes[0].Delayed)
	assert.Equal(t, "AAPL", quotes[1].Symbol)
	assert.Equal(t, "USD", quotes[1].Currency)
	assert.False(t, quotes[1].Delayed)
	assert.Equal(t, int32(1), yahooCalls.Load())
	assert.Equal(t, int32(1), twelveCalls.Load())
	hydrated := router.HydrateQuotes(context.Background(), MarketRequest{Listings: []MarketListing{
		{Symbol: "PLZ.UN", Exchange: "TSX", MIC: "XTSE", Currency: "CAD"},
		{Symbol: "AAPL", Exchange: "NASDAQ", MIC: "XNAS", Currency: "USD"},
	}}, quotes)
	require.NotEmpty(t, hydrated[0].LogoData, "the bundled Plaza logo must hydrate Yahoo quotes through the existing shared logo pipeline")
	require.NotEmpty(t, hydrated[0].LogoVersion)
}

func TestLocalYahooRouterDoesNotRouteUnqualifiedOrMismatchedPLZListing(t *testing.T) {
	var yahooCalls, twelveCalls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "yahoo.fixture.invalid" {
			yahooCalls.Add(1)
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(yahooFixture(t))), Request: r}, nil
		}
		twelveCalls.Add(1)
		return jsonResponse(http.StatusOK, map[string]any{"symbol": "PLZ.UN", "name": "Plaza Retail REIT", "exchange": "TSX", "mic_code": "XTSE", "currency": "CAD", "close": "5.27", "change": "-0.01", "percent_change": "-0.19", "timestamp": 1792432759, "is_market_open": false}), nil
	})}
	td := NewTwelveDataAdapter(fakeResolver{secret: "offline-test"}, client)
	yahoo := NewYahooFinanceAdapter(client)
	yahoo.BaseURL = "https://yahoo.fixture.invalid"
	router := NewLocalYahooMarketRouter(td, yahoo)
	for _, request := range []MarketRequest{
		{Symbols: []string{"PLZ.UN"}, CredentialID: "primary", ScopeType: "server_owner", ScopeID: "local-test"},
		{Listings: []MarketListing{{Symbol: "PLZ.UN", Exchange: "NYSE", MIC: "XNYS", Currency: "USD"}}, CredentialID: "primary", ScopeType: "server_owner", ScopeID: "local-test"},
	} {
		_, err := router.Quotes(context.Background(), request)
		require.NoError(t, err)
	}
	assert.Zero(t, yahooCalls.Load(), "only explicit TSX/XTSE CAD listings may use Yahoo")
	assert.Equal(t, int32(2), twelveCalls.Load())
}
