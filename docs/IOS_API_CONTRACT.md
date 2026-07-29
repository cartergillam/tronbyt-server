# iOS mobile API contract

All endpoints require `Authorization: Bearer <api-key>`. JSON field names are
case-sensitive. Nullable fields below should be decoded with optional Swift
properties. Dates in metadata are repository strings; device timestamps remain
RFC 3339 as documented in `API.md`.

## Preview

`GET /v0/devices/{deviceID}/preview` returns WebP bytes, not JSON:

```http
HTTP/1.1 200 OK
Content-Type: image/webp
Content-Length: 4128
ETag: "a1b2..."
Last-Modified: Tue, 28 Jul 2026 14:20:00 GMT
Cache-Control: no-cache
```

Store the ETag and send `If-None-Match` on refresh. A `304` has no body. A `404`
means no installation frame has rendered yet. Preserve the returned bytes as-is;
they can contain an animated WebP.

## Catalogue

`GET /v0/catalogue?search=baseball&category=sports&limit=50&offset=0`:

```json
{
  "apps": [{
    "id": "mlb",
    "name": "MLB",
    "description": "Live scores",
    "author": "Tronbyt",
    "category": "sports",
    "tags": ["baseball"],
    "repository": "system",
    "iconURL": "/v0/catalogue/mlb/icon",
    "configurable": true,
    "compatible": null,
    "updated": "2026-07-01",
    "recommendedRenderIntervalMin": 5
  }],
  "offset": 0,
  "limit": 50,
  "total": 1,
  "nextOffset": null
}
```

`iconURL`, `compatible`, `nextOffset`, `published`, `updated`, and the recommended
interval are nullable/omittable. Fetch icon URLs with the same Bearer header.
`GET /v0/catalogue/{appID}` returns the same metadata plus `schema` when schema
loading succeeds. URL-encode `appID` as a path segment.

## Schema

`GET /v0/catalogue/{appID}/schema`:

```json
{
  "version": "1",
  "fields": [{
    "key": "show_team_background",
    "title": "Show team background",
    "description": "Use team colours behind the score",
    "type": "boolean",
    "required": false,
    "default": true,
    "secret": false,
    "order": 0,
    "sourceType": "onoff"
  }, {
    "key": "team",
    "title": "Team",
    "type": "enum",
    "required": false,
    "default": "tor",
    "options": [{"label": "Toronto", "value": "tor"}],
    "secret": false,
    "order": 1,
    "sourceType": "dropdown"
  }]
}
```

Supported normalized `type` values include `boolean`, `string`, `secret`,
`integer`, `number`, `enum`, `colour`, `location`, `date`, `time`, `datetime`,
`image`, and `group`. `default`, `minimum`, `maximum`, `options`, `visibility`,
and `sourceType` are optional. Values in `visibility` follow Pixlet's existing
shape.

## Installation configuration

`GET /v0/devices/{deviceID}/installations/{installationID}/config`:

```json
{
  "installation": {
    "id": "123",
    "appID": "mlb",
    "enabled": true,
    "pinned": false,
    "pushed": false,
    "renderIntervalMin": 5,
    "displayTimeSec": 15,
    "lastRenderAt": null,
    "isInactive": false,
    "startTime": null,
    "endTime": null,
    "days": []
  },
  "appID": "mlb",
  "schema": {"version": "1", "fields": []},
  "config": {"show_team_background": false},
  "savedSecrets": {"api_token": true}
}
```

Secret values never occur in `config`. `savedSecrets[key] == true` means the
server has a non-empty saved value.

`lastRenderAt` is nullable. A zero-value server timestamp is serialized as
`null`; clients should continue treating zero, epoch, implausibly old, and
clearly future legacy values as invalid.

PATCH with:

```json
{"config":{"show_team_background":false}}
```

Omitted properties are preserved. For a secret, omission, `null`, or
`{"keepExisting":true}` preserves the saved value. A new string replaces it.
The response is the sanitized GET shape.

## Installation creation

`POST /v0/devices/{deviceID}/installations`:

```json
{
  "appID": "mlb",
  "name": "MLB",
  "config": {},
  "enabled": true,
  "displayTimeSec": 15,
  "renderIntervalMin": 5
}
```

Success is `201` with the sanitized installation-config shape. The current
server model does not store a separate display alias, so `name` is accepted for
forward compatibility while `appID` remains the installed app name.

## Ordering

`PATCH /v0/devices/{deviceID}/installations/order`:

```json
{"installationIDs":["weather-main","mlb-main","clock-main"]}
```

Every persistent installed app, including disabled apps, must occur exactly
once. Temporary pushed-frame records are absent from installation lists, must
not be included, and are rejected if supplied. Success:

```json
{
  "installationIDs": ["weather-main", "mlb-main", "clock-main"],
  "installations": []
}
```

## Errors

New endpoints use:

```json
{
  "error": {
    "code": "invalid_config",
    "message": "Configuration validation failed",
    "fields": {"team": "is not an allowed option"}
  }
}
```

`fields` is optional. Decode `code` as `String` so the client remains
forward-compatible. Relevant statuses are `400`, `401`, `403`/`404`, `409`,
`415`, `422`, `500`, and `502`.
