package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func weatherFixtureAdapter(t *testing.T) (*OpenMeteoAdapter, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	a := NewOpenMeteoAdapter(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		require.Empty(t, r.Header.Get("Authorization"))
		require.Equal(t, "celsius", r.URL.Query().Get("temperature_unit"))
		body := `{"current":{"time":1789239600,"temperature_2m":25,"apparent_temperature":27,"weather_code":2,"precipitation":0,"wind_speed_10m":16},"hourly":{"time":[1789243200,1789246800],"temperature_2m":[24,null],"weather_code":[61,71],"precipitation_probability":[70,null],"precipitation":[0.4,null]}}`
		if r.URL.Query().Get("daily") != "" {
			body = `{"daily":{"time":["2026-09-12","2026-09-13","2026-09-14","2026-09-15"],"temperature_2m_max":[25,24,22,null],"temperature_2m_min":[14,13,12,null],"weather_code":[0,61,71,null],"precipitation_probability_max":[0,70,60,null]}}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})})
	return a, calls
}
func TestWeatherSharedMetricCacheAndTTL(t *testing.T) {
	a, calls := weatherFixtureAdapter(t)
	req := WeatherRequest{Location: Location{Latitude: 43.3, Longitude: -79.8, Timezone: "America/Toronto"}, Units: "metric"}
	for i := 0; i < 40; i++ {
		req.ScopeID = fmt.Sprint(i)
		req.Units = "imperial"
		r, err := a.Forecast(t.Context(), req)
		require.NoError(t, err)
		require.Len(t, r.Daily, 4)
		require.Equal(t, 25.0, *r.Current.Temperature)
	}
	require.Equal(t, int32(2), calls.Load()) // One combined current/hourly + one daily request.
	require.Equal(t, 15*time.Minute, WeatherCurrentTTL)
	require.Equal(t, time.Hour, WeatherDailyTTL)
	req.Location.Latitude = 44
	_, err := a.Forecast(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, int32(4), calls.Load())
	// Expiring current data refreshes only current/hourly, not the hourly daily cache.
	a.CurrentCache.mu.Lock()
	for k, e := range a.CurrentCache.entries {
		e.freshUntil = time.Now().Add(-time.Second)
		a.CurrentCache.entries[k] = e
		delete(a.CurrentCache.lastRun, k)
	}
	a.CurrentCache.mu.Unlock()
	_, err = a.Forecast(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, int32(5), calls.Load())
}
func TestWeatherConditions(t *testing.T) {
	for code, want := range map[int]string{0: "clear", 1: "mostly_clear", 2: "partly_cloudy", 3: "cloudy", 45: "fog", 51: "drizzle", 61: "rain", 65: "heavy_rain", 66: "freezing_rain", 71: "snow", 75: "heavy_snow", 95: "thunderstorm", 999: "unknown"} {
		require.Equal(t, want, wmoCondition(&code))
	}
	require.Equal(t, "unknown", wmoCondition(nil))
}
func TestWeatherDisplayPrecipUnitsAndMidnight(t *testing.T) {
	a, _ := weatherFixtureAdapter(t)
	r, err := a.Forecast(t.Context(), WeatherRequest{Location: Location{Timezone: "America/Toronto"}})
	require.NoError(t, err)
	now := time.Unix(1789239600, 0)
	v := r.ForDisplay(now, "imperial")
	require.Equal(t, 77.0, *v.Current.Temperature)
	require.Equal(t, 25.0, *r.Current.Temperature)
	require.NotNil(t, v.Soon)
	require.Equal(t, "RAIN", v.Soon.Kind)
	require.Len(t, v.Daily, 3)
	r.Hourly = r.Hourly[1:]
	p := .7
	r.Hourly[0].Probability = &p
	v = r.ForDisplay(now, "metric")
	require.Equal(t, "SNOW", v.Soon.Kind)
	p = .1
	v = r.ForDisplay(now, "metric")
	require.Nil(t, v.Soon)
	amount := .2
	r.Current.Precipitation = &amount
	r.Current.Condition = "freezing_rain"
	v = r.ForDisplay(now, "metric")
	require.Equal(t, "NOW", v.Soon.Timing)
	require.Equal(t, "ICE", v.Soon.Kind)
	r.Current.Precipitation = nil
	r.Stale = true
	require.Nil(t, r.ForDisplay(now, "metric").Soon)
	loc, err := time.LoadLocation("America/Toronto")
	require.NoError(t, err)
	before := time.Date(2026, 9, 12, 23, 59, 0, 0, loc)
	after := before.Add(2 * time.Minute)
	require.Equal(t, "Sat", r.ForDisplay(before, "metric").Daily[0].Weekday)
	require.Equal(t, "Sun", r.ForDisplay(after, "metric").Daily[0].Weekday)
}
func TestWeatherFailureLKGAndRetryCoalescing(t *testing.T) {
	a, calls := weatherFixtureAdapter(t)
	req := WeatherRequest{Location: Location{Timezone: "America/Toronto"}}
	_, err := a.Forecast(t.Context(), req)
	require.NoError(t, err)
	a.CurrentCache.mu.Lock()
	for k, e := range a.CurrentCache.entries {
		e.freshUntil = time.Now().Add(-time.Second)
		a.CurrentCache.entries[k] = e
		delete(a.CurrentCache.lastRun, k)
	}
	a.CurrentCache.mu.Unlock()
	a.Client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, fmt.Errorf("offline") })
	for i := 0; i < 20; i++ {
		v, err := a.Forecast(t.Context(), req)
		require.NoError(t, err)
		require.True(t, v.Stale)
		require.NotNil(t, v.Current)
	}
	require.Equal(t, int32(3), calls.Load())
}
func TestWeatherMalformedNullBoundedAndMissingLocation(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"current":{"time":1,"temperature_2m":null}}`, `{"current":{"time":1,"temperature_2m":1e308}}`, `bad`, strings.Repeat("x", 256*1024+1)} {
		a := NewOpenMeteoAdapter(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})})
		_, err := a.Forecast(context.Background(), WeatherRequest{Location: Location{Timezone: "America/Toronto"}})
		require.Error(t, err)
	}
	a, calls := weatherFixtureAdapter(t)
	_, err := a.Forecast(t.Context(), WeatherRequest{})
	require.Error(t, err)
	require.Zero(t, calls.Load())
}

