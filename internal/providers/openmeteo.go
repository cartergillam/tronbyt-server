package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const WeatherCurrentTTL = 15 * time.Minute
const WeatherDailyTTL = time.Hour
const WeatherLKGTTL = 6 * time.Hour

type OpenMeteoAdapter struct {
	Client       *http.Client
	BaseURL      string
	CurrentCache *Cache[WeatherReport]
	DailyCache   *Cache[WeatherReport]
}

func NewOpenMeteoAdapter(client *http.Client) *OpenMeteoAdapter {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &OpenMeteoAdapter{Client: client, BaseURL: "https://api.open-meteo.com/v1/forecast", CurrentCache: NewCache[WeatherReport](5 * time.Minute), DailyCache: NewCache[WeatherReport](5 * time.Minute)}
}
func (a *OpenMeteoAdapter) Forecast(ctx context.Context, req WeatherRequest) (WeatherReport, error) {
	if !validWeatherLocation(req.Location) {
		return WeatherReport{}, SanitizedError{Code: "weather_location_missing", Message: "Set the device location"}
	}
	// No user/device/credential/units/label enters public same-location SI keys.
	key := fmt.Sprintf("open-meteo:%.6f,%.6f:%s", req.Location.Latitude, req.Location.Longitude, req.Location.Timezone)
	if req.DailyOnly {
		report, stale, err := a.DailyCache.Get(ctx, key, WeatherDailyTTL, WeatherLKGTTL, func(ctx context.Context) (WeatherReport, error) { return a.fetch(ctx, req.Location, true) })
		report.Location = req.Location
		report.DailyFetchedAt = report.FetchedAt
		report.Stale = stale
		return report, err
	}
	current, stale, err := a.CurrentCache.Get(ctx, key, WeatherCurrentTTL, WeatherLKGTTL, func(ctx context.Context) (WeatherReport, error) { return a.fetch(ctx, req.Location, false) })
	if err != nil {
		// A daily forecast can remain useful after current-weather LKG expires.
		// This fallback is read-only and never performs another provider request.
		a.DailyCache.mu.Lock()
		entry, found := a.DailyCache.entries[key]
		a.DailyCache.mu.Unlock()
		if found && time.Now().Before(entry.staleUntil) {
			fallback := entry.value
			fallback.Location = req.Location
			fallback.DailyFetchedAt = fallback.FetchedAt
			fallback.Stale = true
			return fallback, nil
		}
		return WeatherReport{}, err
	}
	daily, dailyStale, dailyErr := a.DailyCache.Get(ctx, key, WeatherDailyTTL, WeatherLKGTTL, func(ctx context.Context) (WeatherReport, error) { return a.fetch(ctx, req.Location, true) })
	current.Location = req.Location
	current.Daily = daily.Daily
	current.DailyFetchedAt = daily.FetchedAt
	current.Stale = stale || dailyStale || dailyErr != nil
	return current, nil
}
func (a *OpenMeteoAdapter) fetch(ctx context.Context, l Location, daily bool) (WeatherReport, error) {
	endpoint, err := url.Parse(a.BaseURL)
	if err != nil {
		return WeatherReport{}, TemporarilyUnavailable()
	}
	q := endpoint.Query()
	q.Set("latitude", strconv.FormatFloat(l.Latitude, 'f', 6, 64))
	q.Set("longitude", strconv.FormatFloat(l.Longitude, 'f', 6, 64))
	q.Set("timezone", l.Timezone)
	q.Set("temperature_unit", "celsius")
	q.Set("wind_speed_unit", "kmh")
	q.Set("precipitation_unit", "mm")
	if daily {
		q.Set("daily", "weather_code,temperature_2m_max,temperature_2m_min,precipitation_probability_max")
		q.Set("forecast_days", "4")
	} else {
		q.Set("current", "temperature_2m,apparent_temperature,weather_code,precipitation,wind_speed_10m,is_day")
		q.Set("hourly", "temperature_2m,weather_code,precipitation_probability,precipitation")
		q.Set("forecast_hours", "12")
		q.Set("timeformat", "unixtime")
	}
	endpoint.RawQuery = q.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return WeatherReport{}, TemporarilyUnavailable()
	}
	response, err := a.Client.Do(request)
	if err != nil {
		return WeatherReport{}, TemporarilyUnavailable()
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return WeatherReport{}, TemporarilyUnavailable()
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 256*1024+1))
	if err != nil || len(body) > 256*1024 {
		return WeatherReport{}, weatherBadResponse()
	}
	var p meteoResponse
	if json.Unmarshal(body, &p) != nil {
		return WeatherReport{}, weatherBadResponse()
	}
	r := WeatherReport{Provider: "open-meteo", FetchedAt: time.Now().UTC()}
	if daily {
		if len(p.Daily.Time) == 0 || len(p.Daily.Time) > 4 {
			return WeatherReport{}, weatherBadResponse()
		}
		for i, date := range p.Daily.Time {
			if _, err := time.Parse("2006-01-02", date); err != nil {
				return WeatherReport{}, weatherBadResponse()
			}
			r.Daily = append(r.Daily, WeatherDay{Date: date, High: meteoTemperatureValue(meteoValue(p.Daily.High, i)), Low: meteoTemperatureValue(meteoValue(p.Daily.Low, i)), Condition: wmoCondition(meteoCode(p.Daily.Code, i)), Probability: meteoProbability(p.Daily.Probability, i)})
		}
	} else {
		if p.Current.Time <= 0 || meteoTemperatureValue(p.Current.Temperature) == nil || len(p.Hourly.Time) > 12 {
			return WeatherReport{}, weatherBadResponse()
		}
		r.Current = &WeatherNow{Daytime: meteoDaytime(p.Current.Daytime), Timestamp: time.Unix(p.Current.Time, 0).UTC(), Temperature: meteoTemperatureValue(p.Current.Temperature), FeelsLike: meteoRange(p.Current.FeelsLike, -150, 100), Condition: wmoCondition(p.Current.Code), Precipitation: meteoRange(p.Current.Precipitation, 0, 300), WindSpeed: meteoRange(p.Current.Wind, 0, 500)}
		for i, t := range p.Hourly.Time {
			r.Hourly = append(r.Hourly, WeatherHour{Timestamp: time.Unix(t, 0).UTC(), Temperature: meteoTemperatureValue(meteoValue(p.Hourly.Temperature, i)), Condition: wmoCondition(meteoCode(p.Hourly.Code, i)), Probability: meteoProbability(p.Hourly.Probability, i), Precipitation: meteoRange(meteoValue(p.Hourly.Precipitation, i), 0, 300)})
		}
	}
	return r, nil
}
func weatherBadResponse() error {
	return SanitizedError{Code: "provider_response_invalid", Message: "Weather is unavailable", Retryable: true}
}
func meteoValue(v []*float64, i int) *float64 {
	if i >= len(v) {
		return nil
	}
	return v[i]
}
func meteoCode(v []*int, i int) *int {
	if i >= len(v) {
		return nil
	}
	return v[i]
}
func meteoProbability(v []*float64, i int) *float64 {
	n := meteoValue(v, i)
	if n == nil || *n < 0 || *n > 100 {
		return nil
	}
	value := *n / 100
	return &value
}
func wmoCondition(code *int) string {
	if code == nil {
		return "unknown"
	}
	switch *code {
	case 0:
		return "clear"
	case 1:
		return "mostly_clear"
	case 2:
		return "partly_cloudy"
	case 3:
		return "cloudy"
	case 45, 48:
		return "fog"
	case 51, 53, 55:
		return "drizzle"
	case 56, 57, 66, 67:
		return "freezing_rain"
	case 61, 63, 80, 81:
		return "rain"
	case 65, 82:
		return "heavy_rain"
	case 71, 73, 77, 85:
		return "snow"
	case 75, 86:
		return "heavy_snow"
	case 95, 96, 99:
		return "thunderstorm"
	default:
		return "unknown"
	}
}

