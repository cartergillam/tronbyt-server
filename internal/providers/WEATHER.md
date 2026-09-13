# Weather v1

App ID `tronbyt-weather`, display name Tronbyt Weather. Open-Meteo is the only new
implemented forecast backend. The existing credential-backed `local-weather`
candidate remains compatible; it is not silently migrated or overwritten.

`ForecastProvider.Forecast(context, WeatherRequest)` returns `WeatherReport`.
This reuses existing Location, WeatherRequest, shared Cache and server-managed
render injection. Current, hourly and daily models use a normalized condition
vocabulary, nullable numeric values, UTC instants, local date strings,
precipitation in mm and probability fractions 0–1. Current wind is km/h. A nullable daytime flag selects sun/moon presentation,
without exposing backend-specific daylight fields.
No raw WMO codes, provider response fields, URLs or credentials enter Pixlet/iOS.
Reports are bounded to 12 hourly and four daily entries before rendering; the
view contains at most four near-term hours and three days. Provider bodies are
limited to 256 KiB. Invalid JSON/current temperature produces sanitized failure.
Implausible optional numeric values become missing; temperatures are bounded to
physical forecast ranges before caching/conversion. Render location labels are
bounded to 80 characters. Observations over an hour old are marked STALE even
when just fetched; freshness never erases source age.

Open-Meteo WMO mapping: 0 clear, 1 mostly_clear, 2 partly_cloudy, 3 cloudy,
45/48 fog, 51/53/55 drizzle, 56/57/66/67 freezing_rain, 61/63/80/81 rain,
65/82 heavy_rain, 71/73/77/85 snow, 75/86 heavy_snow, 95/96/99 thunderstorm;
missing/unrecognized codes become unknown.

The saved DeviceLocation supplies coordinates and device timezone (including
existing explicit device timezone preference). No separate app location copy or
geocoder is created. Invalid/missing coordinates/timezone produce SET LOCATION.
iOS links owners to the existing device-location city/current-location search;
members without location see an owner-configuration hint. Celsius is the default;
Fahrenheit and mph conversion happens on a copied display report, leaving cached
metric values untouched. Dates and near-term timing use device-local time, never
the server timezone. Daily local date labels are recalculated at render time.

Two independently coalesced caches share public data by provider, coordinates
(six decimal places) and timezone, excluding units, owner/device identity and
location labels. Current/hourly refresh after 15 minutes; daily after 60 minutes.
Both retain six additional hours of LKG. Failed refreshes/backoff serve LKG as
STALE; even after current LKG expires, a useful cached daily forecast can survive.
Retries are spaced at least five minutes per cache key. All state is in process;
restarts lose caches. Render/preview/device frequency and animation frames do
not spend additional provider requests inside freshness windows. Concurrent
same-location requests coalesce. At steady state one location uses about four
current/hourly requests and one daily request per hour. FORECAST requests only
daily data: approximately one request/hour/shared location, without requesting
current or hourly fields. Its daily cache is the same cache used by CURRENT/AUTO.
The normalized request carries resource intent (`DailyOnly`), not backend fields.

A copied ForDisplay report selects up to three days starting today. Precipitation
is meaningful at >=50% probability or >=0.2 mm within the next four hours.
Current measured precipitation >=0.1 mm with a recent timestamp becomes NOW.
Snow and freezing rain get SNOW/ICE wording. Future guidance is suppressed for
stale data. Timing is deliberately approximate (WITHIN 1 HR / IN ~2–4 HR), based
on UTC forecast instants, across local midnight/DST correctly.

AUTO rotates Current -> optional Precipitation -> Forecast at five seconds/page.
CURRENT and FORECAST remain single-purpose. Missing current/daily/hourly fields
are handled without inventing numeric zeros. Forecast-only LKG is useful if
current data is unavailable. Bundled deterministic transparent pixel icons have
no remote acquisition; the Apps generator reproduces PNGs and embedded constants.

To swap backends, implement ForecastProvider with the same normalized metric
report and inject it into Server.ForecastProvider. Apps/iOS require no provider
changes. Provider attribution/license metadata stays separate from display fields.

Open-Meteo free service has noncommercial usage and request limits; forecast data
also has attribution/license requirements. Before commercial distribution,
review current terms and arrange an appropriate service/license. Do not assume
free development access authorizes a commercial rollout:
https://open-meteo.com/en/terms and https://open-meteo.com/en/docs .

All automated provider tests use synthetic offline HTTP transports. Injection
regressions exercise forty renders/two devices/two unit settings with just one
current/hourly request plus one daily request. Weather render fixtures include
Canadian winter/summer extremes, rain/snow/ice/fog/storm, nullable fields, LKG,
three-day columns, and AUTO precipitation page selection.
