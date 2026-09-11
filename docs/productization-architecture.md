# Productization architecture

## Verified apps

`internal/server/verified_apps.json` is the server-authoritative allowlist keyed
by canonical app ID. Parsing an upstream manifest, installing successfully, or
appearing popular never grants verified status. Each entry records the verified
apps checkout/version, date, compatibility notes, recommendation status, and
optional preferred configuration defaults.

The verified-manifest digest participates in catalogue revision identity.
Clients therefore invalidate cached metadata when verification changes even if
the apps checkout SHA stays constant. `category=verified` selects only explicit
allowlist entries; normal search ranks matching verified and recommended apps
first. Direct routes and installed filtering operate on the full catalogue and
do not remove verified metadata.

The production-verified set contains OG Clock, MLB Game, and A Quote A Day. CFL
Scores is currently a candidate while the numeric-team-ID fix and future-game
rendering await renewed physical-display approval. Market Watch and Local
Weather are also candidates. Candidate metadata is visible to clients but does
not place an app in the verified category or an unattended starter bundle. NWS
Daily Forecast remains intentionally excluded.

## Managed provider credentials

Set `PROVIDER_CREDENTIAL_MASTER_KEY` to base64 or hex encoding of exactly 32
random bytes. The value is supplied by deployment secret management and is
never persisted in SQLite. `ProviderCredential` stores AES-256-GCM ciphertext,
a unique nonce, authenticated scope/provider metadata, key version, validation
state, and last-used timestamps. Its encrypted fields have `json:"-"` and no
mobile or device endpoint resolves them.

Owners rotate a logical credential by writing a new secret to the same
credential ID. Rotation creates new ciphertext and increments `keyVersion`, so
installations continue referring to the logical ID. Resolution requires the
expected scope type and scope ID and occurs only inside a server-side provider
adapter. Supported scope vocabulary is `server_owner`, `household`, `user`, and
`device`. Ordinary mobile member sessions cannot call credential-management
routes.

Provider cache keys contain normalized request inputs—not secrets or device IDs.
This permits safe response sharing for identical market symbols or
location/unit requests. Adapters apply a minimum request interval, preserve a
bounded last-known-good response, mark stale results, and return sanitized error
codes. The concrete adapters are Twelve Data (`/quote`) and OpenWeather One Call
3.0. Twelve Data availability and latency depend on the active subscription, so
the app labels data as latest/delayed and never promises real-time delivery.
OpenWeather's One Call data is expected to refresh on roughly a ten-minute
cadence.

### Existing raw-key fields

No automatic migration is performed. Upstream Pixlet apps still contain legacy
secret configuration fields that pass raw credentials to the Pixlet runtime.
Examples in the current apps checkout include Rachio, Spot the Station, Oura,
CTA L Tracker, Meraki Usage, Enphase Summary, GitLab Issues, YNAB, Powerwall,
Tesla Solar, and several transit/home-automation apps. They remain compatible,
but are not converted to managed credentials until an app-specific migration
can preserve behavior and verify that its provider calls have moved into a
server adapter. Export, diagnostic, and logging redaction must continue treating
those schema fields as secrets.

## Household and member permissions

The existing `User` remains the server owner, preserving single-owner installs.
A `Household` points to that owner. `HouseholdMember` records a pending or active
member role, while `DeviceAssignment` is the complete device allowlist for that
member.

| Principal | Assigned device control | Provider credentials | Server admin |
| --- | --- | --- | --- |
| Owner user API key | All owned devices | Metadata/write/rotate | Existing owner UI |
| Member mobile session | Assigned devices only | None | None |
| Device API key | Frame polling and operational device reporting only | None | None |

Member sessions enter the existing `RequireDevice` path with only assigned
devices preloaded. Explicit mobile-control and owner guards reject device keys;
the catalogue/mobile middleware cannot promote a matching device key into an
owner principal.

## Pairing flow

1. An authenticated owner creates a member and assigns owned devices.
2. The owner requests an eight-character, ten-minute pairing code.
3. Only an HMAC of the normalized code is stored. `PAIRING_CODE_SECRET` is a
   deployment-provided secret of at least 32 characters.
4. The iOS client redeems the code once. The transaction conditionally marks it
   redeemed and creates a random, revocable mobile session.
5. Only the session token is returned. Codes and tokens contain no user API key,
   device key, provider secret, or authentication header.
6. Expired, replayed, revoked, or unassigned-device requests fail closed.

The verified starter bundle uses the same installation contract for each app
and returns an explicit per-app result with HTTP 207 for partial failure. This
keeps partial state visible rather than claiming a transaction succeeded. The
starter set contains only verified apps with safe unattended defaults; apps
requiring an explicit user choice remain available for manual installation.

## Market and weather provider contracts

`internal/providers` defines implementation-neutral contracts. Market requests
allow one to ten unique symbols and return symbol/display
name, latest available price, absolute and percentage change, market status,
logo reference, quote timestamp, provider update time, and stale state. A
provider adapter must describe its plan latency; the UI must not say “real time”
unless that plan guarantees it.

Weather requests contain an already-authorized effective location (device
default or explicit installation override), units, and a logical credential ID.
Responses include current/feels-like/high/low, precipitation probability,
hourly forecasts, alerts, provider update time, and stale state. Device location
and unit selection remain outside shared cache keys only when they actually
differ.

Pixlet/custom apps receive sanitized provider data, never credentials. Missing
credentials and temporary provider failures use stable error codes suitable for
iOS messages and display fallbacks.

## Deployment preparation

Generate and store these outside the database before enabling the corresponding
features:

```text
PROVIDER_CREDENTIAL_MASTER_KEY=<base64 32 random bytes>
PAIRING_CODE_SECRET=<at least 32 random characters>
```

Back up the database together with the exact master-key version. Losing the
master key makes encrypted credentials intentionally unrecoverable; rotating
the deployment master key requires a dedicated decrypt-and-reencrypt operation,
which is separate from routine provider-secret rotation.

Startup treats these product features as optional fail-closed capabilities:
an absent or invalid credential master key disables credential management and
provider adapters; an absent, short, common, or low-diversity pairing secret
disables household provisioning. Existing frame polling continues to operate.
Capabilities report availability as booleans and never reveal secret values or
validation details. Production must use randomly generated values; repeated-byte
or placeholder material is rejected.
