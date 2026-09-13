package providers

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func marketFixtureListing(exchange, mic, currency string) MarketListing {
	return MarketListing{Symbol: "SHOP", Name: "Shopify Inc.", Exchange: exchange, MIC: mic, Currency: currency, Country: "Canada"}
}
func TestCanonicalWatchlistValidation(t *testing.T) {
	cad := marketFixtureListing("TSX", "XTSE", "CAD")
	usd := marketFixtureListing("NYSE", "XNYS", "USD")
	body, _ := json.Marshal([]MarketListing{cad, usd})
	list, err := ParseMarketWatchlist(string(body))
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.NotEqual(t, list[0].ID, list[1].ID)
	for _, raw := range []string{`[]`, `null`, `{}`, `[{"symbol":"BAD SYMBOL"}]`, `[{"symbol":"AAPL"},{"symbol":"aapl"}]`, `[{"symbol":"SHOP:TSX"},{"symbol":"SHOP","exchange":"TSX","mic":"XTSE","currency":"CAD"}]`, strings.Repeat("x", 8193)} {
		_, err := ParseMarketWatchlist(raw)
		require.Error(t, err, raw)
	}
	var five []MarketListing
	for _, sym := range []string{"AAPL", "NVDA", "MSFT", "SHOP", "RY"} {
		five = append(five, MarketListing{Symbol: sym})
	}
	body, _ = json.Marshal(five)
	_, err = ParseMarketWatchlist(string(body))
	require.NoError(t, err)
	five = append(five, MarketListing{Symbol: "GOOG"})
	body, _ = json.Marshal(five)
	_, err = ParseMarketWatchlist(string(body))
	require.Error(t, err)
}

func TestMarketSearchUsesCredentialScopeCachesAndDisambiguates(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		assert.Equal(t, "/symbol_search", req.URL.Path)
		assert.Equal(t, "Shopify", req.URL.Query().Get("symbol"))
		assert.Equal(t, "true", req.URL.Query().Get("show_plan"))
		assert.Equal(t, "apikey test-secret", req.Header.Get("Authorization"))
		assert.NotContains(t, req.URL.String(), "test-secret")
		return jsonResponse(200, map[string]any{"data": []any{
			map[string]any{"symbol": "SHOP", "instrument_name": "Shopify Inc.", "exchange": "TSX", "mic_code": "XTSE", "currency": "CAD", "country": "Canada", "access": map[string]string{"plan": "Grow"}},
			map[string]any{"symbol": "SHOP", "instrument_name": "Shopify Inc.", "exchange": "NYSE", "mic_code": "XNYS", "currency": "USD", "country": "United States"},
			map[string]any{"symbol": "BAD SYMBOL", "exchange": "NYSE", "currency": "USD"},
		}}), nil
	})}
	adapter := NewTwelveDataAdapter(fakeResolver{secret: "test-secret"}, client)
	req := MarketSearchRequest{Query: "Shopify", CredentialID: "primary", ScopeType: "server_owner", ScopeID: "owner"}
	first, err := adapter.Search(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, first, 2)
	assert.Equal(t, "Grow", first[0].RequiredPlan)
	assert.NotEqual(t, first[0].ID, first[1].ID)
	first[0].Name = "modified"
	second, err := adapter.Search(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, "Shopify Inc.", second[0].Name)
	assert.Equal(t, int32(1), calls.Load())
	req.Query = "Another"
	_, err = adapter.Search(t.Context(), req)
	require.Error(t, err)
	assert.Equal(t, int32(1), calls.Load())
	req.Query = "a"
	_, err = adapter.Search(t.Context(), req)
	require.Error(t, err)
}

