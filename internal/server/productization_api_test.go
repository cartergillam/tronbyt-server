package server

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tronbyt-server/internal/apps"
	"tronbyt-server/internal/credentials"
	"tronbyt-server/internal/data"
	"tronbyt-server/internal/provisioning"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func configureProductizationServices(t *testing.T, s *Server) {
	t.Helper()
	pairing, err := provisioning.NewService(s.DB, "server-test-pairing-secret-with-adequate-length")
	require.NoError(t, err)
	s.Provisioning = pairing
	key := make([]byte, 32)
	_, err = rand.Read(key)
	require.NoError(t, err)
	store, err := credentials.NewStore(s.DB, base64.StdEncoding.EncodeToString(key))
	require.NoError(t, err)
	s.CredentialStore = store
}

func TestOwnerProvisionedPairingProducesDeviceScopedRevocableSession(t *testing.T) {
	s := newTestServerAPI(t)
	configureProductizationServices(t, s)
	require.NoError(t, gorm.G[data.Device](s.DB).Create(t.Context(), &data.Device{
		ID: "unassigned", Username: "testuser", Name: "Unassigned", APIKey: "unassigned-key",
	}))

	request := newAPIRequest(http.MethodPost, "/v0/household/members", "test_api_key", []byte(`{"displayName":"Family","deviceIDs":["testdevice"]}`))
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
	var memberResponse struct {
		Member data.HouseholdMember `json:"member"`
	}
	require.NoError(t, json.NewDecoder(recorder.Body).Decode(&memberResponse))

	request = newAPIRequest(http.MethodPost, "/v0/household/members/"+memberResponse.Member.ID+"/pairing-codes", "test_api_key", []byte(`{"lifetimeSeconds":600}`))
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
	var codeResponse struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.NewDecoder(recorder.Body).Decode(&codeResponse))
	assert.NotEmpty(t, codeResponse.Code)
	assert.NotContains(t, recorder.Body.String(), "device_api_key")
	assert.NotContains(t, recorder.Body.String(), "test_api_key")

	request = httptest.NewRequest(http.MethodPost, "/v0/pairing/redeem", bytes.NewBufferString(`{"code":"`+codeResponse.Code+`"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var pairingResponse struct {
		SessionToken string   `json:"sessionToken"`
		SessionID    string   `json:"sessionID"`
		DeviceIDs    []string `json:"deviceIDs"`
	}
	require.NoError(t, json.NewDecoder(recorder.Body).Decode(&pairingResponse))
	assert.Equal(t, []string{"testdevice"}, pairingResponse.DeviceIDs)
	assert.True(t, strings.HasPrefix(pairingResponse.SessionToken, "tm_"))

	request = newAPIRequest(http.MethodGet, "/v0/devices", pairingResponse.SessionToken, nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), "testdevice")
	assert.NotContains(t, recorder.Body.String(), "unassigned")
	request = newAPIRequest(http.MethodGet, "/v0/capabilities", pairingResponse.SessionToken, nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), `"authorizationScope":"household_member"`)

	s.systemAppsCache = []apps.AppMetadata{{Manifest: apps.Manifest{ID: "og-clock", Name: "OG Clock", FileName: "fixture.webp"}}}
	require.NoError(t, gorm.G[data.App](s.DB).Create(t.Context(), &data.App{DeviceID: "unassigned", Iname: "clock", Name: "og-clock", Enabled: true}))
	request = newAPIRequest(http.MethodGet, "/v0/catalogue?installed=true&deviceID=unassigned", pairingResponse.SessionToken, nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.NotContains(t, recorder.Body.String(), `"id":"og-clock"`, "member catalogue filters cannot inspect an unassigned device")

	request = newAPIRequest(http.MethodGet, "/v0/devices/unassigned", pairingResponse.SessionToken, nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusNotFound, recorder.Code)
	request = newAPIRequest(http.MethodPost, "/v0/household/members", pairingResponse.SessionToken, []byte(`{"displayName":"Escalation","deviceIDs":["testdevice"]}`))
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusForbidden, recorder.Code)
	request = newAPIRequest(http.MethodGet, "/v0/provider-credentials/weather-primary?scopeType=device&scopeID=testdevice", pairingResponse.SessionToken, nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusForbidden, recorder.Code)

	request = httptest.NewRequest(http.MethodPost, "/v0/pairing/redeem", bytes.NewBufferString(`{"code":"`+codeResponse.Code+`"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusUnauthorized, recorder.Code, "a pairing code is single-use")

	request = newAPIRequest(http.MethodDelete, "/v0/mobile-sessions/"+pairingResponse.SessionID, "test_api_key", nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusNoContent, recorder.Code, recorder.Body.String())
	request = newAPIRequest(http.MethodGet, "/v0/devices", pairingResponse.SessionToken, nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestProviderCredentialRoutesNeverReturnSecretAndRejectMemberOrDeviceScopeEscalation(t *testing.T) {
	s := newTestServerAPI(t)
	configureProductizationServices(t, s)
	body := []byte(`{"provider":"openweather","scopeType":"device","scopeID":"testdevice","secret":"super-private-provider-key"}`)
	request := newAPIRequest(http.MethodPut, "/v0/provider-credentials/weather-primary", "test_api_key", body)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.NotContains(t, recorder.Body.String(), "super-private-provider-key")
	assert.Contains(t, recorder.Body.String(), `"keyVersion":1`)

	var stored data.ProviderCredential
	require.NoError(t, s.DB.First(&stored, "id = ?", "weather-primary").Error)
	assert.NotContains(t, string(stored.Ciphertext), "super-private-provider-key")

	request = newAPIRequest(http.MethodPut, "/v0/provider-credentials/weather-primary", "device_api_key", body)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusForbidden, recorder.Code)

	request = newAPIRequest(http.MethodPut, "/v0/provider-credentials/weather-primary", "test_api_key", []byte(`{"provider":"openweather","scopeType":"device","scopeID":"testdevice","secret":"rotated-key"}`))
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), `"keyVersion":2`)
}

