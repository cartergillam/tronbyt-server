# Display reliability architecture

## HTTP frame lifecycle

`GET /{device}/next` is serialized per device. Each poll reloads the device and ordered installations, authenticates the device-scoped key, selects a frame, updates `last_seen`, and records a structured diagnostic containing the request ID, protocol, selection reason, rotation indices, render classification, cache decision, sanitized WebP path, SHA-256 prefix, byte count, dwell, brightness, status, and duration. API keys and authenticated URLs are never logged.

Selection precedence is temporary one-shot content, display-off/default handling, Night Mode, pinning, interstitials, explicit persistent pushes, then enabled normal rotation. Invalid indices and references are repaired. Missing persistent WebP files remove their database row and repair pin/display/restore/interstitial references.

## Temporary push lifecycle

Show Now pushes are one-shot by default. They are file-backed with `__` names, coalesced by installation when possible, excluded from installation lists and normal rotation, and deleted after consumption. A caller must send `persistent: true` to create a persistent pushed installation and stable WebP.

At startup, legacy unclassified rows with an existing stable WebP are preserved and classified as persistent. Rows whose files are missing are removed, device references are repaired, order is compacted, and only then are orphan stable WebPs deleted. Database classification and reference repair share a transaction; file deletion occurs after commit so an interrupted run remains recoverable and idempotent. Explicit `push_kind=persistent` rows and pending `__` one-shot files survive.

Set `TRONBYT_PUSH_CLEANUP_DRY_RUN=true` for a startup inspection that logs classification counts without changing the database or filesystem. For production migration, stop the container and back up both the SQLite file and `/app/data/webp` tree as one recovery point. If cleanup is interrupted, restart normally; the phases are safe to rerun. If an operator must roll back, stop the server and restore the database and WebP tree from the same backup timestamp—never restore only one side.

## Render outcomes and caching

Installations record `visible`, `hidden`, `empty`, `failure`, or `upstream_failure`, a sanitized message, the next eligible render time, consecutive counts, and the last visible successful render. App markers are:

- `TRONBYT-NEXT-RENDER: <RFC3339>` for a known content boundary.
- `TRONBYT-HIDDEN-UNTIL: <RFC3339>` for intentional absence.
- `TRONBYT-RENDER-FAILURE: <sanitized reason>` for an upstream failure.

Visible content uses the configured interval. Hidden content retries within 30 minutes, empty output within 5 minutes, and failures within 2 minutes. Boundaries must be 5 seconds to 24 hours in the future. A stale no-game result is therefore never cached for hours. Future-dated `last_render` values are treated as invalid and rerendered.

## Device location

The existing device PATCH route accepts a structured location with description/locality, latitude, longitude, IANA timezone, optional region/country/provider/place ID, and enforces device authorization. Empty location clears it. A change invalidates only location-aware, non-pushed renders. Rendering supplies `$location`; an absent, empty, or `__device__` app location inherits it, while an explicit app location remains authoritative. Installation and diagnostics payloads report `custom`, `device`, `fallback`, or `missing` as the effective source.

## Catalogue load protection

Catalogue and icon reads use lightweight authentication that does not preload every installation. Icon responses provide revisioned URLs, private cache headers and validators, reject files over 4 MiB or 4.2 megapixels before serving, and expose request/result/duration metrics. Mobile pagination, bounded/coalesced icon loading, and server-side search/filtering prevent catalogue traffic from starving physical frame polling. `BenchmarkCatalogueForUser1000` is the warm-cache regression benchmark.

## Key scope

A physical display must use its device key. User keys are account-wide and should only be used by trusted management clients. Changing a device key invalidates the authenticated Image URL stored by HTTP firmware. After changing it, save the device, copy the complete replacement Image URL from the device page through a secure channel, and update the board. Do not place either key or the authenticated URL in logs or diagnostics.

## Deployment and rollback

Deploy the apps repository first, then the server, then iOS. Firmware should be built and USB-tested before flashing a physical board. The server uses its normal automatic schema migration for additive columns and performs idempotent startup cleanup.

Before deployment, back up the database and device WebP directory. To roll back application code, restore the prior binaries/containers and app checkout. Additive columns may remain. If persistent pushed content is required after rollback, restore the database and WebP backup together; do not recreate removed legacy temporary rows independently.
