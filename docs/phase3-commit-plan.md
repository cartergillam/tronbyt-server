# Phase 3 commit plan

This plan describes staging boundaries only. Run `git diff --cached --check`
and the named verification before every commit. Do not use `git add -A` for
files marked **partial**.

## `tronbyt-apps`

### 1. `fix(mlb): select games by device-local date and team ID`

- Purpose: correct Toronto/UTC/month-boundary selection, status handling,
  bounded no-game retry, background compatibility, diagnostics, and fixtures.
- Files: `apps/mlb_game/mlb_game.star`, `docs/mlb-game.md`.
- Staging: whole files; no dependency.
- Verify: Pixlet v0.50.1 format; deterministic regression; background on/off;
  one sanitized live render.
- Risk/rollback: medium upstream-schema/timezone risk; revert both files
  together to restore the prior renderer.

### 2. `fix(clock): render on exact local minute boundaries`

- Purpose: deterministic timezone, 12/24-hour, DST, and refresh behavior.
- Files: `apps/ogclock/og_clock.star`, `docs/clock.md`.
- Staging: whole files; independent of commit 1.
- Verify: Pixlet v0.50.1 format/regression and the server-embedded Pixlet test.
- Risk/rollback: low; revert both files together. Never stage
  `apps/ogclock/.starcache/`.

## `tronbyt-server`

### 1. `feat(api): add capability-aware catalogue and safe previews`

- Purpose: capabilities, normalized schemas, paginated/searchable catalogue,
  bounded icon decoding/caching, installation preview, and lightweight
  catalogue authentication.
- Files: `internal/server/mobile_api.go`,
  `internal/server/mobile_api_test.go`, `internal/server/middleware.go`,
  `internal/server/metrics.go`; route-registration and schema-validation hunks
  from `internal/server/handlers_api.go` and
  `internal/server/handlers_api_test.go`.
- Staging: **partial** for both `handlers_api*` files; stage only catalogue,
  capabilities, preview, schema-pattern, unknown-key, and route hunks. Leave
  location, push, render-health, and recovery hunks unstaged.
- Dependencies: none; additive API contract.
- Verify: `go test ./internal/server -run 'Mobile|Catalogue|Capabilities|Preview|Schema' -count=1` and `go vet ./...`.
- Risk/rollback: medium response-shape/cache risk; route removal and file
  revert leave legacy APIs intact.

### 2. `feat(location): persist device defaults and invalidate dependent apps`

- Purpose: structured location validation, authorization, response fields,
  device inheritance, and prompt rerender after a change.
- Files: location-only hunks in `internal/data/models.go`,
  `internal/server/handlers_api.go`, and
  `internal/server/handlers_api_test.go`; location tests in
  `internal/server/reliability_test.go`.
- Staging: **partial in every shared file**. Include `DeviceLocation` additions,
  `HasLocation`, `DeviceUpdate.Location`, validation, payload timezone/location,
  inheritance/source helpers, invalidation, and their tests only.
- Dependencies: commit 1 only where mobile routes expose the fields.
- Verify: location/authorization/rerender tests plus `go test ./... -count=1`.
- Risk/rollback: medium; schema is additive, so reverting code does not require
  dropping columns. Preserve database backups.

### 3. `fix(push): separate one-shot frames from persistent installations`

- Purpose: prevent temporary Show Now frames from becoming installations and
  clean only stale temporary/orphan state.
- Files: push-only hunks in `internal/data/models.go`,
  `internal/server/handlers_api.go`,
  `internal/server/handlers_api_test.go`, `internal/server/rotation.go`,
  `internal/server/rotation_test.go`, `internal/server/server.go`, and
  `internal/server/reliability_test.go`; whole
  `internal/server/push_lifecycle.go`; compatibility adjustment in
  `internal/server/websockets_test.go`.
- Staging: **partial** for all shared files; include `PushKind`, `Persistent`,
  persistent-selection rules, lifecycle cleanup/startup hook, fixture migration,
  dry-run/idempotency, and push tests only.
