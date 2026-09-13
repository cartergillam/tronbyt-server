# Sports Overview v1

`nhl-overview`, `nba-overview`, `nfl-overview` complement the Live apps. They use
`SportsRegistry.Overview` and the same NHLAdapter/ESPNAdapter instances, HTTP
clients, canonical Team and Game contracts and SportsLogoCache as Live. Live
methods and their policies are unchanged. Overview-specific parsers live beside
the existing adapters; no second provider system or background service is added.

## Contract

SportsOverview contains Team, Season (label, phase, nullable record), nullable
Standing, NextGame, LastGame, fetchedAt and stale. OverviewGame embeds Game and
adds favorite-team W/L/T, final label and bounded local date/time presentation.
NHL records are W-L-OTL; NBA W-L; NFL W-L with the tie count shown when nonzero.
Standings logo metadata enriches otherwise static team identities. ESPN schedule
logo arrays are translated into the existing game normalizer's server-only logo
field. If standings metadata is absent, the favorite identity can reuse its
schedule team's logo URL. Overview and Live pass through the same SportsLogoCache
instance; no second acquisition or normalization pipeline exists.

Ranks remain optional and are never computed from array position, wins or a
homegrown playoff formula. ProviderLogoURL remains server-only.

NHL uses `/standings/now` (one league-wide table) and
`/club-schedule-season/{abbreviation}/now`. Stable numeric team IDs come from the
existing catalog. Division/conference sequence fields, wildcard sequence 1/2
and explicit clinch indicators are normalized. The page prioritizes division
rank (ATL/MET/CEN/PAC); other context is retained without a fourth page.

ESPN uses `/apis/v2/sports/{sport}/{league}/standings` and the existing site-v2
base with `/teams/{id}/schedule`. Schedule competition status and object-shaped
scores are translated into the existing NBA/NFL game normalizers. NBA/NFL expose
provider playoffSeed as **SEED**, not a guarantee of qualification or an inferred
conference rank. ESPN's NBA description can call this a projection. Explicit
conferenceRank is supported when present; absent ranks are omitted. NFL division
rank comes only from the provider's explicit `Nth in AFC/NFC East/West/North/South`
summary, and only with a nonzero season record. No standings HTML is scraped.

## Seasons and games

ESPN season type and advertised season date bounds identify preseason, regular,
playoffs and offseason. NHL uses dated standings and nearby explicitly typed
schedule games (past seven / next fourteen days); standings older than 21 days
without nearby phase evidence become offseason. Unknown remains unknown; no
calendar-based qualification logic is invented. Offseason can show a clearly
labelled previous record; preseason suppresses regular records/ranks. New-season
schedule labels cannot inherit old ranks. Zero-game records never create ranks.

The earliest scheduled/pregame game is selected; recent delayed/postponed games
retain an honest status. Active games do not turn Overview into Live. The latest
completed result is selected regardless of array order. Missing/malformed final
scores are rejected rather than becoming zero. NHL OT/SO and NBA multiple OT
reuse normalized flags/periods; NFL ties show T. Date and clock labels are created
with the device timezone, including DST, after cache lookup; unconfirmed ESPN
start times show TBD. Pixlet parses no timestamps.

If a newly published schedule has no final, NHL checks the previous season.
ESPN checks up to two preceding season types (including the preceding year when
needed). These bounded lookbacks are part of the schedule cache. They may still
omit old finals if the provider omits those events; no result is fabricated.

## Efficiency and degradation

Standings: **30 minutes**, shared per provider/league for all favorite teams.
Schedules/results: **10 minutes**, per provider/league/team, independent of
device, installation, timezone, background style and page. No Live-frequency
polling. Known offseason: **6 hours** for each resource. Both retain **24 hours
additional LKG**, with five-minute failed-fetch spacing and concurrent-request
coalescing through existing Cache. Caches are in process and reset on restart.

Normal steady state is about two standings requests/hour/league plus six schedule
refreshes/hour/selected team. A schedule refresh is normally one HTTP request;
bounded missing-final lookbacks can add requests. All same-team installations
reuse it. No provider calls occur from renders inside TTL. Logo caching remains
seven days fresh plus thirty days LKG, shared with Live. Logo failures use bounded
team abbreviations and color. No remote requests occur in Pixlet.

Standings/schedule failures degrade independently. LKG is marked STALE. When
both have never loaded, the known team identity and STANDINGS N/A remain useful;
invalid teams request selection. Missing optional pages are skipped. Response
bodies are capped at 2 MiB, group depth and event counts bounded, numeric values
validated. Render payload includes only one next and one last game.

## UI and verification

Three pages maximum: identity/record/standing, next matchup, last score/result.
Five seconds per useful page; one page is static. Off/Dim/Full changes only logo
panel backgrounds. One shared Apps template generates standalone portable apps.
iOS reuses the existing searchable selector and form lifecycle; Favorite Team
uses friendly names and saves existing numeric canonical provider IDs.

`tools/sports_overview/test_render.py` in Apps creates offline WebPs, schemas and
labelled nearest-neighbor `/tmp/{nhl,nba,nfl}-overview-review.png` montages. Server
schema fixtures mirror the generated Pixlet schema and validate every team ID/name
against the existing catalogs. No automated test uses live provider requests.
Physical validation is pending: see Apps `docs/BEN_FRAME_VALIDATION.md`, which
also retains the unvalidated Weather and Market Watch checklist.