func TestWeatherDailyFallbackAfterCurrentLKGExpires(t *testing.T) {
	a, calls := weatherFixtureAdapter(t)
	req := WeatherRequest{Location: Location{Timezone: "America/Toronto"}}
	_, err := a.Forecast(t.Context(), req)
	require.NoError(t, err)
	a.CurrentCache.mu.Lock()
	for k, e := range a.CurrentCache.entries {
		e.freshUntil = time.Now().Add(-time.Hour)
		e.staleUntil = time.Now().Add(-time.Second)
		a.CurrentCache.entries[k] = e
		delete(a.CurrentCache.lastRun, k)
	}
	a.CurrentCache.mu.Unlock()
	a.Client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, fmt.Errorf("offline") })
	report, err := a.Forecast(t.Context(), req)
	require.NoError(t, err)
	require.True(t, report.Stale)
	require.Nil(t, report.Current)
	require.Len(t, report.Daily, 4)
	require.Equal(t, int32(3), calls.Load())
}
func TestWeatherConcurrentLocationRequestsCoalesce(t *testing.T) {
	a, calls := weatherFixtureAdapter(t)
	req := WeatherRequest{Location: Location{Timezone: "America/Toronto"}}
	results := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func() { _, err := a.Forecast(context.Background(), req); results <- err }()
	}
	for i := 0; i < 20; i++ {
		require.NoError(t, <-results)
	}
	require.Equal(t, int32(2), calls.Load())
}
func TestWeatherTimezoneDSTAndNearTermBoundary(t *testing.T) {
	loc, err := time.LoadLocation("America/Toronto")
	require.NoError(t, err)
	now := time.Date(2026, 11, 1, 0, 30, 0, 0, loc)
	prob := .8
	temp := 0.0
	r := WeatherReport{Location: Location{Timezone: loc.String()}, Current: &WeatherNow{Timestamp: now, Temperature: &temp}, Daily: []WeatherDay{{Date: "2026-11-01"}, {Date: "2026-11-02"}, {Date: "2026-11-03"}}, Hourly: []WeatherHour{{Timestamp: now.Add(3 * time.Hour), Condition: "snow", Probability: &prob}}}
	view := r.ForDisplay(now, "metric")
	require.Equal(t, "IN ~3 HR", view.Soon.Timing)
	require.Equal(t, "Sun", view.Daily[0].Weekday)
	r.Hourly[0].Timestamp = now.Add(5 * time.Hour)
	require.Nil(t, r.ForDisplay(now, "metric").Soon)
}