- Dependencies: none; keep migration and cleanup in this same commit.
- Verify: push/cleanup/production-lineage tests, then full and race suites.
- Risk/rollback: high data-lifecycle risk; back up SQLite and WebPs together,
  run cleanup dry-run first, and roll both stores back as one unit.

### 4. `feat(diagnostics): expose sanitized render and poll health`

- Purpose: render outcomes/backoff, serialized physical polls, sanitized event
  timelines, diagnostic API, selection tracing, and additive response headers.
- Files: render-health hunks in `internal/data/models.go`,
  `internal/server/handlers_api.go`,
  `internal/server/handlers_api_test.go`, `internal/server/rotation.go`,
  `internal/server/rotation_test.go`, `internal/server/server.go`, and
  `internal/server/reliability_test.go`; whole
  `internal/server/render_utils.go`, `internal/server/handlers_device_api.go`,
  `internal/server/handlers_device_api_test.go`, and
  `internal/server/diagnostics.go`; diagnostics routes from the shared handler.
- Staging: **partial** for shared files. Include only health columns/payloads,
  render classification and next-eligible time, poll lock/trace/events,
  diagnostic route, and associated tests.
- Dependencies: commit 3 for persistent/temporary classifications; commit 1
  for capability advertisement.
- Verify: diagnostics/render/rotation/device-poll tests, full suite, race suite.
- Risk/rollback: medium; additive columns can remain after code rollback. Watch
  poll latency and sanitized logging before broad rollout.

### 5. `perf(api): guard physical polling under catalogue load`

- Purpose: preserve poll responsiveness with 1,000 summaries, six icon workers,
  searches, and diagnostics traffic.
- Files: `internal/server/mobile_api_benchmark_test.go`; catalogue metric and
  lightweight-auth hunks already included with commit 1.
- Staging: whole benchmark file after commits 1 and 4.
- Dependencies: commits 1 and 4.
- Verify: `go test ./internal/server -run PhysicalPolling -count=1` and
  `go test ./internal/server -bench CatalogueForUser1000 -benchmem -run '^$' -count=3`.
- Risk/rollback: low (test-only); revert the benchmark file.

### 6. `chore(rehearsal): add isolated deployment and rollback tooling`

- Purpose: reproducible seed, inspect, backup/restore, poll, load, and Compose
  rehearsal with development-only credentials.
- Files: `Dockerfile`, `cmd/rehearsal/main.go`,
  `deploy/rehearsal/compose.yaml`, `scripts/rehearsal.sh`,
  `docs/deployment-rehearsal.md`.
- Staging: whole files; depends on commits 1, 3, and 4.
- Verify: shell/YAML syntax, utility seed/inspect/backup/restore, and a Docker
  Compose start/health/poll/load/restore rehearsal when Docker is available.
- Risk/rollback: low production risk because the utility is a separate image
  target; revert all five files together.

### 7. `docs(release): record compatibility, operations, and rollback order`

- Purpose: compatibility matrix, architecture, release gates, key-scope warning,
  and this staging plan.
- Files: `docs/api-compatibility-matrix.md`,
  `docs/release-checklists.md`, `docs/reliability-architecture.md`,
  `docs/phase3-commit-plan.md`, `web/templates/manager/update.html`.
- Staging: whole documentation files; the template is a single help-text hunk.
- Dependencies: commits 1-6 so statements match shipped behavior.
- Verify: link/command review, secret scan, `git diff --cached --check`.
- Risk/rollback: low; reverting removes guidance only.

## `TronbytMobile 2`

### 1. `feat(api): add profiles, capabilities, and progressive fallbacks`

- Purpose: device/user key scope, optional capabilities, expanded models, and
  old-server protocol defaults.
