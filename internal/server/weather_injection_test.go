package server

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"tronbyt-server/internal/data"
	"tronbyt-server/internal/providers"
)

type weatherOfflineTransport struct{ calls atomic.Int32 }

func (t *weatherOfflineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	body := `{"current":{"time":1789239600,"temperature_2m":25,"weather_code":0}}`
	if r.URL.Query().Get("daily") != "" {
		body = `{"daily":{"time":["2026-09-12"],"temperature_2m_max":[25],"temperature_2m_min":[14]}}`
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
}
func TestWeatherInjectionSharedAcrossRendersDevicesAndUnits(t *testing.T) {
	transport := &weatherOfflineTransport{}
	s := &Server{ForecastProvider: providers.NewOpenMeteoAdapter(&http.Client{Transport: transport})}
	for i := 0; i < 40; i++ {
		d := &data.Device{ID: "a", Location: data.DeviceLocation{Lat: 43.3, Lng: -79.8, Timezone: "America/Toronto"}}
		if i%2 == 0 {
			d.ID = "b"
		}
		config := map[string]any{"units": "imperial"}
		s.injectManagedProviderData(context.Background(), d, &data.App{Name: "tronbyt-weather"}, config)
		require.Contains(t, config, "$provider_data")
		var r providers.WeatherReport
		require.NoError(t, json.Unmarshal([]byte(config["$provider_data"].(string)), &r))
		require.Equal(t, 77.0, *r.Current.Temperature)
		require.NotContains(t, config, "credential_id")
	}
	require.Equal(t, int32(2), transport.calls.Load())
	config := map[string]any{}
	s.injectManagedProviderData(t.Context(), &data.Device{}, &data.App{Name: "tronbyt-weather"}, config)
	require.Contains(t, config["$provider_error"], "weather_location_missing")
	require.Equal(t, int32(2), transport.calls.Load())
}

func TestWeatherInjectionModeCacheCompatibility(t *testing.T) {
	transport := &weatherOfflineTransport{}
	s := &Server{ForecastProvider: providers.NewOpenMeteoAdapter(&http.Client{Transport: transport})}
	d := &data.Device{Location: data.DeviceLocation{Lat: 43.3, Lng: -79.8, Timezone: "America/Toronto"}}
	for i := 0; i < 20; i++ {
		config := map[string]any{"mode": "forecast"}
		s.injectManagedProviderData(t.Context(), d, &data.App{Name: "tronbyt-weather"}, config)
		require.Contains(t, config, "$provider_data")
	}
	require.Equal(t, int32(1), transport.calls.Load())
	for _, mode := range []string{"forecast", "current", "auto", "forecast", "current"} {
		config := map[string]any{"mode": mode}
		s.injectManagedProviderData(t.Context(), d, &data.App{Name: "tronbyt-weather"}, config)
		require.Contains(t, config, "$provider_data")
	}
	require.Equal(t, int32(2), transport.calls.Load())
}
