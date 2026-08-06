package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"tronbyt-server/internal/apps"
	"tronbyt-server/internal/data"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestNormalizeSchemaBytes(t *testing.T) {
	raw := []byte(`{
		"version":"1",
		"schema":[
			{"type":"onoff","id":"show_team_background","name":"Show team background","default":"true"},
			{"type":"dropdown","id":"team","name":"Team","default":"tor","options":[
				{"display":"Toronto","text":"Toronto","value":"tor"},
				{"display":"Seattle","text":"Seattle","value":"sea"}
			]},
			{"type":"number","id":"opacity","name":"Opacity","minimum":0,"maximum":1},
			{"type":"text","id":"token","name":"Token","secret":true}
		]
	}`)

	schema, err := normalizeSchemaBytes(raw)
	require.NoError(t, err)
	require.Len(t, schema.Fields, 4)
	assert.Equal(t, "boolean", schema.Fields[0].Type)
	assert.Equal(t, true, schema.Fields[0].Default)
	assert.Equal(t, "enum", schema.Fields[1].Type)
	assert.Equal(t, "Toronto", schema.Fields[1].Options[0].Label)
	assert.Equal(t, "number", schema.Fields[2].Type)
	assert.Equal(t, "secret", schema.Fields[3].Type)
	assert.True(t, schema.Fields[3].Secret)
}

func TestConfigurationSchemaJSONContractOmitsEmptyOptions(t *testing.T) {
	schema := normalizedSchema{Version: "1", Fields: []normalizedSchemaField{{
		Key: "show_team_colored_logo_background", Title: "Team-colour background",
		Type: "boolean", Default: true,
	}}}
	payload, err := json.Marshal(schema)
	require.NoError(t, err)
	var decoded struct {
		Fields []map[string]any `json:"fields"`
	}
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Len(t, decoded.Fields, 1)
	assert.Equal(t, "boolean", decoded.Fields[0]["type"])
	assert.Equal(t, true, decoded.Fields[0]["default"])
	assert.NotContains(t, decoded.Fields[0], "options", "clients must tolerate omitted options for non-enum controls")
}

func TestValidateConfigPatchAndSecretSanitization(t *testing.T) {
	minimum, maximum := 0.0, 10.0
	schema := normalizedSchema{Version: "1", Fields: []normalizedSchemaField{
		{Key: "enabled", Type: "boolean"},
		{Key: "mode", Type: "enum", Options: []normalizedSchemaOption{{Label: "A", Value: "a"}}},
		{Key: "count", Type: "integer", Minimum: &minimum, Maximum: &maximum},
		{Key: "token", Type: "secret", Secret: true},
	}}
	existing := map[string]any{"enabled": "true", "token": "saved-secret"}

	updated, fields := validateConfigPatch(schema, existing, map[string]any{
		"enabled": false,
		"mode":    "a",
		"count":   float64(4),
		"token":   map[string]any{"keepExisting": true},
	})
	require.Empty(t, fields)
	assert.Equal(t, "false", updated["enabled"])
	assert.Equal(t, "saved-secret", updated["token"])

	values, secrets := sanitizeConfig(schema, updated)
	assert.Equal(t, false, values["enabled"])
	assert.NotContains(t, values, "token")
	assert.True(t, secrets["token"])

	_, fields = validateConfigPatch(schema, existing, map[string]any{"unknown": true, "mode": "bad", "count": 11.0})
	assert.Contains(t, fields, "unknown")
	assert.Contains(t, fields, "mode")
	assert.Contains(t, fields, "count")
}

