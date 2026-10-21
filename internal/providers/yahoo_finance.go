package providers

// Yahoo Finance support is intentionally experimental. Yahoo's chart endpoint
// is unofficial and this adapter must only be enabled in an explicit local
// development server.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	yahooProviderID = "yahoo-finance-experimental"
	yahooSymbol     = "PLZ-UN.TO"
	yahooDisplay    = "PLZ.UN"
)

type YahooFinanceAdapter struct {
	Client  *http.Client
	BaseURL string
	Cache   *Cache[MarketQuote]

	mu           sync.Mutex
	lastAttempt  time.Time
	lastSuccess  time.Time
	failure      string
	stale        bool
	backoffUntil time.Time
}

func NewYahooFinanceAdapter(client *http.Client) *YahooFinanceAdapter {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &YahooFinanceAdapter{
		Client: client, BaseURL: "https://query1.finance.yahoo.com",
		Cache: NewCache[MarketQuote](30 * time.Second),
	}
}

// Quotes accepts only the explicitly verified TSX listing. It never guesses a
// Yahoo alias for arbitrary symbols or silently substitutes another listing.
func (a *YahooFinanceAdapter) Quotes(ctx context.Context, request MarketRequest) ([]MarketQuote, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	symbols := request.Symbols
	if request.Listings != nil {
		symbols = make([]string, 0, len(request.Listings))
		for _, listing := range request.Listings {
			if !isYahooPLZListing(listing) {
				return nil, SanitizedError{Code: "invalid_symbol", Message: "This experimental provider only supports the verified PLZ.UN listing", Retryable: false}
			}
			symbols = append(symbols, listing.Symbol)
		}
	}
	if len(symbols) != 1 || !strings.EqualFold(strings.TrimSpace(symbols[0]), yahooDisplay) {
		return nil, SanitizedError{Code: "invalid_symbol", Message: "This experimental provider only supports PLZ.UN", Retryable: false}
	}
	quote, stale, err := a.Cache.Get(ctx, "yahoo:XTSE:PLZ.UN:CAD", 5*time.Minute, marketStaleTTL, a.fetch)
	a.mu.Lock()
	a.stale = stale && err == nil
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	quote.Stale = stale
	return []MarketQuote{quote}, nil
}

func isYahooPLZListing(listing MarketListing) bool {
	if !strings.EqualFold(listing.Symbol, yahooDisplay) {
		return false
	}
	venue := strings.ToUpper(strings.TrimSpace(listing.MIC))
	if venue == "" {
		venue = strings.ToUpper(strings.TrimSpace(listing.Exchange))
	}
	return (venue == "XTSE" || venue == "TSX" || venue == "TORONTO STOCK EXCHANGE") && strings.EqualFold(listing.Currency, "CAD")
}

func (a *YahooFinanceAdapter) fetch(ctx context.Context) (MarketQuote, error) {
	a.mu.Lock()
	if time.Now().Before(a.backoffUntil) {
		a.mu.Unlock()
		return MarketQuote{}, SanitizedError{Code: "provider_rate_limited", Message: "Market data is temporarily rate limited", Retryable: true}
	}
	a.lastAttempt = time.Now().UTC()
	a.mu.Unlock()
	quote, err := a.requestQuote(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.failure = yahooDiagnosticReason(err)
		var classified SanitizedError
		if errors.As(err, &classified) && classified.Code == "provider_rate_limited" {
			a.backoffUntil = time.Now().Add(marketRateLimitBackoff)
		}
		return MarketQuote{}, err
	}
	a.lastSuccess = quote.QuoteTimestamp
	a.failure = ""
	a.backoffUntil = time.Time{}
	return quote, nil
}

func (a *YahooFinanceAdapter) requestQuote(ctx context.Context) (MarketQuote, error) {
	endpoint := strings.TrimRight(a.BaseURL, "/") + "/v8/finance/chart/" + url.PathEscape(yahooSymbol)
	u, err := url.Parse(endpoint)
	if err != nil {
		return MarketQuote{}, TemporarilyUnavailable()
	}
	query := u.Query()
	query.Set("range", "1d")
	query.Set("interval", "1m")
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return MarketQuote{}, TemporarilyUnavailable()
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Tronbyt local experimental adapter")
	resp, err := a.Client.Do(req)
	if err != nil {
		return MarketQuote{}, SanitizedError{Code: "provider_network_error", Message: "Market data is temporarily unavailable", Retryable: true}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024+1))
	if err != nil || len(body) > 512*1024 {
		return MarketQuote{}, SanitizedError{Code: "provider_response_invalid", Message: "Market data could not be read", Retryable: true}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return MarketQuote{}, SanitizedError{Code: "provider_rate_limited", Message: "Market data is temporarily rate limited", Retryable: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return MarketQuote{}, SanitizedError{Code: "provider_error", Message: "Market data is temporarily unavailable", Retryable: true}
	}
	quote, err := DecodeYahooFinanceChart(body)
	if err != nil {
		return MarketQuote{}, SanitizedError{Code: "provider_response_invalid", Message: "Market data could not be read", Retryable: true}
	}
	return quote, nil
}

