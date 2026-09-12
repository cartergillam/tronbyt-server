package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	sportsLogoSize       = 16
	sportsLogoMaxBytes   = 512 * 1024
	sportsLogoFreshTTL   = 7 * 24 * time.Hour
	sportsLogoStaleTTL   = 30 * 24 * time.Hour
	sportsLogoRetryDelay = time.Minute
)

// SportsLogoCache acquires provider logos on the server, converts them to a
// compact transparent PNG and coalesces requests. A failed logo is omitted;
// sports data and rendering continue with the app's abbreviation fallback.
type SportsLogoCache struct {
	Client *http.Client
	Cache  *Cache[string]
}

func NewSportsLogoCache(client *http.Client) *SportsLogoCache {
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	securedClient := *client
	originalRedirect := client.CheckRedirect
	securedClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !allowedSportsLogoURL(request.URL.String()) {
			return errors.New("sports logo redirect host is not allowed")
		}
		if originalRedirect != nil {
			return originalRedirect(request, via)
		}
		if len(via) >= 10 {
			return errors.New("sports logo redirect limit exceeded")
		}
		return nil
	}
	return &SportsLogoCache{Client: &securedClient, Cache: NewCache[string](sportsLogoRetryDelay)}
}

func (cache *SportsLogoCache) Hydrate(ctx context.Context, snapshot SportsSnapshot) SportsSnapshot {
	result := cloneSportsSnapshot(snapshot)
	urls := sportsLogoURLs(result)
	if len(urls) == 0 {
		return result
	}

	logos := make(map[string]string, len(urls))
	var mu sync.Mutex
	var group sync.WaitGroup
	for _, logoURL := range urls {
		logoURL := logoURL
		group.Add(1)
		go func() {
			defer group.Done()
			data, _, err := cache.Cache.Get(ctx, logoURL, sportsLogoFreshTTL, sportsLogoStaleTTL, func(ctx context.Context) (string, error) {
				return cache.fetch(ctx, logoURL)
			})
			if err == nil && data != "" {
				mu.Lock()
				logos[logoURL] = data
				mu.Unlock()
			}
		}()
	}
	group.Wait()
	applySportsLogos(&result, logos)
	return result
}

func (cache *SportsLogoCache) fetch(ctx context.Context, logoURL string) (string, error) {
	if !allowedSportsLogoURL(logoURL) {
		return "", errors.New("sports logo host is not allowed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, logoURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := cache.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.Request == nil || !allowedSportsLogoURL(resp.Request.URL.String()) {
		return "", errors.New("sports logo response host is not allowed")
	}
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("sports logo response was not successful")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, sportsLogoMaxBytes+1))
	if err != nil || len(body) == 0 || len(body) > sportsLogoMaxBytes {
		return "", errors.New("sports logo response was invalid")
	}
	normalized, err := normalizeSportsLogo(body, resp.Header.Get("Content-Type"))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(normalized), nil
}

func allowedSportsLogoURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "a.espncdn.com" || strings.HasSuffix(host, ".espncdn.com") ||
		host == "assets.nhle.com" || strings.HasSuffix(host, ".nhl.com")
}

func normalizeSportsLogo(body []byte, contentType string) ([]byte, error) {
	canvas := image.NewRGBA(image.Rect(0, 0, sportsLogoSize, sportsLogoSize))
	if strings.Contains(strings.ToLower(contentType), "svg") || bytes.Contains(bytes.ToLower(body[:min(len(body), 256)]), []byte("<svg")) {
		icon, err := oksvg.ReadIconStream(bytes.NewReader(body))
		if err != nil || icon.ViewBox.W <= 0 || icon.ViewBox.H <= 0 {
			return nil, errors.New("sports SVG logo could not be decoded")
		}
		width, height := fitLogoSize(icon.ViewBox.W, icon.ViewBox.H)
		icon.SetTarget(float64((sportsLogoSize-width)/2), float64((sportsLogoSize-height)/2), float64(width), float64(height))
		scanner := rasterx.NewScannerGV(sportsLogoSize, sportsLogoSize, canvas, canvas.Bounds())
		icon.Draw(rasterx.NewDasher(sportsLogoSize, sportsLogoSize, scanner), 1)
	} else {
		source, _, err := image.Decode(bytes.NewReader(body))
		if err != nil || source.Bounds().Dx() <= 0 || source.Bounds().Dy() <= 0 {
			return nil, errors.New("sports raster logo could not be decoded")
		}
		width, height := fitLogoSize(float64(source.Bounds().Dx()), float64(source.Bounds().Dy()))
		target := image.Rect((sportsLogoSize-width)/2, (sportsLogoSize-height)/2, (sportsLogoSize-width)/2+width, (sportsLogoSize-height)/2+height)
		xdraw.CatmullRom.Scale(canvas, target, source, source.Bounds(), draw.Over, nil)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func fitLogoSize(width, height float64) (int, int) {
	if width >= height {
		return sportsLogoSize, max(1, int(height/width*sportsLogoSize))
	}
	return max(1, int(width/height*sportsLogoSize)), sportsLogoSize
}

func cloneSportsSnapshot(snapshot SportsSnapshot) SportsSnapshot {
	snapshot.Games = append([]Game(nil), snapshot.Games...)
	snapshot.UpcomingGames = append([]Game(nil), snapshot.UpcomingGames...)
	if snapshot.NextGame != nil {
		next := *snapshot.NextGame
		snapshot.NextGame = &next
	}
	return snapshot
}

func sportsLogoURLs(snapshot SportsSnapshot) []string {
	seen := map[string]bool{}
	result := []string{}
	add := func(team Team) {
		if team.ProviderLogoURL != "" && !seen[team.ProviderLogoURL] {
			seen[team.ProviderLogoURL] = true
			result = append(result, team.ProviderLogoURL)
		}
	}
	for _, game := range snapshot.Games {
		add(game.AwayTeam)
		add(game.HomeTeam)
	}
	for _, game := range snapshot.UpcomingGames {
		add(game.AwayTeam)
		add(game.HomeTeam)
	}
	if snapshot.NextGame != nil {
		add(snapshot.NextGame.AwayTeam)
		add(snapshot.NextGame.HomeTeam)
	}
	return result
}

func applySportsLogos(snapshot *SportsSnapshot, logos map[string]string) {
	apply := func(team *Team) { team.LogoData = logos[team.ProviderLogoURL] }
	for index := range snapshot.Games {
		apply(&snapshot.Games[index].AwayTeam)
		apply(&snapshot.Games[index].HomeTeam)
	}
	for index := range snapshot.UpcomingGames {
		apply(&snapshot.UpcomingGames[index].AwayTeam)
		apply(&snapshot.UpcomingGames[index].HomeTeam)
	}
	if snapshot.NextGame != nil {
		apply(&snapshot.NextGame.AwayTeam)
		apply(&snapshot.NextGame.HomeTeam)
	}
}
