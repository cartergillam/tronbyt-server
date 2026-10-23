package providers

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
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
	LogoVersion      string       `json:"logoVersion,omitempty"`
	LogoData         string       `json:"logoData,omitempty"`
	ErrorCode        string       `json:"errorCode,omitempty"`
	EOD              bool         `json:"eod,omitempty"`
	Symbol           string       `json:"symbol"`
	DisplayName      string       `json:"displayName"`
	Price            float64      `json:"price"`
	PreviousClose    float64      `json:"previousClose,omitempty"`
	AbsoluteChange   float64      `json:"absoluteChange"`
	PercentageChange float64      `json:"percentageChange"`
	Exchange         string       `json:"exchange,omitempty"`
	MIC              string       `json:"mic,omitempty"`
	Currency         string       `json:"currency,omitempty"`
	MarketStatus     MarketStatus `json:"marketStatus"`
	QuoteTimestamp   time.Time    `json:"quoteTimestamp"`
	ProviderUpdated  time.Time    `json:"providerUpdated"`
	// Delayed marks an explicitly delayed/EOD quote, or a conservative label
	// when a provider's freshness cannot be verified. It is distinct from Stale.
	Delayed bool `json:"delayed"`
	Stale   bool `json:"stale"`
}

type MarketRequest struct {
	Symbols      []string
	Listings     []MarketListing
	CredentialID string
	ScopeType    string
	ScopeID      string
	MaxSymbols   int
}

// MarketQuoteDiagnostic is an owner-only operational summary. It deliberately
// contains logical credential identity and sanitized categories only; never
// provider response text, request data, or credential material.
type MarketQuoteDiagnostic struct {
	Symbol                       string    `json:"symbol"`
	LastSuccessfulQuoteTimestamp time.Time `json:"lastSuccessfulQuoteTimestamp,omitzero"`
	LastRefreshAttempt           time.Time `json:"lastRefreshAttempt,omitzero"`
	FailureReason                string    `json:"failureReason,omitempty"`
	ProviderID                   string    `json:"providerID"`
	CredentialID                 string    `json:"credentialID,omitempty"`
	Stale                        bool      `json:"stale"`
	RetryBackoffActive           bool      `json:"retryBackoffActive"`
	RetryAt                      time.Time `json:"retryAt,omitzero"`
}

// MarketQuoteDiagnosticProvider reads adapter cache state and must not refresh
// quotes as a side effect.
type MarketQuoteDiagnosticProvider interface {
	QuoteDiagnostics(ctx context.Context, request MarketRequest) ([]MarketQuoteDiagnostic, error)
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
		_, err := ParseMarketWatchlistLimit(string(raw), request.MaxSymbols)
		return err
	}
	maxSymbols := request.MaxSymbols
	if maxSymbols < 1 || maxSymbols > MaxMarketSymbols {
		maxSymbols = MaxMarketSymbols
	}
	if len(request.Symbols) == 0 || len(request.Symbols) > maxSymbols {
		return SanitizedError{Code: "market_symbol_limit", Message: "Choose between 1 and " + strconv.Itoa(maxSymbols) + " market symbols to protect shared market-data limits", Retryable: false}
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

// MaxMarketSymbols is the safe hard ceiling for a Market Watch installation.
// Administrators may configure a stricter limit, but cannot raise this cap.
const MaxMarketSymbols = 8

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
	DailyOnly    bool // Resource intent, not a cache identity or provider-specific mode.
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
