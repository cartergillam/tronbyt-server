# Release checklists

These are operator checklists. None of the push, deploy, flash, key rotation, or
production mutation steps are performed by the Phase 3 code pass.

## Apps release

- [ ] Confirm `origin` is the intended fork and `upstream` is `tronbyt/apps`.
- [ ] Fetch and review upstream without merging until the release diff is clean.
- [ ] Confirm branch `feature/mlb-clock-reliability`.
- [ ] Run Pixlet v0.50.1 format and deterministic Clock/MLB regressions.
- [ ] Render Clock through the server's embedded Pixlet version.
- [ ] Run one sanitized live MLB render; do not make it the only proof.
- [ ] Review exact staged diff, commit locally, then push the feature branch.
- [ ] Point the server rehearsal at the fork URL and refresh system apps.
- [ ] Confirm catalogue revision/icon invalidation and rerender Clock/MLB.
- [ ] Record the apps rollback commit and verify the server can refresh it.

## Server release

- [ ] Back up the production SQLite/database and WebP data volume together.
- [ ] Record image/config versions and current health/poll baselines.
- [ ] Run startup push cleanup in dry-run and review every count.
- [ ] Build an immutable server image and record tag, digest, and build log.
- [ ] Pull the candidate without stopping the current container.
- [ ] Stop writes, take the final paired backup, and run additive migration.
- [ ] Start the candidate and wait for `/health`.
- [ ] Run authenticated synthetic polling and verify app/bytes/hash/latency.
- [ ] Observe a real physical poll and device diagnostics.
- [ ] Verify Clock, MLB, location inheritance, catalogue, icons, and push cleanup.
- [ ] Roll back the server image and database/WebP backup together if needed.

## iOS release

- [ ] Run the complete hosted XCTest suite on an installed simulator runtime.
- [ ] Build Release with strict concurrency and review warnings.
- [ ] Confirm distribution signing, team, bundle ID, and entitlements.
- [ ] Archive and validate without changing production server data.
- [ ] Install on a physical device and migrate an existing Keychain profile.
- [ ] Test both device-key and user-key profiles.
- [ ] Exercise 1,000-summary App Library pagination/search/icon loading.
- [ ] Test location granted, denied, cancellation, update, and clear.
- [ ] Check static/animated matrix preview performance and Reduce Motion.
- [ ] Retain the prior signed build as the rollback artifact.

## Firmware release

- [ ] Run the pinned ESP-IDF 5.5.2 MatrixPortal-S3 workflow.
- [ ] Review configure/build logs, warnings, partition fit, sizes, and hashes.
- [ ] Back up the known-good firmware and NVS/configuration.
- [ ] Flash the candidate by USB before permitting OTA distribution.
- [ ] Monitor serial output and confirm no credential or authenticated URL logs.
- [ ] Test saved-Wi-Fi recovery, wrong-password classification, and HTTP 401.
- [ ] Start the router after the display and confirm automatic recovery.
- [ ] Repeat cold power cycles and verify watchdog stability/poll cadence.
- [ ] Permit OTA only after USB validation succeeds.
- [ ] Keep the prior binary/hash and documented USB rollback procedure available.

## Release order

1. Apps fork, so server rendering consumes the corrected deterministic app code.
2. Server, after paired data backup and cleanup dry-run.
3. iOS, after the server advertises the optional capabilities it exposes.
4. Firmware by USB to one canary board; OTA only after physical recovery tests.

The recommended first production change is the apps release followed by a
single-server canary with synthetic and physical polling observed. Location and
iOS rollout can follow without forcing a firmware upgrade. Firmware is last
because power-loss recovery still needs physical proof.
