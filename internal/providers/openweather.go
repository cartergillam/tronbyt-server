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

type OpenWeatherAdapter struct {
	Credentials CredentialResolver
	Client      *http.Client
	BaseURL     string
	Cache       *Cache[WeatherSnapshot]
}

func NewOpenWeatherAdapter(credentials CredentialResolver, client *http.Client) *OpenWeatherAdapter {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &OpenWeatherAdapter{
		Credentials: credentials, Client: client, BaseURL: "https://api.openweathermap.org/data/3.0/onecall",
		Cache: NewCache[WeatherSnapshot](time.Second),
	}
}

func (adapter *OpenWeatherAdapter) Weather(ctx context.Context, request WeatherRequest) (WeatherSnapshot, error) {
	if err := request.Validate(); err != nil {
		return WeatherSnapshot{}, err
	}
	key := fmt.Sprintf("%s:%s:%.4f,%.4f:%s", request.ScopeType, request.ScopeID, request.Location.Latitude, request.Location.Longitude, request.Units)
	snapshot, stale, err := adapter.Cache.Get(ctx, key, 10*time.Minute, 2*time.Hour, func(ctx context.Context) (WeatherSnapshot, error) {
		secret, err := adapter.Credentials.Resolve(ctx, request.CredentialID, request.ScopeType, request.ScopeID)
		if err != nil {
			return WeatherSnapshot{}, MissingCredential("OpenWeather")
		}
		return adapter.fetch(ctx, request.Location, request.Units, secret)
	})
	if err != nil {
		return WeatherSnapshot{}, err
	}
	snapshot.Stale = stale
	return snapshot, nil
}

func (adapter *OpenWeatherAdapter) ValidateCredential(ctx context.Context, id, scopeType, scopeID string) error {
	secret, err := adapter.Credentials.Resolve(ctx, id, scopeType, scopeID)
	if err != nil {
		return MissingCredential("OpenWeather")
	}
	_, err = adapter.fetch(ctx, Location{Latitude: 43.6532, Longitude: -79.3832, Timezone: "America/Toronto"}, "metric", secret)
	return err
}

func (adapter *OpenWeatherAdapter) fetch(ctx context.Context, location Location, units, secret string) (WeatherSnapshot, error) {
	endpoint, _ := url.Parse(adapter.BaseURL)
	query := endpoint.Query()
	query.Set("lat", strconv.FormatFloat(location.Latitude, 'f', 6, 64))
	query.Set("lon", strconv.FormatFloat(location.Longitude, 'f', 6, 64))
	query.Set("units", units)
	query.Set("exclude", "minutely")
	query.Set("appid", secret)
	endpoint.RawQuery = query.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	resp, err := adapter.Client.Do(req)
	if err != nil {
		return WeatherSnapshot{}, TemporarilyUnavailable()
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return WeatherSnapshot{}, SanitizedError{Code: "provider_credential_invalid", Message: "The OpenWeather credential could not be validated", Retryable: false}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return WeatherSnapshot{}, SanitizedError{Code: "provider_rate_limited", Message: "Weather data is temporarily rate limited", Retryable: true}
	}
	if resp.StatusCode != http.StatusOK {
		return WeatherSnapshot{}, TemporarilyUnavailable()
	}
	var payload openWeatherResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || payload.Current.DT == 0 || len(payload.Current.Weather) == 0 {
		return WeatherSnapshot{}, SanitizedError{Code: "provider_response_invalid", Message: "Weather data could not be read", Retryable: true}
	}
	result := WeatherSnapshot{
		Location: location,
		Current: WeatherCondition{
			Timestamp: time.Unix(payload.Current.DT, 0).UTC(), Summary: payload.Current.Weather[0].Description,
			Temperature: payload.Current.Temp, FeelsLike: payload.Current.FeelsLike,
		},
		ProviderUpdated: time.Unix(payload.Current.DT, 0).UTC(),
	}
	if payload.Timezone != "" {
		result.Location.Timezone = payload.Timezone
	}
	if len(payload.Daily) > 0 {
		result.Current.DailyHigh = payload.Daily[0].Temp.Max
		result.Current.DailyLow = payload.Daily[0].Temp.Min
		result.Current.PrecipitationProbability = payload.Daily[0].Pop
	}
	for _, item := range payload.Hourly {
		if len(result.Hourly) == 12 {
			break
		}
		summary := ""
		if len(item.Weather) > 0 {
			summary = item.Weather[0].Description
		}
		result.Hourly = append(result.Hourly, HourlyForecast{Timestamp: time.Unix(item.DT, 0).UTC(), Temperature: item.Temp, Summary: summary, PrecipitationProbability: item.Pop})
	}
	for index, alert := range payload.Alerts {
		result.Alerts = append(result.Alerts, WeatherAlert{ID: fmt.Sprintf("openweather-%d-%d", alert.Start, index), Title: alert.Event, Severity: severityForAlert(alert.Event), StartsAt: time.Unix(alert.Start, 0).UTC(), ExpiresAt: time.Unix(alert.End, 0).UTC()})
	}
	return result, nil
}

type openWeatherResponse struct {
	Timezone string `json:"timezone"`
	Current  struct {
		DT        int64   `json:"dt"`
		Temp      float64 `json:"temp"`
		FeelsLike float64 `json:"feels_like"`
		Weather   []struct {
			Description string `json:"description"`
		} `json:"weather"`
	} `json:"current"`
	Hourly []struct {
		DT      int64   `json:"dt"`
		Temp    float64 `json:"temp"`
		Pop     float64 `json:"pop"`
		Weather []struct {
			Description string `json:"description"`
		} `json:"weather"`
	} `json:"hourly"`
	Daily []struct {
		Temp struct {
			Min float64 `json:"min"`
			Max float64 `json:"max"`
		} `json:"temp"`
		Pop float64 `json:"pop"`
	} `json:"daily"`
	Alerts []struct {
		Event string `json:"event"`
		Start int64  `json:"start"`
		End   int64  `json:"end"`
	} `json:"alerts"`
}

func severityForAlert(event string) string {
	lower := strings.ToLower(event)
	if strings.Contains(lower, "warning") || strings.Contains(lower, "emergency") {
		return "severe"
	}
	if strings.Contains(lower, "watch") {
		return "moderate"
	}
	return "advisory"
}