func TestDeviceCredentialIsForbiddenFromEveryMobileAdministrationSurface(t *testing.T) {
	s := newTestServerAPI(t)
	configureProductizationServices(t, s)
	tests := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/v0/capabilities", ""},
		{http.MethodGet, "/v0/catalogue", ""},
		{http.MethodGet, "/v0/devices", ""},
		{http.MethodGet, "/v0/devices/testdevice", ""},
		{http.MethodPatch, "/v0/devices/testdevice", `{}`},
		{http.MethodPost, "/v0/devices/testdevice/installations", `{}`},
		{http.MethodPost, "/v0/household/members", `{}`},
		{http.MethodGet, "/v0/household/members", ""},
		{http.MethodPatch, "/v0/household/members/member", `{}`},
		{http.MethodPost, "/v0/household/members/member/pairing-codes", `{}`},
		{http.MethodDelete, "/v0/mobile-sessions/session", ""},
		{http.MethodGet, "/v0/provider-credentials", ""},
		{http.MethodGet, "/v0/provider-credentials/existing", ""},
		{http.MethodPut, "/v0/provider-credentials/existing", `{}`},
		{http.MethodPatch, "/v0/provider-credentials/existing", `{}`},
		{http.MethodDelete, "/v0/provider-credentials/existing", ""},
		{http.MethodPost, "/v0/provider-credentials/existing/validate", ""},
		{http.MethodPost, "/v0/devices/testdevice/starter-bundles/verified-starter", ""},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			request := newAPIRequest(test.method, test.path, "device_api_key", []byte(test.body))
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, request)
			assert.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
			assert.NotContains(t, recorder.Body.String(), "existing")
		})
	}
}

func TestOwnerCredentialLifecycleListDisableEnableAndDelete(t *testing.T) {
	s := newTestServerAPI(t)
	configureProductizationServices(t, s)
	request := newAPIRequest(http.MethodPut, "/v0/provider-credentials/weather-primary", "test_api_key", []byte(`{"provider":"openweather","secret":"never-return-this"}`))
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.NotContains(t, recorder.Body.String(), "never-return-this")

	request = newAPIRequest(http.MethodGet, "/v0/provider-credentials", "test_api_key", nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), `"id":"weather-primary"`)
	assert.NotContains(t, recorder.Body.String(), "never-return-this")

	request = newAPIRequest(http.MethodPatch, "/v0/provider-credentials/weather-primary", "test_api_key", []byte(`{"enabled":false}`))
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), `"enabled":false`)

	request = newAPIRequest(http.MethodPatch, "/v0/provider-credentials/weather-primary", "test_api_key", []byte(`{"enabled":true}`))
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), `"enabled":true`)

	request = newAPIRequest(http.MethodDelete, "/v0/provider-credentials/weather-primary", "test_api_key", nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusNoContent, recorder.Code, recorder.Body.String())
}
