package providers

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

type MarketStatus string

const (
	MarketOpen    MarketStatus = "open"
	MarketClosed  MarketStatus = "closed"
	MarketPre     MarketStatus = "pre_market"
	MarketAfter   MarketStatus = "after_hours"
	MarketUnknown MarketStatus = "unknown"
)

type MarketQuote struct {
	ListingID        string       `json:"listingID,omitempty"`
	LogoData         string       `json:"logoData,omitempty"`
	ErrorCode        string       `json:"errorCode,omitempty"`
	EOD              bool         `json:"eod,omitempty"`
	Symbol           string       `json:"symbol"`
	DisplayName      string       `json:"displayName"`
	Price            float64      `json:"price"`
	AbsoluteChange   float64      `json:"absoluteChange"`
	PercentageChange float64      `json:"percentageChange"`
	Exchange         string       `json:"exchange,omitempty"`
	MIC              string       `json:"mic,omitempty"`
	Currency         string       `json:"currency,omitempty"`
	MarketStatus     MarketStatus `json:"marketStatus"`
	QuoteTimestamp   time.Time    `json:"quoteTimestamp"`
	ProviderUpdated  time.Time    `json:"providerUpdated"`
	// Delayed is set only when Twelve Data explicitly identifies the quote as
	// end-of-day or delayed. It is distinct from Stale, which means the server
	// is showing a bounded last-known-good response after a failed refresh.
	Delayed bool `json:"delayed"`
	Stale   bool `json:"stale"`
}

type MarketRequest struct {
	Symbols      []string
	Listings     []MarketListing
	CredentialID string
	ScopeType    string
	ScopeID      string
}

var marketSymbolPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9.-]{0,14}(:[A-Z0-9][A-Z0-9.-]{0,14})?$`)

func NormalizeMarketSymbol(value string) (string, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	if !marketSymbolPattern.MatchString(value) {
		return "", errors.New("market symbol is invalid")
	}
	return value, nil
}

func (request MarketRequest) Validate() error {
	if request.Listings != nil {
		raw, _ := json.Marshal(request.Listings)
		_, err := ParseMarketWatchlist(string(raw))
		return err
	}
	if len(request.Symbols) == 0 || len(request.Symbols) > MaxMarketSymbols {
		return SanitizedError{Code: "market_symbol_limit", Message: "Choose between 1 and 5 market symbols", Retryable: false}
	}
	seen := map[string]bool{}
	for _, symbol := range request.Symbols {
		var err error
		symbol, err = NormalizeMarketSymbol(symbol)
		if err != nil || seen[symbol] {
			return SanitizedError{Code: "invalid_symbol", Message: "A market symbol is not available", Retryable: false}
		}
		seen[symbol] = true
	}
	return nil
}

// MaxMarketSymbols keeps the default five-minute open-market refresh within a
// Twelve Data Basic-plan daily credit budget for a personal display.
const MaxMarketSymbols = 5

type MarketProvider interface {
	Quotes(context.Context, MarketRequest) ([]MarketQuote, error)
}

type Location struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Timezone  string  `json:"timezone"`
	Label     string  `json:"label,omitempty"`
}

type WeatherRequest struct {
	Location     Location
	Units        string
	CredentialID string
	ScopeType    string
	ScopeID      string
}

func (request WeatherRequest) Validate() error {
	if request.Location.Latitude < -90 || request.Location.Latitude > 90 || request.Location.Longitude < -180 || request.Location.Longitude > 180 {
		return errors.New("weather location is invalid")
	}
	if request.Units != "metric" && request.Units != "imperial" {
		return errors.New("weather units must be metric or imperial")
	}
	return nil
}

type WeatherCondition struct {
	Timestamp                time.Time `json:"timestamp"`
	Summary                  string    `json:"summary"`
	Temperature              float64   `json:"temperature"`
	FeelsLike                float64   `json:"feelsLike"`
	DailyHigh                float64   `json:"dailyHigh"`
	DailyLow                 float64   `json:"dailyLow"`
	PrecipitationProbability float64   `json:"precipitationProbability"`
}

type HourlyForecast struct {
	Timestamp                time.Time `json:"timestamp"`
	Temperature              float64   `json:"temperature"`
	Summary                  string    `json:"summary"`
	PrecipitationProbability float64   `json:"precipitationProbability"`
}

type WeatherAlert struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Severity  string    `json:"severity"`
	StartsAt  time.Time `json:"startsAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type WeatherSnapshot struct {
	Location        Location         `json:"location"`
	Current         WeatherCondition `json:"current"`
	Hourly          []HourlyForecast `json:"hourly"`
	Alerts          []WeatherAlert   `json:"alerts"`
	ProviderUpdated time.Time        `json:"providerUpdated"`
	Stale           bool             `json:"stale"`
}

type WeatherProvider interface {
	Weather(context.Context, WeatherRequest) (WeatherSnapshot, error)
}

type CredentialResolver interface {
	Resolve(context.Context, string, string, string) (string, error)
}

type CredentialValidator interface {
	ValidateCredential(context.Context, string, string, string) error
}

type SanitizedError struct {
	Code      string
	Message   string
	Retryable bool
}

func (e SanitizedError) Error() string { return e.Message }

func MissingCredential(provider string) error {
	return SanitizedError{Code: "provider_credential_missing", Message: provider + " is not configured on this server", Retryable: false}
}

func ProviderSetupRequired(provider string) error {
	return SanitizedError{Code: "provider_setup_required", Message: provider + " setup is required on this server", Retryable: false}
}

func TemporarilyUnavailable() error {
	return SanitizedError{Code: "provider_temporarily_unavailable", Message: "Provider data is temporarily unavailable", Retryable: true}
}
