package server

import (
	"context"
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
