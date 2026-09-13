package providers

import (
	"context"
	"fmt"
	"math"
	"time"
)

// ForecastProvider is the provider-neutral Weather v1 boundary. WeatherRequest,
// Location and Cache are shared with the legacy credential-backed weather app.
type ForecastProvider interface {
	Forecast(context.Context, WeatherRequest) (WeatherReport, error)
}
type WeatherNow struct {
	Daytime       *bool     `json:"daytime,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
	Temperature   *float64  `json:"temperature"`
	FeelsLike     *float64  `json:"feelsLike,omitempty"`
	Condition     string    `json:"condition"`
	Precipitation *float64  `json:"precipitation,omitempty"` // mm
	WindSpeed     *float64  `json:"windSpeed,omitempty"`     // km/h in the SI cache
}
type WeatherHour struct {
	Timestamp     time.Time `json:"timestamp"`
	Temperature   *float64  `json:"temperature,omitempty"`
	Condition     string    `json:"condition"`
	Probability   *float64  `json:"probability,omitempty"` // fraction 0..1
	Precipitation *float64  `json:"precipitation,omitempty"`
}
type WeatherDay struct {
	Date        string   `json:"date"`
	Weekday     string   `json:"weekday,omitempty"`
	High        *float64 `json:"high"`
	Low         *float64 `json:"low"`
	Condition   string   `json:"condition"`
	Probability *float64 `json:"probability,omitempty"`
}
type WeatherSoon struct {
	Kind        string   `json:"kind"`
	Timing      string   `json:"timing"`
	Probability *float64 `json:"probability,omitempty"`
}
type WeatherReport struct {
	Location       Location      `json:"location"`
	Current        *WeatherNow   `json:"current"`
	Hourly         []WeatherHour `json:"hourly,omitempty"`
	Daily          []WeatherDay  `json:"daily"`
	Soon           *WeatherSoon  `json:"soon,omitempty"`
	Provider       string        `json:"provider"`
	FetchedAt      time.Time     `json:"fetchedAt"`
	DailyFetchedAt time.Time     `json:"dailyFetchedAt"`
	Stale          bool          `json:"stale"`
	Units          string        `json:"units"`
}

func validWeatherLocation(l Location) bool {
	_, err := time.LoadLocation(l.Timezone)
	return err == nil && l.Timezone != "" && l.Timezone != "Local" && !math.IsNaN(l.Latitude) && !math.IsInf(l.Latitude, 0) && !math.IsNaN(l.Longitude) && !math.IsInf(l.Longitude, 0) && l.Latitude >= -90 && l.Latitude <= 90 && l.Longitude >= -180 && l.Longitude <= 180
}
func convertedTemperature(v *float64, units string) *float64 {
	if v == nil {
		return nil
	}
	n := *v
	if units == "imperial" {
		n = n*9/5 + 32
	}
	return &n
}

// ForDisplay copies slices/pointers before converting, preserving shared metric
// cache values. All date boundaries and timing labels use the device timezone.
func (r WeatherReport) ForDisplay(now time.Time, units string) WeatherReport {
	loc, err := time.LoadLocation(r.Location.Timezone)
	if err != nil {
		loc = time.UTC
	}
	r.Units = units
	if label := []rune(r.Location.Label); len(label) > 80 {
		r.Location.Label = string(label[:80])
	}
	if r.Current != nil && r.Current.Timestamp.Before(now.Add(-time.Hour)) {
		r.Stale = true
	}
	if r.Current != nil {
		c := *r.Current
		c.Temperature = convertedTemperature(c.Temperature, units)
		c.FeelsLike = convertedTemperature(c.FeelsLike, units)
		if c.WindSpeed != nil {
			v := *c.WindSpeed
			if units == "imperial" {
				v /= 1.609344
			}
			c.WindSpeed = &v
		}
		r.Current = &c
	}
	days := []WeatherDay{}
	today := now.In(loc).Format("2006-01-02")
	for _, d := range r.Daily {
		if d.Date < today {
			continue
		}
		t, err := time.ParseInLocation("2006-01-02", d.Date, loc)
		if err != nil {
			continue
		}
		d.Weekday = t.Format("Mon")
		d.High = convertedTemperature(d.High, units)
		d.Low = convertedTemperature(d.Low, units)
		days = append(days, d)
		if len(days) == 3 {
			break
		}
	}
	r.Daily = days
	hours := []WeatherHour{}
	for _, h := range r.Hourly {
		if h.Timestamp.Before(now) || h.Timestamp.After(now.Add(4*time.Hour)) {
			continue
		}
		h.Temperature = convertedTemperature(h.Temperature, units)
		hours = append(hours, h)
		if len(hours) == 4 {
			break
		}
	}
	r.Hourly = hours
	r.Soon = nil
	if r.Current != nil && r.Current.Timestamp.After(now.Add(-time.Hour)) && r.Current.Precipitation != nil && *r.Current.Precipitation >= .1 {
		r.Soon = &WeatherSoon{Kind: precipKind(r.Current.Condition), Timing: "NOW"}
	} else if !r.Stale { // Do not promise approaching precipitation from old data.
		for _, h := range hours {
			if h.Timestamp.Before(now) {
				continue
			}
			if (h.Probability != nil && *h.Probability >= .5) || (h.Precipitation != nil && *h.Precipitation >= .2) {
				n := int(math.Ceil(h.Timestamp.Sub(now).Hours()))
				timing := "WITHIN 1 HR"
				if n > 1 {
					timing = fmt.Sprintf("IN ~%d HR", min(n, 4))
				}
				r.Soon = &WeatherSoon{Kind: precipKind(h.Condition), Timing: timing, Probability: h.Probability}
				break
			}
		}
	}
	return r
}
func precipKind(condition string) string {
	switch condition {
	case "snow", "heavy_snow":
		return "SNOW"
	case "freezing_rain":
		return "ICE"
	default:
		return "RAIN"
	}
}
