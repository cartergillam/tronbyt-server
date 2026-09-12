package server

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
	"tronbyt-server/internal/data"
	"tronbyt-server/internal/providers"
)

type marketSearchStub struct {
	requests []providers.MarketSearchRequest
	quotes   []providers.MarketRequest
}

func (stub *marketSearchStub) Search(_ context.Context, request providers.MarketSearchRequest) ([]providers.MarketListing, error) {
	stub.requests = append(stub.requests, request)
	return []providers.MarketListing{{ID: "twelvedata:SHOP:XTSE", Symbol: "SHOP", Exchange: "TSX", Currency: "CAD", MIC: "XTSE"}}, nil
}
func (stub *marketSearchStub) Quotes(_ context.Context, request providers.MarketRequest) ([]providers.MarketQuote, error) {
	stub.quotes = append(stub.quotes, request)
	return []providers.MarketQuote{{Symbol: "SHOP", Currency: "CAD"}}, nil
}
func TestMarketSearchAuthorizationAndOwnerScope(t *testing.T) {
	s := newTestServerAPI(t)
	stub := &marketSearchStub{}
	s.MarketProvider = stub
	for _, test := range []struct {
		key, device string
		status      int
	}{{"test_api_key", "testdevice", 200}, {"", "testdevice", 401}, {"device_api_key", "testdevice", 403}, {"device_api_key", "unassigned", 403}} {
		r := newAPIRequest(http.MethodGet, "/v0/devices/"+test.device+"/market/search?q=Shopify&credentialID=custom&scopeID=other", test.key, nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		assert.Equal(t, test.status, w.Code, w.Body.String())
	}
	require.Len(t, stub.requests, 1)
	for _, req := range stub.requests {
		assert.Equal(t, "testuser", req.ScopeID)
		assert.Equal(t, "server_owner", req.ScopeType)
		assert.Equal(t, "custom", req.CredentialID)
	}
}
func TestMarketWatchlistConfigAndRenderContract(t *testing.T) {
	schema := normalizedSchema{Fields: []normalizedSchemaField{{Key: "watchlist", Type: "string"}, {Key: "symbols", Type: "string"}}}
	raw := `[{"symbol":"SHOP","exchange":"TSX","mic":"XTSE","currency":"CAD"}]`
	result, errs := validateConfigPatch(schema, map[string]any{"symbols": "AAPL"}, map[string]any{"watchlist": raw})
	require.Empty(t, errs)
	assert.Contains(t, result["watchlist"], "twelvedata:SHOP:XTSE")
	_, errs = validateConfigPatch(schema, result, map[string]any{"watchlist": "[]"})
	assert.Contains(t, errs, "watchlist")
	_, errs = validateConfigPatch(schema, map[string]any{"symbols": "AAPL"}, map[string]any{})
	assert.Empty(t, errs)
	stub := &marketSearchStub{}
	s := &Server{MarketProvider: stub}
	result["credential_id"] = "primary"
	s.injectManagedProviderData(t.Context(), &data.Device{Username: "owner"}, &data.App{Name: "market-watch"}, result)
	require.Len(t, stub.quotes, 1)
	require.Len(t, stub.quotes[0].Listings, 1)
	assert.Equal(t, "CAD", stub.quotes[0].Listings[0].Currency)
	var quotes []providers.MarketQuote
	require.NoError(t, json.Unmarshal([]byte(result["$provider_data"].(string)), &quotes))
	assert.Equal(t, "CAD", quotes[0].Currency)
}
