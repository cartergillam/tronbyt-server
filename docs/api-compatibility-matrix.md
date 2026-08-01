# Server, iOS, and firmware compatibility

Phase 3 keeps the existing device and installation APIs intact and adds new
fields, routes, and response headers. Baseline display control does not require
a simultaneous upgrade. New feature screens are capability-gated when they
require a current server.

| Combination | Expected behavior | Fallback and verification |
| --- | --- | --- |
| Older iOS + current server | Existing device, installation, brightness, schedule, and push routes retain their prior shapes. Additional JSON fields are additive. | Server API and handler suites cover legacy and current payload aliases. |
| Current iOS + older server | Connection testing and core device/installation management continue. A missing `/v0/capabilities` response is treated as an older server. | Capability detection is optional. App Library, location, diagnostics actions, and installation preview are hidden or unavailable until the server advertises them. XCTest covers protocol defaults and minimal payload decoding. |
| Device-key profile | Exactly one authorized device is accepted and catalogue reads remain scoped to that device. | Server authorization-boundary tests and iOS profile-scope tests cover the one-device rule. |
| User-key profile | Every device owned by the user can be selected. | Existing user API behavior is preserved; iOS selected-device and display-restore state remain profile-scoped. |
| HTTP firmware | `/{device}/next` remains authenticated as configured and returns WebP. | Brightness/dwell behavior is unchanged. `Tronbyt-App` and `Tronbyt-Installation` are additive diagnostic headers; handler tests cover authenticated selection. |
| WebSocket firmware | Existing WebSocket route and authentication remain supported. | No protocol change is required by this pass. Server WebSocket tests remain in the full/race suites; physical recovery still requires board validation. |
| Server without location | Device payloads omit `location` and `timezone`. | iOS models decode both as optional, hide location management without `location-v1`, and reject an update the server does not confirm. Existing app-specific location remains untouched. |
| Server without diagnostics | No device diagnostics or recovery actions are requested. | The UI requires advertised `device-diagnostics`/`diagnostic-actions`; installed-app loading treats diagnostics as optional. |
| Server without paginated catalogue | Installed-app management remains available, but the new App Library is not shown. | The App Library requires advertised `catalogue-pagination`. Protocol defaults keep alternate/test clients source-compatible; a new-server upgrade is required only for the new catalogue feature. |
| Unknown installation schema keys | Unknown values survive a known-field edit and save. | iOS initializes editing from the complete server dictionary and XCTest asserts round-trip preservation. Server validation still rejects keys explicitly outside a current schema when that endpoint enforces one. |
| British MLB background key | Existing `show_team_coloured_logo_background` installations render as before. | MLB reads the old key only when the canonical US-spelling key is absent. iOS migrates the old value to `show_team_colored_logo_background` on edit; deterministic Starlark and XCTest coverage protect both paths. |

Capability checks are conservative: an absent capability document means an
optional feature is unavailable, not implicitly enabled. Errors remain visible
and sanitized; the client does not fabricate a successful location or preview
when an older server ignores or lacks the operation.