- Files: `TronbytMobile/Core/API/APIClient.swift`,
  `TronbytMobile/Core/API/APIError.swift`,
  `TronbytMobile/Core/API/TronbytAPI.swift`,
  `TronbytMobile/Core/Models/Device.swift`,
  `TronbytMobile/Core/Models/Installation.swift`,
  `TronbytMobile/Core/Models/Requests.swift`,
  `TronbytMobile/Core/Models/ServerProfile.swift`, and capability/profile hunks
  in `TronbytMobile/App/SessionStore.swift`,
  `TronbytMobileTests/ModelDecodingTests.swift`, and
  `TronbytMobileTests/TestSupport.swift`.
- Staging: **partial** in the models, API, session, and test files: exclude
  location, catalogue UI, preview, diagnostics UI, and MLB-key migration hunks.
- Dependencies: server upgrades are optional because fallbacks are conservative.
- Verify: decoding, networking, profile persistence/key-scope, and legacy-server tests.
- Risk/rollback: medium profile-selection risk; Keychain data remains compatible.

### 2. `feat(app-library): add paginated schema-driven discovery`

- Purpose: pagination/search cancellation, six-icon limit, configuration fields,
  unknown-key preservation, optimistic rollback, and MLB key migration.
- Files: `TronbytMobile/Features/Apps/AppsView.swift`,
  `TronbytMobile/Features/Apps/AppsViewModel.swift`, app-library hunks in
  `TronbytMobile/Features/Apps/AppDetailView.swift` and
  `TronbytMobile/UI/Components.swift`, plus matching hunks in all three test
  files.
- Staging: **partial** for AppDetail, Components, and tests; exclude location
  editor and matrix-preview/diagnostic-only hunks.
- Dependencies: commit 1; feature stays hidden without server pagination capability.
- Verify: pagination, stale cancellation, six-request icon concurrency, schema
  preservation, MLB migration, and rollback tests.
- Risk/rollback: medium UI/API-load risk; capability gating restores installed-app-only behavior.

### 3. `feat(location): manage explicit device location`

- Purpose: search, one-shot foreground location, reverse geocoding, manual
  fallback, confirmation, privacy copy, clearing, persistence, and cancellation.
- Files: `Configuration/Info.plist`, `TronbytMobile.xcodeproj/project.pbxproj`,
  `TronbytMobile/Features/Settings/DeviceLocationView.swift`; location hunks in
  `TronbytMobile/Core/Models/Device.swift`,
  `TronbytMobile/Core/Models/Requests.swift`,
  `TronbytMobile/Core/API/TronbytAPI.swift`,
  `TronbytMobile/Features/Settings/SettingsView.swift`,
  `TronbytMobile/Features/Apps/AppDetailView.swift`,
  `TronbytMobile/UI/Components.swift`, and all three test files.
- Staging: **partial** in every shared file; include only location models,
  protocol calls, UI/editor, injection fakes, cancellation, validation, and tests.
- Dependencies: commit 1 and a server advertising `location-v1`.
- Verify: granted/denied, reverse-geocode failure, stale search, request
  cancellation, manual validation, update/clear, inheritance/override tests.
- Risk/rollback: medium privacy/data risk; removing the UI leaves the additive
  server location untouched and does not enable background tracking.

### 4. `feat(preview): add diagnostics and 64x36 matrix rendering`

- Purpose: capability-gated diagnostics/actions and deterministic static or
  animated matrix preview with Reduce Motion.
- Files: preview/diagnostics hunks in
  `TronbytMobile/Features/Apps/AppDetailView.swift`,
  `TronbytMobile/Features/Apps/AppsViewModel.swift`,
  `TronbytMobile/UI/Components.swift`, `TronbytMobile/App/SessionStore.swift`,
  and matching hunks in `ModelDecodingTests.swift`,
  `PersistenceAndBrightnessTests.swift`, and `TestSupport.swift`.
- Staging: **partial** in all files; do not absorb unrelated library/location hunks.
- Dependencies: commit 1; server diagnostics/preview capabilities are optional.
- Verify: diagnostics decoding/fallback, static/animated 64x36, Reduce Motion,
  recovery action, and mutation rollback tests.