func TestMLBBackgroundLegacyMigrationAndRequiredTeamSchema(t *testing.T) {
	raw := []byte(`{"version":"1","schema":[
		{"type":"dropdown","id":"team","name":"Team Focus","default":"141","options":[{"display":"Toronto Blue Jays","value":"141"}]},
		{"type":"dropdown","id":"team_color_background_style","name":"Team-colour background","default":"full","options":[{"display":"Off","value":"off"},{"display":"Dim","value":"dim"},{"display":"Full","value":"full"}]}
	]}`)
	schema, err := normalizeSchemaBytes(raw)
	require.NoError(t, err)
	require.True(t, schema.Fields[0].Required)

	full, fields := validateConfigPatch(schema, map[string]any{"team": "141", "show_team_colored_logo_background": true}, nil)
	require.Empty(t, fields)
	assert.Equal(t, "full", full["team_color_background_style"])
	assert.NotContains(t, full, "show_team_colored_logo_background")

	off, fields := validateConfigPatch(schema, map[string]any{"team": "141", "show_team_coloured_logo_background": false}, nil)
	require.Empty(t, fields)
	assert.Equal(t, "off", off["team_color_background_style"])

	_, fields = validateConfigPatch(schema, nil, map[string]any{"team_color_background_style": "dim"})
	assert.Equal(t, "is required", fields["team"])
}

func TestMobilePreviewAndETag(t *testing.T) {
	s := newTestServerAPI(t)
	ctx := context.Background()
	path := "system-apps/apps/test/test.star"
	app := data.App{
		DeviceID: "testdevice", Iname: "100", Name: "test", Path: &path,
		Enabled: true, Order: 0,
	}
	require.NoError(t, gorm.G[data.App](s.DB).Create(ctx, &app))
	displaying := app.Iname
	_, err := gorm.G[data.Device](s.DB).Where("id = ?", "testdevice").Update(ctx, "displaying_app", displaying)
	require.NoError(t, err)
	dir := filepath.Join(s.DataDir, "webp", "testdevice")
	require.NoError(t, os.MkdirAll(dir, 0755))
	image := []byte("RIFF-fake-animated-webp")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "test-100.webp"), image, 0644))

	req := newAPIRequest(http.MethodGet, "/v0/devices/testdevice/preview", "device_api_key", nil)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "image/webp", rr.Header().Get("Content-Type"))
	assert.Equal(t, image, rr.Body.Bytes())
	etag := rr.Header().Get("ETag")
	require.NotEmpty(t, etag)

	req = newAPIRequest(http.MethodGet, "/v0/devices/testdevice/preview", "device_api_key", nil)
	req.Header.Set("If-None-Match", etag)
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotModified, rr.Code)
	assert.Empty(t, rr.Body.Bytes())
}

func TestMobilePreviewAuthorizationAndMissing(t *testing.T) {
	s := newTestServerAPI(t)
	for _, test := range []struct {
		name, key string
		status    int
	}{
		{name: "missing key", status: http.StatusUnauthorized},
		{name: "invalid key", key: "wrong", status: http.StatusUnauthorized},
		{name: "authorized but no preview", key: "device_api_key", status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v0/devices/testdevice/preview", nil)
			if test.key != "" {
				req.Header.Set("Authorization", "Bearer "+test.key)
			}
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, req)
			assert.Equal(t, test.status, rr.Code)
		})
	}
}

func TestInstallationPreviewIsScopedToRealInstallation(t *testing.T) {
	s := newTestServerAPI(t)
	ctx := context.Background()
	path := "system-apps/apps/clock/clock.star"
	app := data.App{DeviceID: "testdevice", Iname: "clock-main", Name: "clock", Path: &path, Enabled: true}
	require.NoError(t, gorm.G[data.App](s.DB).Create(ctx, &app))
	dir := filepath.Join(s.DataDir, "webp", "testdevice")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	content := []byte("RIFF-installation-preview")
	require.NoError(t, os.WriteFile(s.getAppWebpPath(dir, &app), content, 0o644))

	req := newAPIRequest(http.MethodGet, "/v0/devices/testdevice/installations/clock-main/preview", "device_api_key", nil)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, content, rr.Body.Bytes())
	assert.NotEmpty(t, rr.Header().Get("ETag"))

	req = newAPIRequest(http.MethodGet, "/v0/devices/testdevice/installations/missing/preview", "device_api_key", nil)
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestCapabilitiesDetectAuthorizationScope(t *testing.T) {
	s := newTestServerAPI(t)
	for _, test := range []struct {
		name, key, scope string
	}{
		{name: "device key", key: "device_api_key", scope: "device"},
		{name: "user key", key: "test_api_key", scope: "user"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := newAPIRequest(http.MethodGet, "/v0/capabilities", test.key, nil)
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, req)
			require.Equal(t, http.StatusOK, rr.Code)
			var payload capabilityResponse
			require.NoError(t, json.NewDecoder(rr.Body).Decode(&payload))
			assert.Equal(t, test.scope, payload.AuthorizationScope)
			assert.Contains(t, payload.Features, "device-diagnostics")
			assert.NotEmpty(t, payload.IconRevision)
		})
	}
}