func TestCanonicalQuotesDoNotSubstituteListingsAndIsolateEntitlement(t *testing.T) {
	for _, mode := range []string{"good", "plan", "wrong_currency", "wrong_mic", "wrong_symbol"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			adapter := NewTwelveDataAdapter(fakeResolver{secret: "secret"}, &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				q := req.URL.Query()
				assert.Equal(t, "SHOP", q.Get("symbol"))
				assert.Empty(t, q.Get("apikey"))
				mic, currency, exchange := "XNYS", "USD", "NYSE"
				if q.Get("mic_code") == "XTSE" {
					mic, currency, exchange = "XTSE", "CAD", "TSX"
					if mode == "plan" {
						return jsonResponse(200, map[string]any{"code": 401, "status": "error", "message": "Upgrade your plan secret"}), nil
					}
				}
				symbol := "SHOP"
				if mode == "wrong_currency" {
					currency = "EUR"
				}
				if mode == "wrong_mic" {
					mic = "XNAS"
				}
				if mode == "wrong_symbol" {
					symbol = "AAPL"
				}
				return jsonResponse(200, map[string]any{"symbol": symbol, "name": "Shopify", "exchange": exchange, "mic_code": mic, "currency": currency, "close": "123.45", "change": "-1.25", "percent_change": "-1.0", "timestamp": 1786028400, "is_market_open": true}), nil
			})})
			req := MarketRequest{Listings: []MarketListing{marketFixtureListing("TSX", "XTSE", "CAD"), marketFixtureListing("NYSE", "XNYS", "USD")}, CredentialID: "primary", ScopeType: "server_owner", ScopeID: "owner"}
			quotes, err := adapter.Quotes(t.Context(), req)
			require.NoError(t, err)
			require.Len(t, quotes, 2)
			if mode == "good" {
				assert.Equal(t, "CAD", quotes[0].Currency)
				assert.Equal(t, "USD", quotes[1].Currency)
				assert.Empty(t, quotes[0].ErrorCode)
			}
			if mode == "plan" {
				assert.Equal(t, "provider_entitlement_required", quotes[0].ErrorCode)
				assert.Empty(t, quotes[1].ErrorCode)
				assert.Equal(t, "CAD", quotes[0].Currency)
			}
			if strings.HasPrefix(mode, "wrong_") {
				assert.Equal(t, "listing_mismatch", quotes[0].ErrorCode)
				assert.Zero(t, quotes[0].Price)
			}
			_, err = adapter.Quotes(t.Context(), req)
			require.NoError(t, err)
			assert.Equal(t, int32(2), calls.Load())
		})
	}
}

func TestMarketLogosAreOptionalCachedAndDoNotReceiveCredentials(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "logo", true: "plan_fallback"}[failure], func(t *testing.T) {
			var metadata, imageCalls atomic.Int32
			imageBytes, err := os.ReadFile("testdata/logos/aapl.jpg")
			require.NoError(t, err)
			adapter := NewTwelveDataAdapter(fakeResolver{secret: "test-secret"}, &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/logo" {
					metadata.Add(1)
					assert.Equal(t, "apikey test-secret", req.Header.Get("Authorization"))
					if failure {
						return jsonResponse(403, map[string]any{"status": "error", "code": 403}), nil
					}
					return jsonResponse(200, map[string]any{"url": "https://api.twelvedata.com/logo/apple.com"}), nil
				}
				imageCalls.Add(1)
				assert.Empty(t, req.Header.Get("Authorization"))
				assert.Equal(t, "/logo/apple.com", req.URL.Path)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/jpeg"}}, Body: io.NopCloser(bytes.NewReader(imageBytes)), Request: req}, nil
			})})
			req := MarketRequest{CredentialID: "primary", ScopeType: "server_owner", ScopeID: "owner"}
			original := []MarketQuote{{Symbol: "NVDA", Price: 123, Exchange: "NASDAQ", MIC: "XNAS", Currency: "USD"}}
			quotes := adapter.HydrateQuotes(t.Context(), req, original)
			second := adapter.HydrateQuotes(t.Context(), req, original)
			assert.Equal(t, 123.0, quotes[0].Price)
			assert.Empty(t, original[0].LogoData)
			assert.Equal(t, quotes, second)
			assert.Equal(t, int32(1), metadata.Load())
			if failure {
				assert.Empty(t, quotes[0].LogoData)
				assert.Zero(t, imageCalls.Load())
			} else {
				assert.NotEmpty(t, quotes[0].LogoData)
				assert.Equal(t, int32(1), imageCalls.Load())
			}
		})
	}
	for _, value := range []string{"http://logo.twelvedata.com/a.png", "https://evil.test/a.png", "https://api.twelvedata.com/quote", "https://logo.twelvedata.com.evil.test/a.png", "https://logo.twelvedata.com/a.png?apikey=secret", "https://logo.twelvedata.com:444/a.png"} {
		assert.False(t, allowedMarketLogoURL(value))
	}
	images := newMarketLogoImages(&http.Client{})
	req, _ := http.NewRequest("GET", "https://evil.test/logo.png", nil)
	assert.Error(t, images.client.CheckRedirect(req, nil))
}
