package providers

import (
	"context"
	"errors"
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
}

func (request MarketRequest) Validate() error {
	if len(request.Symbols) == 0 || len(request.Symbols) > 10 {
		return errors.New("market request must contain between 1 and 10 symbols")
	}
	seen := map[string]bool{}
	for _, symbol := range request.Symbols {
		symbol = strings.ToUpper(strings.TrimSpace(symbol))
		if symbol == "" || len(symbol) > 16 || seen[symbol] {
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
