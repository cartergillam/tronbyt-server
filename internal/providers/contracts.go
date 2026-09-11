package providers

import (
	"context"
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
	Symbol           string       `json:"symbol"`
	DisplayName      string       `json:"displayName"`
	Price            float64      `json:"price"`
	AbsoluteChange   float64      `json:"absoluteChange"`
	PercentageChange float64      `json:"percentageChange"`
	MarketStatus     MarketStatus `json:"marketStatus"`
	LogoAssetURL     string       `json:"logoAssetURL,omitempty"`
	QuoteTimestamp   time.Time    `json:"quoteTimestamp"`
	ProviderUpdated  time.Time    `json:"providerUpdated"`
	Stale            bool         `json:"stale"`
}

type MarketRequest struct {
	Symbols      []string
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
	if len(request.Symbols) == 0 || len(request.Symbols) > 10 {
		return errors.New("market request must contain between 1 and 10 symbols")
	}
	seen := map[string]bool{}
	for _, symbol := range request.Symbols {
		var err error
		symbol, err = NormalizeMarketSymbol(symbol)
		if err != nil || seen[symbol] {
			return errors.New("market symbols must be unique and valid")
		}
		seen[symbol] = true
	}
	return nil
}

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

func TemporarilyUnavailable() error {
	return SanitizedError{Code: "provider_temporarily_unavailable", Message: "Provider data is temporarily unavailable", Retryable: true}
}