func TestWeatherSourceAgeMarksStaleWithoutMutatingCache(t *testing.T) {
	a, _ := weatherFixtureAdapter(t)
	r, err := a.Forecast(t.Context(), WeatherRequest{Location: Location{Timezone: "America/Toronto"}})
	require.NoError(t, err)
	require.False(t, r.Stale)
	view := r.ForDisplay(r.Current.Timestamp.Add(2*time.Hour), "imperial")
	require.True(t, view.Stale)
	require.False(t, r.Stale)
	require.Nil(t, view.Soon)
}

func TestWeatherDaytimeNormalized(t *testing.T) {
	day, night, unknown := 1, 0, 2
	require.True(t, *meteoDaytime(&day))
	require.False(t, *meteoDaytime(&night))
	require.Nil(t, meteoDaytime(nil))
	require.Nil(t, meteoDaytime(&unknown))
}

func TestWeatherDaytimeWireContractIsNeutral(t *testing.T) {
	for _, raw := range []string{"0", "1", "null"} {
		a := NewOpenMeteoAdapter(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			require.Contains(t, req.URL.Query().Get("current"), "is_day")
			body := fmt.Sprintf(`{"current":{"time":1789239600,"temperature_2m":25,"weather_code":0,"is_day":%s}}`, raw)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})})
		report, err := a.fetch(t.Context(), Location{Timezone: "America/Toronto"}, false)
		require.NoError(t, err)
		if raw == "null" {
			require.Nil(t, report.Current.Daytime)
		} else {
			require.NotNil(t, report.Current.Daytime)
			require.Equal(t, raw == "1", *report.Current.Daytime)
		}
		body, err := json.Marshal(report)
		require.NoError(t, err)
		require.NotContains(t, string(body), "is_day")
		require.NotContains(t, string(body), "weather_code")
	}
}

func TestWeatherForecastOnlySkipsCurrentAndSharesDailyCache(t *testing.T) {
	a, calls := weatherFixtureAdapter(t)
	req := WeatherRequest{Location: Location{Latitude: 43.3, Longitude: -79.8, Timezone: "America/Toronto"}, DailyOnly: true}
	for i := 0; i < 20; i++ {
		r, err := a.Forecast(t.Context(), req)
		require.NoError(t, err)
		require.Nil(t, r.Current)
		require.Empty(t, r.Hourly)
		require.Len(t, r.Daily, 4)
	}
	require.Equal(t, int32(1), calls.Load())
	a.DailyCache.mu.Lock()
	for _, e := range a.DailyCache.entries {
		require.True(t, time.Until(e.freshUntil) > 59*time.Minute)
	}
	a.DailyCache.mu.Unlock()
	req.DailyOnly = false
	_, err := a.Forecast(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, int32(2), calls.Load()) // Current/hourly added; daily reused.
	req.DailyOnly = true
	_, err = a.Forecast(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, int32(2), calls.Load())
}

func TestWeatherForecastOnlyRequestShape(t *testing.T) {
	a := NewOpenMeteoAdapter(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		q := req.URL.Query()
		require.NotEmpty(t, q.Get("daily"))
		require.Empty(t, q.Get("current"))
		require.Empty(t, q.Get("hourly"))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"daily":{"time":["2026-09-13"]}}`))}, nil
	})})
	_, err := a.Forecast(t.Context(), WeatherRequest{Location: Location{Timezone: "America/Toronto"}, DailyOnly: true})
	require.NoError(t, err)
}

func TestWeatherRejectsNonFiniteProviderTemperature(t *testing.T) {
	for _, raw := range []string{"NaN", "Infinity", "-Infinity", "1e308", "-1e308"} {
		t.Run(raw, func(t *testing.T) {
			a := NewOpenMeteoAdapter(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body := fmt.Sprintf(`{"current":{"time":1789239600,"temperature_2m":%s}}`, raw)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			})})
			_, err := a.fetch(t.Context(), Location{Timezone: "America/Toronto"}, false)
			require.Error(t, err)
		})
	}
}
