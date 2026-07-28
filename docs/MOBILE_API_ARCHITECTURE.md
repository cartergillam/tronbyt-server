# Mobile API architecture

## Existing implementation audit

The server has two authentication adapters. Manager routes use `RequireLogin`, which
loads a user from a secure session cookie. `/v0` routes use `APIAuthMiddleware`,
which accepts a Bearer value matching either `User.APIKey` or `Device.APIKey`.
`RequireDevice` then resolves the requested `{id}` from that authenticated scope.
A device key places only that device in the owner's in-memory `Devices` collection;
a user key retains access to all of the user's devices. An inaccessible device is
never loaded by a device-specific handler.

Manager app operations were concentrated in `handlers_app.go`:

* `handleAddAppPost` parsed an HTML form, generated an installation name, assigned
  order, created the database row, rendered, notified, and redirected.
* `handleConfigAppPost` parsed the Manager JSON form, directly mutated the GORM
  model, saved every field, forced a render, notified, and redirected. Manager
  behavior intentionally remains unchanged.
* `handleReorderApps` converted a drag/drop operation to a complete ordered slice,
  then updated all changed `order` columns in one database transaction.
* `handleCurrentApp` called `GetCurrentAppImage`, the reusable preview source.
* `handleAppSchemaGet` called `renderer.GetSchema`, including Manager-only support
  for bypassing cached dynamic dropdown data.

Catalogue discovery was already reusable. `Server.ListSystemApps` returns a locked
copy of the system-app cache. `apps.ListUserApps` scans both user uploads and the
user's custom repository. `apps.AppMetadata` combines manifest metadata, inferred
defaults, source path, update date, and preview files. Catalogue IDs are existing
manifest IDs; system apps win if a custom app has the same ID. This collision rule
is an upstream data-model limitation.

Pixlet's schema JSON is `{version, schema: [...]}`. Fields use Pixlet types such as
`onoff`, `dropdown`, `radio`, `text`, `color`, `datetime`, `location`,
`locationbased`, `typeahead`, `generated`, OAuth, and PNG. The current Pixlet
schema has no native numeric constraints or required marker, but the normalizer
passes these through when a compatible custom schema supplies them.

Installed configuration is stored as JSON in `App.Config`. Pixlet `onoff` values
are stored as `"true"`/`"false"` strings by the Manager. Secret text fields were
not separately encrypted or tagged in storage. The mobile service therefore uses
the schema as the sensitivity authority, never serializes their values, and
reports only a `savedSecrets[key]` Boolean.

Rendered installation WebPs are stored below `webp/{deviceID}`. Normal apps use
`{appName}-{iname}.webp`; persistent pushed apps use
`webp/{deviceID}/pushed/{installationID}.webp`. `GetCurrentAppImage` first uses a
WebSocket-confirmed `Device.DisplayingApp`, then falls back to the legacy expanded
rotation list at `LastAppIndex`. It reads the existing WebP and does not render.
`GET /{id}/next`, in contrast, advances rotation, may render stale apps, and
updates device state.

## Shared mobile services

`mobile_api.go` contains transport-neutral helpers used by the JSON adapters:

* catalogue aggregation and safe app-path resolution;
* Pixlet schema loading and normalization;
* configuration patch validation, Pixlet Boolean conversion, and redaction;
* rendering and rendered-image persistence;
* complete-list ordering validation and transactional persistence.

The API does not invoke Manager handlers or construct fake requests. Existing
Manager handlers continue to use the same underlying renderer, catalogue
discovery, image paths, ordering model, and dashboard notifications.

Configuration PATCH renders before persistence. Database mutation and image write
then occur within the database transaction callback, so validation or render
failure leaves saved configuration unchanged. A filesystem write followed by a
database commit failure can leave an unreferenced new image; relational state is
still rolled back. This is the unavoidable boundary between SQLite/GORM and the
filesystem without introducing a content store.

Installation creation similarly validates and renders before inserting. Duplicate
installation of the same resolved catalogue path is rejected. Ordering requires
every installation, including disabled installations, exactly once. Thus disabled
apps keep their enabled state while participating in the authoritative order.

## Preview semantics

`GET /v0/devices/{deviceID}/preview` represents the frame Manager calls
“current”: the confirmed `DisplayingApp` for WebSocket/HTTP devices when
available, otherwise the current legacy rotation index. It does not consume a
pushed ephemeral queue, advance rotation, select the next app, or render. Pinned
and night-mode apps appear only after normal device delivery logic marks them
current. The original animated WebP bytes are returned unchanged.

The ETag is a SHA-256 digest of those exact bytes. `Last-Modified` is the
installation's last successful render time (or last render time as fallback).

## Security properties

All routes added here are behind `APIAuthMiddleware`. Every device route is also
behind `RequireDevice`; installation lookup is then constrained to the device
already in context. Catalogue metadata, schemas, and icons require a valid Bearer
key but are read-only. Paths always originate in configured catalogue metadata
and are resolved with `securejoin`; API callers cannot provide a source path.

Mutation bodies require `application/json`, are limited to 1 MiB, reject unknown
top-level properties, and return typed JSON errors. Configuration properties are
checked against the live schema. Secret values are accepted only as writes,
preserved when omitted, and never returned.

