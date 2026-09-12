package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	marketFailureRetry     = 30 * time.Second
)

type marketFailure struct {
	err   error
	until time.Time
}

type TwelveDataAdapter struct {
	Credentials  CredentialResolver
	Client       *http.Client
	BaseURL      string
	Cache        *Cache[[]MarketQuote]
	backoffMu    sync.Mutex
	backoffUntil map[string]time.Time
	failureMu    sync.Mutex
	failures     map[string]marketFailure
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
		failures:     make(map[string]marketFailure),
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
	if err := adapter.recentFailure(key); err != nil {
		return nil, err
	}
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
		adapter.recordFailure(key, err)
		return nil, err
	}
	adapter.clearFailure(key)
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
	if err := adapter.validateAAPL(ctx, secret); err != nil {
		return err
	}
	// A successfully validated replacement credential should be usable on the
	// next render, rather than waiting for a previous local error throttle.
	adapter.clearCredentialState(scopeType, scopeID, id)
	return nil
}

func (adapter *TwelveDataAdapter) fetchQuote(ctx context.Context, symbol, secret string) (MarketQuote, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return MarketQuote{}, MissingCredential("Market data")
	}
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return MarketQuote{}, SanitizedError{Code: "provider_response_invalid", Message: "Market data could not be read", Retryable: true}
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
	if err := json.Unmarshal(body, &payload); err != nil {
		return MarketQuote{}, SanitizedError{Code: "provider_response_invalid", Message: "Market data could not be read", Retryable: true}
	}
	if err := twelveDataError(resp.StatusCode, payload.Code, payload.Status, payload.Message); err != nil {
		return MarketQuote{}, err
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

func (adapter *TwelveDataAdapter) validateAAPL(ctx context.Context, secret string) error {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return MissingCredential("Market data")
	}
	endpoint, _ := url.Parse(strings.TrimRight(adapter.BaseURL, "/") + "/time_series")
	query := endpoint.Query()
	query.Set("symbol", "AAPL")
	query.Set("interval", "1day")
	query.Set("outputsize", "1")
	// This is the lowest-cost, Basic-compatible endpoint and matches the
	// documented request shape used for manual credential validation.
	query.Set("apikey", secret)
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return TemporarilyUnavailable()
	}
	resp, err := adapter.Client.Do(req)
	if err != nil {
		return TemporarilyUnavailable()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return SanitizedError{Code: "provider_response_invalid", Message: "Market data could not be read", Retryable: true}
	}
	var payload struct {
		Meta struct {
			Symbol string `json:"symbol"`
		} `json:"meta"`
		Values []struct {
			Close string `json:"close"`
		} `json:"values"`
		Status  string `json:"status"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return SanitizedError{Code: "provider_response_invalid", Message: "Market data could not be read", Retryable: true}
	}
	if err := twelveDataError(resp.StatusCode, payload.Code, payload.Status, payload.Message); err != nil {
		return err
	}
	if payload.Meta.Symbol == "" || len(payload.Values) != 1 {
		return SanitizedError{Code: "provider_response_invalid", Message: "Market data could not be read", Retryable: true}
	}
	if _, err := strconv.ParseFloat(payload.Values[0].Close, 64); err != nil {
		return SanitizedError{Code: "provider_response_invalid", Message: "Market data could not be read", Retryable: true}
	}
	return nil
}

func twelveDataError(httpStatus, providerCode int, status, message string) error {
	if httpStatus == http.StatusOK && status != "error" && providerCode == 0 {
		return nil
	}
	code := providerCode
	if code == 0 {
		code = httpStatus
	}
	lowerMessage := strings.ToLower(message)
	switch code {
	case http.StatusTooManyRequests:
		return SanitizedError{Code: "provider_rate_limited", Message: "Market data is temporarily rate limited", Retryable: true}
	case http.StatusUnauthorized:
		if containsAny(lowerMessage, "plan", "premium", "subscription", "not available", "access") {
			return SanitizedError{Code: "provider_entitlement_required", Message: "The market symbol is not available on this provider plan", Retryable: false}
		}
		return SanitizedError{Code: "provider_credential_invalid", Message: "The market credential could not be validated", Retryable: false}
	case http.StatusForbidden:
		return SanitizedError{Code: "provider_entitlement_required", Message: "The market symbol is not available on this provider plan", Retryable: false}
	case http.StatusBadRequest, http.StatusNotFound:
		return SanitizedError{Code: "invalid_symbol", Message: "A market symbol is not available", Retryable: false}
	}
	if strings.Contains(lowerMessage, "symbol") {
		return SanitizedError{Code: "invalid_symbol", Message: "A market symbol is not available", Retryable: false}
	}
	return TemporarilyUnavailable()
}

func (adapter *TwelveDataAdapter) recordFailure(key string, err error) {
	adapter.failureMu.Lock()
	adapter.failures[key] = marketFailure{err: err, until: time.Now().Add(marketFailureRetry)}
	adapter.failureMu.Unlock()
}

func (adapter *TwelveDataAdapter) recentFailure(key string) error {
	adapter.failureMu.Lock()
	failure, found := adapter.failures[key]
	if !found {
		adapter.failureMu.Unlock()
		return nil
	}
	if time.Now().Before(failure.until) {
		adapter.failureMu.Unlock()
		return failure.err
	}
	delete(adapter.failures, key)
	adapter.failureMu.Unlock()
	// Cache's normal request throttle must not replace an expired classified
	// provider failure with a generic local throttling error.
	adapter.Cache.mu.Lock()
	delete(adapter.Cache.lastRun, key)
	adapter.Cache.mu.Unlock()
	return nil
}

func (adapter *TwelveDataAdapter) clearFailure(key string) {
	adapter.failureMu.Lock()
	delete(adapter.failures, key)
	adapter.failureMu.Unlock()
}

func (adapter *TwelveDataAdapter) clearCredentialState(scopeType, scopeID, credentialID string) {
	prefix := scopeType + ":" + scopeID + ":" + credentialID + ":"
	adapter.failureMu.Lock()
	for key := range adapter.failures {
		if strings.HasPrefix(key, prefix) {
			delete(adapter.failures, key)
		}
	}
	adapter.failureMu.Unlock()
	adapter.Cache.mu.Lock()
	for key := range adapter.Cache.entries {
		if strings.HasPrefix(key, prefix) {
			delete(adapter.Cache.entries, key)
		}
	}
	for key := range adapter.Cache.lastRun {
		if strings.HasPrefix(key, prefix) {
			delete(adapter.Cache.lastRun, key)
		}
	}
	adapter.Cache.mu.Unlock()
}

func (adapter *TwelveDataAdapter) String() string {
	return fmt.Sprintf("TwelveDataAdapter(%s)", adapter.BaseURL)
}