func TestDeviceDiagnosticsAreScopedAndSanitized(t *testing.T) {
	s := newTestServerAPI(t)
	ctx := context.Background()
	path := "system-apps/apps/test/test.star"
	app := data.App{
		DeviceID: "testdevice", Iname: "clock-main", Name: "clock", Path: &path,
		Enabled: true, LastRenderResult: "failure", LastRenderMessage: "https://private.example/render?token=secret-value",
	}
	require.NoError(t, gorm.G[data.App](s.DB).Create(ctx, &app))
	s.diagnosticsEvents.add("testdevice", "render_failure", "token=secret-value", app.Iname)

	req := newAPIRequest(http.MethodGet, "/v0/devices/testdevice/diagnostics", "device_api_key", nil)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.NotContains(t, rr.Body.String(), "secret-value")
	var payload deviceDiagnostics
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&payload))
	assert.Equal(t, "testdevice", payload.DeviceID)
	require.Len(t, payload.Apps, 1)
	assert.Equal(t, "failure", payload.Apps[0].RenderResult)

	other := data.Device{ID: "otherdevice", Username: "otheruser", Name: "Other", APIKey: "other_key"}
	require.NoError(t, gorm.G[data.Device](s.DB).Create(ctx, &other))
	req = newAPIRequest(http.MethodGet, "/v0/devices/otherdevice/diagnostics", "device_api_key", nil)
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestDeviceKeyListsOnlyAuthorizedDevice(t *testing.T) {
	s := newTestServerAPI(t)
	other := data.Device{ID: "otherdevice", Username: "testuser", Name: "Other", APIKey: "other_device_key"}
	require.NoError(t, gorm.G[data.Device](s.DB).Create(context.Background(), &other))

	req := newAPIRequest(http.MethodGet, "/v0/devices", "device_api_key", nil)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	var payload ListDevicesPayload
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&payload))
	require.Len(t, payload.Devices, 1)
	assert.Equal(t, "testdevice", payload.Devices[0].ID)
}

