package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	marketOpenFreshTTL     = 5 * time.Minute
	marketClosedFreshTTL   = 45 * time.Minute
	marketStaleTTL         = 30 * time.Minute
	marketRateLimitBackoff = 15 * time.Minute
)

type TwelveDataAdapter struct {
	Credentials  CredentialResolver
	Client       *http.Client
	BaseURL      string
	Cache        *Cache[[]MarketQuote]
	backoffMu    sync.Mutex
	backoffUntil map[string]time.Time
}

func NewTwelveDataAdapter(credentials CredentialResolver, client *http.Client) *TwelveDataAdapter {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &TwelveDataAdapter{
		Credentials: credentials, Client: client, BaseURL: "https://api.twelvedata.com",
		// Five minutes prevents a device polling every few seconds from spending
		// a Basic-plan quote credit on every render after an upstream failure.
		Cache:        NewCache[[]MarketQuote](marketOpenFreshTTL),
		backoffUntil: make(map[string]time.Time),
	}
}

func (adapter *TwelveDataAdapter) Quotes(ctx context.Context, request MarketRequest) ([]MarketQuote, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	normalized := make([]string, 0, len(request.Symbols))
	for _, symbol := range request.Symbols {
		value, _ := NormalizeMarketSymbol(symbol)
		normalized = append(normalized, value)
	}
	key := request.ScopeType + ":" + request.ScopeID + ":" + request.CredentialID + ":" + strings.Join(normalized, ",")
	backoffKey := request.ScopeType + ":" + request.ScopeID + ":" + request.CredentialID
	quotes, stale, err := adapter.Cache.GetWithTTL(ctx, key, marketStaleTTL, func(ctx context.Context) ([]MarketQuote, time.Duration, error) {
		if adapter.rateLimited(backoffKey) {
			return nil, 0, SanitizedError{Code: "provider_rate_limited", Message: "Market data is temporarily rate limited", Retryable: true}
		}
		secret, err := adapter.Credentials.Resolve(ctx, request.CredentialID, request.ScopeType, request.ScopeID)
		if err != nil {
			return nil, 0, MissingCredential("Market data")
		}
		result := make([]MarketQuote, 0, len(normalized))
		for _, symbol := range normalized {
			quote, err := adapter.fetchQuote(ctx, symbol, secret)
			if err != nil {
				adapter.recordBackoff(backoffKey, err)
				return nil, 0, err
			}
			result = append(result, quote)
		}
		return result, quoteFreshTTL(result), nil
	})
	if err != nil {
		return nil, err
	}
	// The cached slice is shared. Copy it before annotating a stale fallback so
	// a later fresh request can never inherit the stale marker.
	quotes = append([]MarketQuote(nil), quotes...)
	if stale {
		for index := range quotes {
			quotes[index].Stale = true
		}
	}
	return quotes, nil
}

func quoteFreshTTL(quotes []MarketQuote) time.Duration {
	for _, quote := range quotes {
		if quote.MarketStatus == MarketOpen {
			return marketOpenFreshTTL
		}
	}
	return marketClosedFreshTTL
}

func (adapter *TwelveDataAdapter) rateLimited(key string) bool {
	adapter.backoffMu.Lock()
	defer adapter.backoffMu.Unlock()
	until := adapter.backoffUntil[key]
	if time.Now().Before(until) {
		return true
	}
	delete(adapter.backoffUntil, key)
	return false
}

func (adapter *TwelveDataAdapter) recordBackoff(key string, err error) {
	var sanitized SanitizedError
	if !errors.As(err, &sanitized) || sanitized.Code != "provider_rate_limited" {
		return
	}
	adapter.backoffMu.Lock()
	adapter.backoffUntil[key] = time.Now().Add(marketRateLimitBackoff)
	adapter.backoffMu.Unlock()
}

func (adapter *TwelveDataAdapter) ValidateCredential(ctx context.Context, id, scopeType, scopeID string) error {
	secret, err := adapter.Credentials.Resolve(ctx, id, scopeType, scopeID)
	if err != nil {
		return MissingCredential("Market data")
	}
	_, err = adapter.fetchQuote(ctx, "AAPL", secret)
	return err
}

