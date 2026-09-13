package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"tronbyt-server/internal/data"
	"tronbyt-server/internal/providers"
)

func TestMarketOwnerLabelsMultipleKeysAndDeviceAssignments(t *testing.T) {
	s := newTestServerAPI(t)
	configureProductizationServices(t, s)
	for _, id := range []string{"carter-market", "ben-market"} {
		body, _ := json.Marshal(map[string]string{"provider": "twelve-data", "label": id, "secret": "offline-test-" + id})
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, newAPIRequest(http.MethodPut, "/v0/provider-credentials/"+id, "test_api_key", body))
		require.Equal(t, 200, rec.Code)
		require.NotContains(t, rec.Body.String(), "offline-test-")
	}
	require.NoError(t, s.DB.Create(&data.Device{ID: "second-frame", Username: "testuser", APIKey: "offline-device"}).Error)
	for device, id := range map[string]string{"testdevice": "carter-market", "second-frame": "ben-market"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, newAPIRequest(http.MethodPut, "/v0/devices/"+device+"/market-credential", "test_api_key", []byte(`{"credentialID":"`+id+`"}`)))
		require.Equal(t, 200, rec.Code)
		require.Contains(t, rec.Body.String(), id)
		require.NotContains(t, rec.Body.String(), "offline-test-")
	}
	list := httptest.NewRecorder()
	s.ServeHTTP(list, newAPIRequest(http.MethodGet, "/v0/provider-credentials", "test_api_key", nil))
	require.Equal(t, 200, list.Code)
	require.Contains(t, list.Body.String(), `"usedBy":["second-frame"]`)
	// Wrong provider, disabled and cross-owner credentials cannot be assigned.
	for _, id := range []string{"weather", "foreign", "disabled"} {
		provider, owner := "twelve-data", "testuser"
		if id == "weather" {
			provider = "openweather"
		}
		if id == "foreign" {
			owner = "another-owner"
		}
		_, err := s.CredentialStore.Put(t.Context(), id, provider, "server_owner", owner, "offline-test")
		require.NoError(t, err)
		if id == "disabled" {
			_, err = s.CredentialStore.SetEnabled(t.Context(), id, "server_owner", owner, false)
			require.NoError(t, err)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, newAPIRequest(http.MethodPut, "/v0/devices/testdevice/market-credential", "test_api_key", []byte(`{"credentialID":"`+id+`"}`)))
		require.Equal(t, 422, rec.Code)
	}
}
func TestMarketMemberCannotAssignManageSecretsOrSelectAnotherKey(t *testing.T) {
	s := newTestServerAPI(t)
	configureProductizationServices(t, s)
	_, err := s.CredentialStore.Put(t.Context(), "ben-market", "twelve-data", "server_owner", "testuser", "offline-assigned")
	require.NoError(t, err)
	require.NoError(t, s.DB.Model(&data.Device{ID: "testdevice"}).Update("market_credential_id", "ben-market").Error)
	member, err := s.Provisioning.CreateMember(t.Context(), "testuser", "Ben", []string{"testdevice"})
	require.NoError(t, err)
	code, _, err := s.Provisioning.CreatePairingCode(t.Context(), "testuser", member.ID, time.Minute)
	require.NoError(t, err)
	result, err := s.Provisioning.Redeem(t.Context(), code, time.Hour)
	require.NoError(t, err)
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPut, "/v0/devices/testdevice/market-credential", `{"credentialID":"carter-market"}`},
		{http.MethodGet, "/v0/provider-credentials", ""},
		{http.MethodPut, "/v0/provider-credentials/new", `{"provider":"twelve-data","secret":"offline-test"}`},
		{http.MethodPatch, "/v0/provider-credentials/ben-market", `{"enabled":false}`},
		{http.MethodDelete, "/v0/provider-credentials/ben-market", ""},
		{http.MethodPost, "/v0/provider-credentials/ben-market/validate", ""},
	} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, newAPIRequest(route.method, route.path, result.SessionToken, []byte(route.body)))
		require.Equal(t, 403, rec.Code)
		require.NotContains(t, rec.Body.String(), "offline-assigned")
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, newAPIRequest(http.MethodGet, "/v0/devices/testdevice/market-credential", result.SessionToken, nil))
	require.Equal(t, 200, rec.Code)
	require.JSONEq(t, `{"configured":true}`, rec.Body.String())
	provider := &assignedMarketSearch{}
	s.MarketProvider = provider
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, newAPIRequest(http.MethodGet, "/v0/devices/testdevice/market/search?q=Plaza&credentialID=carter-market", result.SessionToken, nil))
	require.Equal(t, 200, rec.Code)
	require.Equal(t, "ben-market", provider.credential)
	request := newAPIRequest(http.MethodPatch, "/", result.SessionToken, nil)
	request = request.WithContext(context.WithValue(request.Context(), apiPrincipalContextKey, apiPrincipalMember))
	require.True(t, memberMarketCredentialChangeForbidden(request, "market-watch", map[string]any{"credential_id": ""}, map[string]any{"credential_id": "carter-market"}))
	require.False(t, memberMarketCredentialChangeForbidden(request, "market-watch", map[string]any{"credential_id": ""}, map[string]any{"symbols": "PLZ.UN"}))
}

type assignedMarketSearch struct{ credential string }

func (p *assignedMarketSearch) Quotes(context.Context, providers.MarketRequest) ([]providers.MarketQuote, error) {
	return nil, nil
}
func (p *assignedMarketSearch) Search(_ context.Context, r providers.MarketSearchRequest) ([]providers.MarketListing, error) {
	p.credential = r.CredentialID
	return []providers.MarketListing{}, nil
}
func TestMarketAssignmentResolutionAndLogoRenderRevision(t *testing.T) {
	s := &Server{}
	d := &data.Device{Username: "owner", MarketCredentialID: "ben"}
	for explicit, want := range map[string]string{"": "ben", "market-primary": "ben", "__device__": "ben", "carter": "carter"} {
		id, err := s.marketCredential(t.Context(), d, explicit)
		require.NoError(t, err)
		require.Equal(t, want, id)
	}
	app := &data.App{Name: "market-watch", Config: map[string]any{"symbols": "PLZ.UN", "display_mode": "ticker"}}
	h := renderContextHash(d, app)
	d.MarketCredentialID = "carter"
	require.NotEqual(t, h, renderContextHash(d, app))
	app.Config["display_mode"] = "focus"
	require.NotEqual(t, h, renderContextHash(d, app))
	d.MarketCredentialID = ""
	id, err := s.marketCredential(t.Context(), d, "")
	require.NoError(t, err)
	require.Equal(t, "market-primary", id)
	encoded, _ := json.Marshal(d)
	require.False(t, strings.Contains(string(encoded), "marketCredential"))
}