- Risk/rollback: medium memory/performance risk; capability gating and Clean
  preview remain rollback paths.

### 5. `fix(settings): clarify Night Mode and key-scope onboarding`

- Purpose: align schedule controls with effective server state and make device
  versus user key selection explicit.
- Files: `TronbytMobile/Features/Onboarding/OnboardingView.swift`,
  `TronbytMobile/Features/Schedules/ScheduleView.swift`,
  `TronbytMobile/Features/Schedules/ScheduleViewModel.swift`; remaining
  schedule/key hunks in `SettingsView.swift`, `SessionStore.swift`,
  `ServerProfile.swift`, and tests.
- Staging: whole schedule/onboarding files; **partial** shared files/tests.
- Dependencies: commit 1.
- Verify: schedule save/reconciliation, profile migration, device/user key tests.
- Risk/rollback: low-to-medium UX risk; revert UI while retaining stored profiles.

### 6. `docs(ios): document architecture and hosted test execution`

- Purpose: reproducible simulator destination selection, test isolation, and
  architecture/privacy behavior.
- Files: `docs/mobile-architecture.md`, `docs/testing.md`.
- Staging: whole files; depends on commits 1-5.
- Verify: execute the documented `xcodebuild ... test` command and secret scan.
- Risk/rollback: low.

## `tronbyt-firmware`

### 1. `fix(wifi): keep saved-network recovery active with bounded retries`

- Purpose: recover after router-delayed startup/power loss without discarding
  credentials or reboot-looping; classify disconnects and bound HTTP retries.
- Files: all semantic recovery/backoff/hash/reset hunks in `main/wifi.c` and
  `main/main.c`; `docs/wifi-recovery.md` behavior sections.
- Staging: **partial** in `main/main.c` and the documentation; exclude pure
  redaction and formatting hunks.
- Dependencies: none.
- Verify: ESP-IDF 5.5.2 MatrixPortal-S3 configure/build, warning review,
  partition fit, then physical router-delay/power-cycle/401 tests before release.
- Risk/rollback: high until physical proof; keep the known-good USB image.

### 2. `security(firmware): redact credentials and authenticated endpoints`

- Purpose: retain useful diagnostics without emitting SSID, API key, or signed URLs.
- Files: semantic logging hunks in `main/gfx.c`, `main/main.c`,
  `main/nvs_settings.c`, `main/ota.c`, and `main/remote.c`; the logging/privacy
  section of `docs/wifi-recovery.md`.
- Staging: **partial** in every file except `remote.c`; exclude pointer-style
  formatting-only hunks.
- Dependencies: independent of commit 1.
- Verify: secret scan, firmware compile, serial-log review on the canary board.
- Risk/rollback: low functional risk; reverting may reintroduce credential logs.

### 3. `style(firmware): apply existing C pointer formatting`

- Purpose: isolate formatting already present in the working tree from behavior.
- Files: remaining formatting-only hunks in `main/gfx.c`,
  `main/nvs_settings.c`, and `main/ota.c`.
- Staging: **partial**, only after commits 1 and 2; confirm the staged diff has no
  changed string, condition, call, or control flow.
- Dependencies: commits 1 and 2.
- Verify: firmware compile and `git diff --cached --check`.
- Risk/rollback: low, but defer this commit if it obscures upstream rebases.

### 4. `ci(firmware): pin MatrixPortal-S3 build to ESP-IDF 5.5.2`

- Purpose: resolve managed components and publish configure logs, warnings,
  sizes, partitions, hashes, ELF/map/binaries, and merged image without flashing.
- Files: `.github/workflows/verify-matrixportal-s3.yml`; build sections of
  `docs/wifi-recovery.md` if not staged with commit 1.
- Staging: whole workflow; documentation may require **partial** staging.
- Dependencies: commits 1-3.
- Verify: YAML parse locally, then run the workflow and inspect every artifact.
- Risk/rollback: low runtime risk; workflow failure blocks firmware release.

