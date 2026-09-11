package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type TwelveDataAdapter struct {
	Credentials CredentialResolver
	Client      *http.Client
	BaseURL     string
	Cache       *Cache[[]MarketQuote]
}

func NewTwelveDataAdapter(credentials CredentialResolver, client *http.Client) *TwelveDataAdapter {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &TwelveDataAdapter{
		Credentials: credentials, Client: client, BaseURL: "https://api.twelvedata.com",
		Cache: NewCache[[]MarketQuote](time.Second),
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
	key := request.ScopeType + ":" + request.ScopeID + ":" + strings.Join(normalized, ",")
	quotes, stale, err := adapter.Cache.Get(ctx, key, 60*time.Second, 30*time.Minute, func(ctx context.Context) ([]MarketQuote, error) {
		secret, err := adapter.Credentials.Resolve(ctx, request.CredentialID, request.ScopeType, request.ScopeID)
		if err != nil {
			return nil, MissingCredential("Market data")
		}
		result := make([]MarketQuote, 0, len(normalized))
		for _, symbol := range normalized {
			quote, err := adapter.fetchQuote(ctx, symbol, secret)
			if err != nil {
				return nil, err
			}
			result = append(result, quote)
		}
		return result, nil
	})
	if err != nil {
		return nil, err
	}
	if stale {
		for index := range quotes {
			quotes[index].Stale = true
		}
	}
	return quotes, nil
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
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return MarketQuote{}, SanitizedError{Code: "provider_credential_invalid", Message: "The market credential could not be validated", Retryable: false}
	}
	if resp.StatusCode != http.StatusOK {
		return MarketQuote{}, TemporarilyUnavailable()
	}
	var payload struct {
		Symbol        string `json:"symbol"`
		Name          string `json:"name"`
		Close         string `json:"close"`
		Change        string `json:"change"`
		PercentChange string `json:"percent_change"`
		Timestamp     int64  `json:"timestamp"`
		LastUpdateAt  int64  `json:"last_update_at"`
		MarketOpen    bool   `json:"is_market_open"`
		Status        string `json:"status"`
		Code          int    `json:"code"`
		Message       string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || payload.Status == "error" || payload.Code != 0 {
		if payload.Code == 400 || strings.Contains(strings.ToLower(payload.Message), "symbol") {
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
		timestamp = payload.Timestamp
	}
	updated := time.Unix(timestamp, 0).UTC()
	status := MarketClosed
	if payload.MarketOpen {
		status = MarketOpen
	}
	return MarketQuote{
		Symbol: payload.Symbol, DisplayName: payload.Name, Price: price, AbsoluteChange: change,
		PercentageChange: percentage, MarketStatus: status, QuoteTimestamp: updated,
		ProviderUpdated: updated,
	}, nil
}

func (adapter *TwelveDataAdapter) String() string {
	return fmt.Sprintf("TwelveDataAdapter(%s)", adapter.BaseURL)
}