func TestInstalledAppsExcludeTemporaryPushedContent(t *testing.T) {
	s := newTestServerAPI(t)
	ctx := context.Background()
	path := "system-apps/apps/clock/clock.star"
	appsToCreate := []data.App{
		{DeviceID: "testdevice", Iname: "clock-main", Name: "clock", Path: &path, Enabled: true, Order: 0},
		{DeviceID: "testdevice", Iname: "102", Name: "pushed", Pushed: true, Enabled: true, Order: 1},
	}
	for i := range appsToCreate {
		require.NoError(t, gorm.G[data.App](s.DB).Create(ctx, &appsToCreate[i]))
	}

	req := newAPIRequest(http.MethodGet, "/v0/devices/testdevice/installations", "device_api_key", nil)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"id":"clock-main"`)
	assert.NotContains(t, rr.Body.String(), `"id":"102"`)
	assert.Contains(t, rr.Body.String(), `"lastRenderAt":null`)

	req = newAPIRequest(http.MethodGet, "/v0/devices/testdevice/installations/102", "device_api_key", nil)
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestLegacyBrightnessPowerOnRestoresNormalRotationAndNeverPushedContent(t *testing.T) {
	s := newTestServerAPI(t)
	ctx := context.Background()
	clockPath := "system-apps/apps/clock/clock.star"
	firstPath := "system-apps/apps/weather/weather.star"
	pushedPath := "pushed:side-eye"
	appsToCreate := []data.App{
		{DeviceID: "testdevice", Iname: "weather", Name: "weather", Path: &firstPath, Enabled: true, Order: 0},
		{DeviceID: "testdevice", Iname: "clock-main", Name: "clock", Path: &clockPath, Enabled: true, Order: 1},
		{DeviceID: "testdevice", Iname: "999", Name: "pushed", Path: &pushedPath, Pushed: true, Enabled: true, Order: 2},
	}
	for i := range appsToCreate {
		require.NoError(t, gorm.G[data.App](s.DB).Create(ctx, &appsToCreate[i]))
	}
	_, err := gorm.G[data.Device](s.DB).Where("id = ?", "testdevice").Updates(ctx, data.Device{
		Brightness:    70,
		DisplayingApp: new("999"),
	})
	require.NoError(t, err)

	req := newAPIRequest(http.MethodPatch, "/v0/devices/testdevice", "device_api_key", []byte(`{"brightness":0}`))
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	device, err := gorm.G[data.Device](s.DB).Where("id = ?", "testdevice").First(ctx)
	require.NoError(t, err)
	require.NotNil(t, device.DisplayRestoreApp)
	assert.Equal(t, "weather", *device.DisplayRestoreApp)
	assert.Nil(t, device.DisplayingApp)

	req = newAPIRequest(http.MethodPatch, "/v0/devices/testdevice", "device_api_key", []byte(`{"brightness":70}`))
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	device, err = gorm.G[data.Device](s.DB).Where("id = ?", "testdevice").First(ctx)
	require.NoError(t, err)
	require.NotNil(t, device.DisplayingApp)
	assert.Equal(t, "weather", *device.DisplayingApp)
	assert.NotEqual(t, "999", *device.DisplayingApp)
}

func TestDisplayRestoreFallsBackToFirstEnabledRealApp(t *testing.T) {
	device := data.Device{Apps: []*data.App{
		{Iname: "pushed", Name: "pushed", Pushed: true, Enabled: true, Order: 0},
		{Iname: "disabled", Name: "clock", Enabled: false, Order: 1},
		{Iname: "weather", Name: "weather", Enabled: true, Order: 2},
	}}
	target := displayRestoreTarget(&device)
	require.NotNil(t, target)
	assert.Equal(t, "weather", target.Iname)
}

func TestCatalogueListingFiltersAndPagination(t *testing.T) {
	s := newTestServerAPI(t)
	s.systemAppsCache = []apps.AppMetadata{
		{Manifest: apps.Manifest{ID: "clock", Name: "Clock", Summary: "Time", Category: "utility", Tags: []string{"time"}}},
		{Manifest: apps.Manifest{ID: "mlb", Name: "MLB", Summary: "Baseball", Category: "sports", Tags: []string{"baseball"}}},
	}
	customDir := filepath.Join(s.DataDir, "users", "testuser", "repo", "apps", "custom-clock")
	require.NoError(t, os.MkdirAll(customDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(customDir, "custom-clock.star"), []byte("def main(config):\n    return []\n"), 0644))

	req := newAPIRequest(http.MethodGet, "/v0/catalogue?category=sports&limit=1&offset=0", "device_api_key", nil)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	var payload struct {
		Apps  []catalogueApp `json:"apps"`
		Total int            `json:"total"`
	}
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&payload))
	require.Len(t, payload.Apps, 1)
	assert.Equal(t, "mlb", payload.Apps[0].ID)
	assert.Equal(t, 1, payload.Total)

	req = newAPIRequest(http.MethodGet, "/v0/catalogue?search=baseball", "device_api_key", nil)
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"id":"mlb"`)
	assert.NotContains(t, rr.Body.String(), `"id":"clock"`)

	req = newAPIRequest(http.MethodGet, "/v0/catalogue?repository=custom-repository", "device_api_key", nil)
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"id":"custom-clock"`)
	assert.Contains(t, rr.Body.String(), `"repository":"custom-repository"`)
}

func TestCatalogueIconCachingAndDecodeLimits(t *testing.T) {
	s := newTestServerAPI(t)
	dir := filepath.Join(s.DataDir, "system-apps", "apps", "icon-test")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	pngBytes, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "icon.png"), pngBytes, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.png"), []byte("not an image"), 0o644))
	s.systemAppsCache = []apps.AppMetadata{
		{Manifest: apps.Manifest{ID: "icon-test", Name: "Icon Test"}, Path: "system-apps/apps/icon-test/icon-test.star", Preview: "icon.png"},
		{Manifest: apps.Manifest{ID: "broken-icon", Name: "Broken Icon"}, Path: "system-apps/apps/icon-test/broken-icon.star", Preview: "broken.png"},
	}

	req := newAPIRequest(http.MethodGet, "/v0/catalogue/icon-test/icon", "device_api_key", nil)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.NotEmpty(t, rr.Header().Get("ETag"))
	assert.Contains(t, rr.Header().Get("Cache-Control"), "stale-while-revalidate")

	req = newAPIRequest(http.MethodGet, "/v0/catalogue/broken-icon/icon", "device_api_key", nil)
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
}

func seedRenderableCatalogueApp(t *testing.T, s *Server) {
	t.Helper()
	dir := filepath.Join(s.DataDir, "system-apps", "apps", "mobiletest")
	require.NoError(t, os.MkdirAll(dir, 0755))
	source := `load("render.star", "render")
