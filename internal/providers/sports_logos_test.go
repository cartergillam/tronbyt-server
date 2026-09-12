package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSportsLogoCacheNormalizesCachesAndHydratesEverySnapshotLocation(t *testing.T) {
	var source bytes.Buffer
	pixels := image.NewRGBA(image.Rect(0, 0, 24, 12))
	for y := 0; y < 12; y++ {
		for x := 0; x < 24; x++ {
			pixels.Set(x, y, color.RGBA{R: 220, A: 255})
		}
	}
	require.NoError(t, png.Encode(&source, pixels))

	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		assert.Equal(t, "a.espncdn.com", request.URL.Hostname())
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(bytes.NewReader(source.Bytes())),
			Request:    request,
		}, nil
	})}
	cache := NewSportsLogoCache(client)
	team := Team{ProviderID: "1", Abbreviation: "ABC", ProviderLogoURL: "https://a.espncdn.com/i/teamlogos/nba/500/abc.png"}
	game := Game{AwayTeam: team, HomeTeam: team}
	snapshot := SportsSnapshot{Games: []Game{game}, UpcomingGames: []Game{game}, NextGame: &game}

	first := cache.Hydrate(t.Context(), snapshot)
	second := cache.Hydrate(t.Context(), snapshot)
	require.NotEmpty(t, first.Games[0].AwayTeam.LogoData)
	assert.Equal(t, first.Games[0].AwayTeam.LogoData, first.NextGame.HomeTeam.LogoData)
	assert.Equal(t, first.Games[0].AwayTeam.LogoData, second.Games[0].AwayTeam.LogoData)
	assert.Empty(t, snapshot.Games[0].AwayTeam.LogoData, "hydration must not mutate a provider-cached snapshot")
	assert.Equal(t, int32(1), calls.Load(), "one provider image is coalesced and reused")

	decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewBufferString(first.Games[0].AwayTeam.LogoData)))
	require.NoError(t, err)
	imageValue, format, err := image.Decode(bytes.NewReader(decoded))
	require.NoError(t, err)
	assert.Equal(t, "png", format)
	assert.Equal(t, image.Rect(0, 0, 16, 16), imageValue.Bounds())
	_, _, _, cornerAlpha := imageValue.At(0, 0).RGBA()
	_, _, _, centerAlpha := imageValue.At(8, 8).RGBA()
	assert.Zero(t, cornerAlpha, "aspect-fit padding remains transparent")
	assert.NotZero(t, centerAlpha, "logo pixels remain visible after normalization")
}

func TestSportsLogoCacheRasterizesSVGAndFallsBackWithoutDamagingGame(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/bad.svg" {
			return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil)), Request: request}, nil
		}
		svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 10"><rect width="20" height="10" fill="#0055aa"/></svg>`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"image/svg+xml"}}, Body: io.NopCloser(bytes.NewBufferString(svg)), Request: request}, nil
	})}
	cache := NewSportsLogoCache(client)
	snapshot := SportsSnapshot{Games: []Game{{
		ID:       "game-1",
		AwayTeam: Team{Abbreviation: "TOR", ProviderLogoURL: "https://assets.nhle.com/good.svg"},
		HomeTeam: Team{Abbreviation: "BOS", ProviderLogoURL: "https://assets.nhle.com/bad.svg"},
	}}}
	result := cache.Hydrate(context.Background(), snapshot)
	assert.NotEmpty(t, result.Games[0].AwayTeam.LogoData)
	assert.Empty(t, result.Games[0].HomeTeam.LogoData)
	assert.Equal(t, "BOS", result.Games[0].HomeTeam.Abbreviation, "fallback identity survives a logo failure")
	assert.Equal(t, GameID("game-1"), result.Games[0].ID)
}

func TestSportsLogoURLAllowlistRejectsUntrustedAndInsecureHosts(t *testing.T) {
	assert.True(t, allowedSportsLogoURL("https://a.espncdn.com/i/teamlogos/nfl/500/buf.png"))
	assert.True(t, allowedSportsLogoURL("https://assets.nhle.com/logos/nhl/svg/TOR_light.svg"))
	assert.False(t, allowedSportsLogoURL("http://a.espncdn.com/logo.png"))
	assert.False(t, allowedSportsLogoURL("https://espncdn.com.example.test/logo.png"))
	assert.False(t, allowedSportsLogoURL("https://127.0.0.1/logo.png"))
	cache := NewSportsLogoCache(&http.Client{})
	allowed, err := http.NewRequest(http.MethodGet, "https://a.espncdn.com/logo.png", nil)
	require.NoError(t, err)
	assert.NoError(t, cache.Client.CheckRedirect(allowed, nil))
	untrusted, err := http.NewRequest(http.MethodGet, "https://example.test/logo.png", nil)
	require.NoError(t, err)
	assert.Error(t, cache.Client.CheckRedirect(untrusted, nil))
}