type meteoResponse struct {
	Current struct {
		Daytime       *int     `json:"is_day"`
		Time          int64    `json:"time"`
		Temperature   *float64 `json:"temperature_2m"`
		FeelsLike     *float64 `json:"apparent_temperature"`
		Code          *int     `json:"weather_code"`
		Precipitation *float64 `json:"precipitation"`
		Wind          *float64 `json:"wind_speed_10m"`
	} `json:"current"`
	Hourly struct {
		Time          []int64    `json:"time"`
		Temperature   []*float64 `json:"temperature_2m"`
		Code          []*int     `json:"weather_code"`
		Probability   []*float64 `json:"precipitation_probability"`
		Precipitation []*float64 `json:"precipitation"`
	} `json:"hourly"`
	Daily struct {
		Time        []string   `json:"time"`
		High        []*float64 `json:"temperature_2m_max"`
		Low         []*float64 `json:"temperature_2m_min"`
		Code        []*int     `json:"weather_code"`
		Probability []*float64 `json:"precipitation_probability_max"`
	} `json:"daily"`
}

func meteoRange(v *float64, low, high float64) *float64 {
	if v == nil || *v < low || *v > high {
		return nil
	}
	return v
}
func meteoTemperatureValue(v *float64) *float64 { return meteoRange(v, -100, 70) }

func meteoDaytime(v *int) *bool {
	if v == nil || (*v != 0 && *v != 1) {
		return nil
	}
	value := *v == 1
	return &value
}
