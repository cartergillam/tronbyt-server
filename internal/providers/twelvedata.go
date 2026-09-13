package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	marketOpenFreshTTL     = 5 * time.Minute
	marketClosedFreshTTL   = 60 * time.Minute
	marketStaleTTL         = 72 * time.Hour
	marketRateLimitBackoff = 15 * time.Minute
	marketFailureRetry     = 30 * time.Second
)

type marketFailure struct {
	err   error
	until time.Time
}

type TwelveDataAdapter struct {
	quoteMu      sync.Mutex // Coalesce overlapping watchlists before deciding which listings need a batch.
	usageMu      sync.Mutex
	usage        map[[32]byte]marketUsage
	diagnostics  MarketDiagnostics
	Credentials  CredentialResolver
	Client       *http.Client
	BaseURL      string
	Cache        *Cache[[]MarketQuote]
	SearchCache  *Cache[[]MarketListing]
	LogoCache    *Cache[string]
	LogoImages   *marketLogoImages
	searchMu     sync.Mutex
	searchLast   map[string]time.Time
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
		Credentials: credentials, Client: client, BaseURL: "https://api.twelvedata.com", usage: make(map[[32]byte]marketUsage),
		// Listing snapshots are shared across renders and installations within credential scope.
		Cache:        NewCache[[]MarketQuote](marketOpenFreshTTL),
		SearchCache:  NewCache[[]MarketListing](time.Second),
		LogoCache:    NewCache[string](time.Minute),
		LogoImages:   newMarketLogoImages(client),
		searchLast:   make(map[string]time.Time),
		backoffUntil: make(map[string]time.Time),
		failures:     make(map[string]marketFailure),
	}
}

func (adapter *TwelveDataAdapter) Quotes(ctx context.Context, request MarketRequest) ([]MarketQuote, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return adapter.listingQuotes(ctx, request)
}

func quoteFreshTTL(quotes []MarketQuote) time.Duration {
	for _, quote := range quotes {
		if quote.MarketStatus == MarketOpen || quote.ErrorCode != "" {
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
	var deferred marketCreditDeferred
	if errors.As(err, &deferred) {
		return
	}
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

func listingQuery(listing MarketListing) url.Values {
	query := url.Values{"symbol": {listing.Symbol}}
	if listing.MIC != "" {
		query.Set("mic_code", listing.MIC)
	} else if listing.Exchange != "" {
		query.Set("exchange", listing.Exchange)
	}
	return query
}

func decodeMarketQuote(body []byte, httpStatus int, symbol string) (MarketQuote, error) {
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
		MarketOpen    *bool  `json:"is_market_open"`
		IsEOD         bool   `json:"is_eod"`
		IsDelayed     bool   `json:"is_delayed"`
		Status        string `json:"status"`
		Code          int    `json:"code"`
		Message       string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return MarketQuote{}, SanitizedError{Code: "provider_response_invalid", Message: "Market data could not be read", Retryable: true}
	}
	if err := twelveDataError(httpStatus, payload.Code, payload.Status, payload.Message); err != nil {
		return MarketQuote{}, err
	}
	price, priceErr := strconv.ParseFloat(payload.Close, 64)
	change, changeErr := strconv.ParseFloat(payload.Change, 64)
	percentage, percentageErr := strconv.ParseFloat(payload.PercentChange, 64)
	if priceErr != nil || changeErr != nil || percentageErr != nil || math.IsNaN(price) || math.IsNaN(change) || math.IsNaN(percentage) || math.IsInf(price, 0) || math.IsInf(change, 0) || math.IsInf(percentage, 0) || payload.Symbol == "" {
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
	status := MarketUnknown
	if payload.MarketOpen != nil {
		status = MarketClosed
		if *payload.MarketOpen {
			status = MarketOpen
		}
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
		Delayed: payload.IsEOD || payload.IsDelayed, EOD: payload.IsEOD,
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
	if !adapter.reserveCredits(secret, 1, false) {
		return marketQuotaError()
	}
	resp, err := adapter.Client.Do(req)
	if err != nil {
		return TemporarilyUnavailable()
	}
	adapter.observeCredits(secret, resp.Header)
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
	// Entitlement can be reported as a 400/404, including a valid Canadian listing.
	if code != http.StatusTooManyRequests && containsAny(lowerMessage, "upgrade", "subscription", "premium", "plan", "not included", "not available on", "access to this") {
		return SanitizedError{Code: "provider_entitlement_required", Message: "The market symbol is not available on this provider plan", Retryable: false}
	}

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
	retry := marketFailureRetry
	var deferred marketCreditDeferred
	if errors.As(err, &deferred) {
		retry = time.Minute
	}
	var classified SanitizedError
	if errors.As(err, &classified) && !classified.Retryable {
		retry = 24 * time.Hour
		if classified.Code == "provider_credential_invalid" {
			retry = 15 * time.Minute
		}
	}
	adapter.failures[key] = marketFailure{err: err, until: time.Now().Add(retry)}
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
	scope := scopeType + ":" + scopeID + ":" + credentialID
	adapter.backoffMu.Lock()
	delete(adapter.backoffUntil, scope)
	adapter.backoffMu.Unlock()
	prefix := scope + ":"
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
