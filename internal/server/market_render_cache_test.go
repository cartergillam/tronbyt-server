package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"tronbyt-server/internal/data"
	"tronbyt-server/internal/providers"
)

type marketCacheResolver struct{}

func (marketCacheResolver) Resolve(context.Context, string, string, string) (string, error) {
	return "offline-test-secret", nil
}

type marketCacheTransport func(*http.Request) (*http.Response, error)

func (f marketCacheTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestMarketRepeatedRenderInjectionSharesQuotesAcrossModesAndInstallations(t *testing.T) {
	var calls atomic.Int32
	a := providers.NewTwelveDataAdapter(marketCacheResolver{}, &http.Client{Transport: marketCacheTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		require.Equal(t, "/quote", r.URL.Path)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"symbol":"AAPL","exchange":"NASDAQ","mic_code":"XNAS","currency":"USD","close":"212.48","change":"2.15","percent_change":"1.02","timestamp":1786028400,"is_market_open":true}`)), Header: make(http.Header)}, nil
	})})
	s := &Server{MarketProvider: a}
	app := &data.App{Name: "market-watch"}
	for _, deviceID := range []string{"installation-a", "installation-b"} {
		for _, mode := range []string{"focus", "ticker"} {
			for range 10 {
				config := map[string]any{"symbols": "AAPL", "credential_id": "market-primary", "display_mode": mode}
				s.injectManagedProviderData(t.Context(), &data.Device{ID: deviceID, Username: "owner"}, app, config)
				var q []providers.MarketQuote
				require.NoError(t, json.Unmarshal([]byte(config["$provider_data"].(string)), &q))
				require.Len(t, q, 1)
				require.NotEmpty(t, q[0].LogoData)
			}
		}
	}
	require.EqualValues(t, 1, calls.Load())
	require.EqualValues(t, 1, a.Diagnostics().EstimatedQuoteCredits)
}