load("schema.star", "schema")

def main(config):
    value = "enabled" if config.bool("enabled") else "disabled"
    return render.Root(child = render.Text(value))

def get_schema():
    return schema.Schema(
        version = "1",
        fields = [
            schema.Toggle(
                id = "enabled",
                name = "Enabled",
                desc = "Whether the test is enabled.",
                icon = "toggleOn",
                default = True,
            ),
        ],
    )
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mobiletest.star"), []byte(source), 0644))
	s.systemAppsCache = []apps.AppMetadata{{
		Manifest: apps.Manifest{
			ID: "mobiletest", Name: "Mobile test", FileName: "mobiletest.star",
		},
		Path: filepath.Join("system-apps", "apps", "mobiletest"),
	}}
}

func TestInstallationCreateConfigAndDuplicate(t *testing.T) {
	s := newTestServerAPI(t)
	seedRenderableCatalogueApp(t, s)

	createBody := []byte(`{
		"appID":"mobiletest",
		"name":"Mobile test",
		"config":{"enabled":true},
		"enabled":true,
		"displayTimeSec":15,
		"renderIntervalMin":5
	}`)
	req := newAPIRequest(http.MethodPost, "/v0/devices/testdevice/installations", "device_api_key", createBody)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	var created struct {
		Installation AppPayload     `json:"installation"`
		Config       map[string]any `json:"config"`
	}
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&created))
	require.NotEmpty(t, created.Installation.ID)
	assert.Equal(t, true, created.Config["enabled"])

	configURL := fmt.Sprintf("/v0/devices/testdevice/installations/%s/config", created.Installation.ID)
	req = newAPIRequest(http.MethodPatch, configURL, "device_api_key", []byte(`{"config":{"enabled":false}}`))
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"enabled":false`)

	req = newAPIRequest(http.MethodPatch, configURL, "device_api_key", []byte(`{"config":{"unknown":true}}`))
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Contains(t, rr.Body.String(), `"unknown":"unknown configuration field"`)

	req = newAPIRequest(http.MethodPost, "/v0/devices/testdevice/installations", "device_api_key", createBody)
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusConflict, rr.Code)
}

func TestInstallationCreateIdempotentRetryUsesMutationID(t *testing.T) {
	s := newTestServerAPI(t)
	seedRenderableCatalogueApp(t, s)
	body := []byte(`{
		"appID":"mobiletest","name":"Mobile test","config":{"enabled":true},
		"enabled":true,"displayTimeSec":15,"renderIntervalMin":5,
		"mutationID":"install-retry-001"
	}`)
	request := func(payload []byte) *httptest.ResponseRecorder {
		req := newAPIRequest(http.MethodPost, "/v0/devices/testdevice/installations", "device_api_key", payload)
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		return rr
	}
	first := request(body)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	var firstPayload struct {
		Installation AppPayload `json:"installation"`
	}
	require.NoError(t, json.NewDecoder(first.Body).Decode(&firstPayload))

	retry := request(body)
	require.Equal(t, http.StatusOK, retry.Code, retry.Body.String())
	var retryPayload struct {
		Installation AppPayload `json:"installation"`
	}
	require.NoError(t, json.NewDecoder(retry.Body).Decode(&retryPayload))
	assert.Equal(t, firstPayload.Installation.ID, retryPayload.Installation.ID)

	var device data.Device
	require.NoError(t, s.DB.First(&device, "id = ?", "testdevice").Error)
	assert.Equal(t, "install-retry-001", device.LastMutationID)
	assert.Equal(t, "installation_created", device.LastMutationResult)
}

func TestInstallationCreateRejectsInvalidAppAndForeignDevice(t *testing.T) {
	s := newTestServerAPI(t)
	seedRenderableCatalogueApp(t, s)
	other := data.Device{ID: "otherdevice", Username: "otheruser", Name: "Other", APIKey: "other_key"}
	require.NoError(t, gorm.G[data.Device](s.DB).Create(context.Background(), &other))

	body := []byte(`{"appID":"missing","config":{},"enabled":true,"displayTimeSec":15,"renderIntervalMin":5}`)
	req := newAPIRequest(http.MethodPost, "/v0/devices/testdevice/installations", "device_api_key", body)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code)

	body = []byte(`{"appID":"mobiletest","config":{"enabled":true},"enabled":true,"displayTimeSec":15,"renderIntervalMin":5}`)
	req = newAPIRequest(http.MethodPost, "/v0/devices/otherdevice/installations", "device_api_key", body)
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestInstallationCreateRenderFailureDoesNotPersist(t *testing.T) {
	s := newTestServerAPI(t)
	dir := filepath.Join(s.DataDir, "system-apps", "apps", "broken-render")
	require.NoError(t, os.MkdirAll(dir, 0755))
	source := `load("schema.star", "schema")

