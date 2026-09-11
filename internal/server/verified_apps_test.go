package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"tronbyt-server/internal/apps"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifiedManifestIsExplicitAndConservative(t *testing.T) {
	for _, id := range []string{"og-clock", "mlb-game", "quote-of-the-day"} {
		metadata, ok := verifiedMetadataFor(id)
		require.True(t, ok, id)
		assert.True(t, metadata.Verified)
		assert.NotEmpty(t, metadata.VerifiedVersion)
		assert.NotEmpty(t, metadata.VerificationDate)
	}
	for _, id := range []string{"cfl-scores", "market-watch", "local-weather"} {
		metadata, ok := verifiedMetadataFor(id)
		require.True(t, ok, id)
		assert.False(t, metadata.Verified)
		assert.True(t, metadata.Candidate)
	}
	_, nwsVerified := verifiedMetadataFor("nws-daily-forecast")
	assert.False(t, nwsVerified)
	assert.NotEmpty(t, verifiedAppsRevision())
	for _, appID := range verifiedStarterBundles["verified-starter"].AppIDs {
		metadata, ok := verifiedMetadataFor(appID)
		assert.True(t, ok && metadata.Verified)
	}
}

func TestVerifiedCatalogueCategoryRankingRevisionAndDirectRoute(t *testing.T) {
	s := newTestServerAPI(t)
	s.systemAppsCache = []apps.AppMetadata{
		{Manifest: apps.Manifest{ID: "unverified-game", Name: "A Game Helper", Category: "sports", FileName: "fixture.webp"}},
		{Manifest: apps.Manifest{ID: "mlb-game", Name: "MLB Game", Category: "sports", FileName: "fixture.webp"}},
		{Manifest: apps.Manifest{ID: "og-clock", Name: "OG Clock", Category: "clocks", FileName: "fixture.webp"}},
		{Manifest: apps.Manifest{ID: "cfl-scores", Name: "CFL Scores", Category: "sports", FileName: "fixture.webp"}},
		{Manifest: apps.Manifest{ID: "quote-of-the-day", Name: "A Quote A Day", Category: "lifestyle", FileName: "fixture.webp"}},
		{Manifest: apps.Manifest{ID: "nws-daily-forecast", Name: "NWS Daily Forecast", Category: "weather", FileName: "fixture.webp"}},
	}

	req := newAPIRequest(http.MethodGet, "/v0/catalogue?category=verified&limit=20", "test_api_key", nil)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var category struct {
		Apps                     []catalogueApp `json:"apps"`
		RepositoryRevision       string         `json:"repositoryRevision"`
		VerifiedManifestRevision string         `json:"verifiedManifestRevision"`
	}
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&category))
	assert.Len(t, category.Apps, 3)
	assert.NotEmpty(t, category.RepositoryRevision)
	assert.Equal(t, verifiedAppsRevision(), category.VerifiedManifestRevision)
	for _, item := range category.Apps {
		assert.True(t, item.Verified)
	}

	req = newAPIRequest(http.MethodGet, "/v0/catalogue?search=game&limit=20", "test_api_key", nil)
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var search struct {
		Apps []catalogueApp `json:"apps"`
	}
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&search))
	require.Len(t, search.Apps, 2)
	assert.Equal(t, "mlb-game", search.Apps[0].ID, "verified relevant results rank before unverified results")

	req = newAPIRequest(http.MethodGet, "/v0/catalogue/mlb-game", "test_api_key", nil)
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"verified":true`)
}