func (adapter *TwelveDataAdapter) fetchQuote(ctx context.Context, symbol, secret string) (MarketQuote, error) {
	endpoint, _ := url.Parse(strings.TrimRight(adapter.BaseURL, "/") + "/quote")
	query := endpoint.Query()
	query.Set("symbol", symbol)
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return MarketQuote{}, TemporarilyUnavailable()
	}
	req.Header.Set("Authorization", "apikey "+secret)
	resp, err := adapter.Client.Do(req)
	if err != nil {
		return MarketQuote{}, TemporarilyUnavailable()
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return MarketQuote{}, SanitizedError{Code: "provider_rate_limited", Message: "Market data is temporarily rate limited", Retryable: true}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return MarketQuote{}, SanitizedError{Code: "provider_credential_invalid", Message: "The market credential could not be validated", Retryable: false}
	}
	if resp.StatusCode == http.StatusForbidden {
		return MarketQuote{}, SanitizedError{Code: "provider_entitlement_required", Message: "The market symbol is not available on this provider plan", Retryable: false}
	}
	if resp.StatusCode != http.StatusOK {
		return MarketQuote{}, TemporarilyUnavailable()
	}
	var payload struct {
		Symbol        string `json:"symbol"`
		Name          string `json:"name"`
		Exchange      string `json:"exchange"`
		MIC           string `json:"mic_code"`
		LegacyMIC     string `json:"mic"`
		Currency      string `json:"currency"`
		Close         string `json:"close"`
		Change        string `json:"change"`
		PercentChange string `json:"percent_change"`
		Timestamp     int64  `json:"timestamp"`
		LastUpdateAt  int64  `json:"last_update_at"`
		LastQuoteAt   int64  `json:"last_quote_at"`
		MarketOpen    bool   `json:"is_market_open"`
		IsEOD         bool   `json:"is_eod"`
		IsDelayed     bool   `json:"is_delayed"`
		Status        string `json:"status"`
		Code          int    `json:"code"`
		Message       string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return MarketQuote{}, SanitizedError{Code: "provider_response_invalid", Message: "Market data could not be read", Retryable: true}
	}
	if payload.Status == "error" || payload.Code != 0 {
		lowerMessage := strings.ToLower(payload.Message)
		switch payload.Code {
		case http.StatusTooManyRequests:
			return MarketQuote{}, SanitizedError{Code: "provider_rate_limited", Message: "Market data is temporarily rate limited", Retryable: true}
		case http.StatusUnauthorized:
			if containsAny(lowerMessage, "plan", "premium", "subscription", "not available", "access") {
				return MarketQuote{}, SanitizedError{Code: "provider_entitlement_required", Message: "The market symbol is not available on this provider plan", Retryable: false}
			}
			return MarketQuote{}, SanitizedError{Code: "provider_credential_invalid", Message: "The market credential could not be validated", Retryable: false}
		case http.StatusForbidden:
			return MarketQuote{}, SanitizedError{Code: "provider_entitlement_required", Message: "The market symbol is not available on this provider plan", Retryable: false}
		}
		if payload.Code == http.StatusBadRequest || payload.Code == http.StatusNotFound || strings.Contains(lowerMessage, "symbol") {
			return MarketQuote{}, SanitizedError{Code: "invalid_symbol", Message: "A market symbol is not available", Retryable: false}
		}
		return MarketQuote{}, TemporarilyUnavailable()
	}
	price, priceErr := strconv.ParseFloat(payload.Close, 64)
	change, changeErr := strconv.ParseFloat(payload.Change, 64)
	percentage, percentageErr := strconv.ParseFloat(payload.PercentChange, 64)
	if priceErr != nil || changeErr != nil || percentageErr != nil || payload.Symbol == "" {
		return MarketQuote{}, SanitizedError{Code: "provider_response_invalid", Message: "Market data could not be read", Retryable: true}
	}
	timestamp := payload.LastUpdateAt
	if timestamp == 0 {
		timestamp = payload.LastQuoteAt
	}
	if timestamp == 0 {
		timestamp = payload.Timestamp
	}
	if timestamp <= 0 {
		return MarketQuote{}, SanitizedError{Code: "provider_response_invalid", Message: "Market data could not be read", Retryable: true}
	}
	updated := time.Unix(timestamp, 0).UTC()
	status := MarketClosed
	if payload.MarketOpen {
		status = MarketOpen
	}
	responseSymbol, normalizeErr := NormalizeMarketSymbol(payload.Symbol)
	if normalizeErr != nil {
		responseSymbol = symbol
	}
	// Twelve Data may return an unqualified symbol even when the request used
	// an exchange suffix. Preserve the configured stable display symbol.
	if strings.Contains(symbol, ":") && !strings.Contains(responseSymbol, ":") {
		responseSymbol = symbol
	}
	mic := payload.MIC
	if mic == "" {
		mic = payload.LegacyMIC
	}
	return MarketQuote{
		Symbol: responseSymbol, DisplayName: payload.Name, Price: price, AbsoluteChange: change,
		PercentageChange: percentage, Exchange: payload.Exchange, MIC: mic, Currency: payload.Currency,
		MarketStatus: status, QuoteTimestamp: updated, ProviderUpdated: updated,
		Delayed: payload.IsEOD || payload.IsDelayed,
	}, nil
}

func (adapter *TwelveDataAdapter) String() string {
	return fmt.Sprintf("TwelveDataAdapter(%s)", adapter.BaseURL)
}