def main(config):
    return 1 / 0

def get_schema():
    return schema.Schema(version = "1", fields = [])
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken-render.star"), []byte(source), 0644))
	s.systemAppsCache = []apps.AppMetadata{{
		Manifest: apps.Manifest{ID: "broken-render", Name: "Broken render", FileName: "broken-render.star"},
		Path:     filepath.Join("system-apps", "apps", "broken-render"),
	}}

	body := []byte(`{"appID":"broken-render","config":{},"enabled":true,"displayTimeSec":15,"renderIntervalMin":5}`)
	req := newAPIRequest(http.MethodPost, "/v0/devices/testdevice/installations", "device_api_key", body)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	count, err := gorm.G[data.App](s.DB).Where("device_id = ?", "testdevice").Count(context.Background(), "*")
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)
}

func TestInstallationOrderingValidationAndSuccess(t *testing.T) {
	s := newTestServerAPI(t)
	ctx := context.Background()
	for i, id := range []string{"100", "200", "300"} {
		app := data.App{DeviceID: "testdevice", Iname: id, Name: id, Order: i, Enabled: i != 1}
		require.NoError(t, gorm.G[data.App](s.DB).Create(ctx, &app))
	}

	for name, body := range map[string]string{
		"partial":   `{"installationIDs":["100","200"]}`,
		"duplicate": `{"installationIDs":["100","100","300"]}`,
		"foreign":   `{"installationIDs":["100","200","999"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := newAPIRequest(http.MethodPatch, "/v0/devices/testdevice/installations/order", "device_api_key", []byte(body))
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
			unchanged, err := gorm.G[data.App](s.DB).Where("device_id = ?", "testdevice").Order("`order` ASC").Find(ctx)
			require.NoError(t, err)
			assert.Equal(t, []string{"100", "200", "300"}, []string{unchanged[0].Iname, unchanged[1].Iname, unchanged[2].Iname})
		})
	}

	req := newAPIRequest(http.MethodPatch, "/v0/devices/testdevice/installations/order", "device_api_key",
		[]byte(`{"installationIDs":["300","100","200"]}`))
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	ordered, err := gorm.G[data.App](s.DB).Where("device_id = ?", "testdevice").Order("`order` ASC").Find(ctx)
	require.NoError(t, err)
	require.Len(t, ordered, 3)
	assert.Equal(t, []string{"300", "100", "200"}, []string{ordered[0].Iname, ordered[1].Iname, ordered[2].Iname})
	assert.False(t, ordered[2].Enabled, fmt.Sprintf("disabled installation should remain disabled: %#v", ordered[2]))
}

func TestInstallationOrderingRejectsTemporaryPushedIDsButDoesNotRequireThem(t *testing.T) {
	s := newTestServerAPI(t)
	ctx := context.Background()
	for i, app := range []data.App{
		{DeviceID: "testdevice", Iname: "100", Name: "clock", Enabled: true, Order: 0},
		{DeviceID: "testdevice", Iname: "200", Name: "weather", Enabled: true, Order: 1},
		{DeviceID: "testdevice", Iname: "999", Name: "pushed", Pushed: true, Enabled: true, Order: 2},
	} {
		app.Order = i
		require.NoError(t, gorm.G[data.App](s.DB).Create(ctx, &app))
	}

	req := newAPIRequest(http.MethodPatch, "/v0/devices/testdevice/installations/order", "device_api_key",
		[]byte(`{"installationIDs":["200","100"]}`))
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	req = newAPIRequest(http.MethodPatch, "/v0/devices/testdevice/installations/order", "device_api_key",
		[]byte(`{"installationIDs":["999","100"]}`))
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Contains(t, rr.Body.String(), "temporary pushed content")
}

func TestTemporaryPushedInstallationCannotBeDeleted(t *testing.T) {
	s := newTestServerAPI(t)
	ctx := context.Background()
	path := "pushed:side-eye"
	app := data.App{DeviceID: "testdevice", Iname: "999", Name: "pushed", Path: &path, Pushed: true, Enabled: true}
	require.NoError(t, gorm.G[data.App](s.DB).Create(ctx, &app))

	req := newAPIRequest(http.MethodDelete, "/v0/devices/testdevice/installations/999", "device_api_key", nil)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	count, err := gorm.G[data.App](s.DB).Where("id = ?", app.ID).Count(ctx, "*")
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}

func TestDeletingPinnedAndDisplayedInstallationClearsDeviceReferences(t *testing.T) {
	s := newTestServerAPI(t)
	ctx := context.Background()
	app := data.App{DeviceID: "testdevice", Iname: "clock", Name: "clock", Enabled: true}
	require.NoError(t, gorm.G[data.App](s.DB).Create(ctx, &app))
	fallback := data.App{DeviceID: "testdevice", Iname: "weather", Name: "weather", Enabled: true}
	require.NoError(t, gorm.G[data.App](s.DB).Create(ctx, &fallback))
	_, err := gorm.G[data.Device](s.DB).Where("id = ?", "testdevice").
		Select("PinnedApp", "DisplayingApp", "DisplayRestoreApp").
		Updates(ctx, data.Device{
			PinnedApp:         new("clock"),
			DisplayingApp:     new("clock"),
			DisplayRestoreApp: new("clock"),
		})
	require.NoError(t, err)

	req := newAPIRequest(http.MethodDelete, "/v0/devices/testdevice/installations/clock", "device_api_key", nil)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	device, err := gorm.G[data.Device](s.DB).Where("id = ?", "testdevice").First(ctx)
	require.NoError(t, err)
	assert.Nil(t, device.PinnedApp)
	assert.Nil(t, device.DisplayingApp)
	assert.Nil(t, device.DisplayRestoreApp)
}