// DecodeYahooFinanceChart normalizes only the documented fixture shape used by
// the experimental chart adapter. It rejects mismatched symbols/currencies and
// keeps the configured display ticker independent from Yahoo's alias.
func DecodeYahooFinanceChart(body []byte) (MarketQuote, error) {
	var payload struct {
		Chart struct {
			Result []struct {
				Meta struct {
					Symbol             string  `json:"symbol"`
					Currency           string  `json:"currency"`
					RegularMarketPrice float64 `json:"regularMarketPrice"`
					PreviousClose      float64 `json:"previousClose"`
					ChartPreviousClose float64 `json:"chartPreviousClose"`
					RegularMarketTime  int64   `json:"regularMarketTime"`
					ExchangeName       string  `json:"exchangeName"`
				} `json:"meta"`
				Timestamps []int64 `json:"timestamp"`
			} `json:"result"`
			Error json.RawMessage `json:"error"`
		} `json:"chart"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Chart.Result) != 1 {
		return MarketQuote{}, fmt.Errorf("invalid Yahoo chart response")
	}
	item := payload.Chart.Result[0]
	meta := item.Meta
	exchange := strings.ToUpper(strings.TrimSpace(meta.ExchangeName))
	if !strings.EqualFold(meta.Symbol, yahooSymbol) || !strings.EqualFold(meta.Currency, "CAD") || (exchange != "TOR" && exchange != "TORONTO") {
		return MarketQuote{}, fmt.Errorf("Yahoo listing identity mismatch")
	}
	previous := meta.ChartPreviousClose
	if previous == 0 {
		previous = meta.PreviousClose
	}
	price := meta.RegularMarketPrice
	if !finitePositive(price) || !finitePositive(previous) || meta.RegularMarketTime <= 0 {
		return MarketQuote{}, fmt.Errorf("Yahoo quote fields are invalid")
	}
	updated := time.Unix(meta.RegularMarketTime, 0).UTC()
	if updated.After(time.Now().Add(24 * time.Hour)) {
		return MarketQuote{}, fmt.Errorf("Yahoo quote timestamp is invalid")
	}
	change := price - previous
	return MarketQuote{
		Symbol: yahooDisplay, DisplayName: "Plaza Retail REIT", Price: price,
		PreviousClose: previous, AbsoluteChange: change,
		PercentageChange: change / previous * 100, Exchange: "Toronto Stock Exchange",
		MIC: "XTSE", Currency: "CAD", MarketStatus: MarketUnknown,
		QuoteTimestamp: updated, ProviderUpdated: updated,
		// Yahoo's payload does not provide a verifiable delay entitlement/value.
		// Always label it conservatively instead of suggesting real-time pricing.
		Delayed: true,
	}, nil
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func yahooDiagnosticReason(err error) string {
	var classified SanitizedError
	if !errors.As(err, &classified) {
		return "BAD_RESPONSE"
	}
	switch classified.Code {
	case "provider_rate_limited":
		return "RATE_LIMITED"
	case "provider_network_error":
		return "NETWORK_ERROR"
	case "provider_response_invalid", "invalid_symbol":
		return "BAD_RESPONSE"
	default:
		return "PROVIDER_ERROR"
	}
}

func (a *YahooFinanceAdapter) QuoteDiagnostics(ctx context.Context, request MarketRequest) ([]MarketQuoteDiagnostic, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	symbol := yahooDisplay
	if request.Listings != nil {
		if len(request.Listings) != 1 || !isYahooPLZListing(request.Listings[0]) {
			return nil, SanitizedError{Code: "invalid_symbol", Message: "Unsupported experimental listing", Retryable: false}
		}
	} else if len(request.Symbols) != 1 || !strings.EqualFold(request.Symbols[0], yahooDisplay) {
		return nil, SanitizedError{Code: "invalid_symbol", Message: "Unsupported experimental listing", Retryable: false}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return []MarketQuoteDiagnostic{{Symbol: symbol, LastSuccessfulQuoteTimestamp: a.lastSuccess, LastRefreshAttempt: a.lastAttempt, FailureReason: a.failure, ProviderID: yahooProviderID, Stale: a.stale, RetryBackoffActive: time.Now().Before(a.backoffUntil), RetryAt: a.backoffUntil}}, nil
}
