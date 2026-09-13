package providers

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
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
)

//go:embed market_logo_overrides/*.svg
var marketLogoOverrides embed.FS

var approvedMarketLogos = map[string]string{
	"twelvedata:AAPL:XNAS": "apple.svg",
	"twelvedata:MSFT:XNAS": "microsoft.svg",
}

func bundledMarketLogo(listing MarketListing) string {
	name := approvedMarketLogos[listing.ID]
	if name == "" || listing.Currency != "USD" {
		return ""
	}
	body, err := marketLogoOverrides.ReadFile("market_logo_overrides/" + name)
	if err != nil {
		return ""
	}
	normalized, err := normalizeLogoAtSize(body, "image/svg+xml", 18)
	if err != nil {
		return ""
	}
	source, _, err := image.Decode(bytes.NewReader(normalized))
	if err != nil {
		return ""
	}
	canvas := image.NewRGBA(image.Rect(0, 0, 20, 20))
	draw.Draw(canvas, image.Rect(1, 1, 19, 19), source, image.Point{}, draw.Src)
	var encoded bytes.Buffer
	if png.Encode(&encoded, canvas) != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(encoded.Bytes())
}

// marketJSON is only used with adapter-owned endpoint paths. The provider
// secret never accompanies image requests or leaves the server contract.
func (adapter *TwelveDataAdapter) marketJSON(ctx context.Context, path string, query url.Values, secret string, target any) error {
	endpoint := strings.TrimRight(adapter.BaseURL, "/") + path + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return TemporarilyUnavailable()
	}
	if strings.TrimSpace(secret) == "" {
		return MissingCredential("Market data")
	}
	req.Header.Set("Authorization", "apikey "+strings.TrimSpace(secret))
	if !adapter.reserveCredits(secret, 1, false) {
		return marketQuotaError()
	}
	response, err := adapter.Client.Do(req)
	if err != nil {
		return TemporarilyUnavailable()
	}
	adapter.observeCredits(secret, response.Header)
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 512*1024+1))
	if err != nil || len(body) > 512*1024 {
		return TemporarilyUnavailable()
	}
	var status struct {
		Code    int    `json:"code"`
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &status) != nil {
		return TemporarilyUnavailable()
	}
	if err := twelveDataError(response.StatusCode, status.Code, status.Status, status.Message); err != nil {
		return err
	}
	if json.Unmarshal(body, target) != nil {
		return TemporarilyUnavailable()
	}
	return nil
}

func (adapter *TwelveDataAdapter) HydrateQuotes(ctx context.Context, request MarketRequest, quotes []MarketQuote) []MarketQuote {
	result := append([]MarketQuote(nil), quotes...)
	// One bounded acquisition window for the whole watchlist, including failures.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var group sync.WaitGroup
	for index, quote := range result {
		if quote.ErrorCode != "" {
			continue
		}
		listing, err := (MarketListing{Symbol: strings.Split(quote.Symbol, ":")[0], Exchange: quote.Exchange, MIC: quote.MIC, Currency: quote.Currency}).Normalize()
		if err != nil {
			continue
		}
		group.Add(1)
		go func(index int, listing MarketListing) {
			defer group.Done()
			key := request.ScopeType + ":" + request.ScopeID + ":" + request.CredentialID + ":" + listing.ID
			logo, _, _ := adapter.LogoCache.GetWithTTL(ctx, key, 30*24*time.Hour, func(ctx context.Context) (string, time.Duration, error) {
				if logo := bundledMarketLogo(listing); logo != "" {
					return logo, 365 * 24 * time.Hour, nil
				}
				secret, err := adapter.Credentials.Resolve(ctx, request.CredentialID, request.ScopeType, request.ScopeID)
				if err != nil {
					return "", 0, err
				}
				var payload struct {
					URL string `json:"url"`
				}
				if err := adapter.marketJSON(ctx, "/logo", listingQuery(listing), secret, &payload); err != nil {
					var classified SanitizedError
					if errors.As(err, &classified) && !classified.Retryable {
						return "", 24 * time.Hour, nil
					}
					return "", 0, err
				}
				if !allowedMarketLogoURL(payload.URL) {
					return "", 24 * time.Hour, nil
				}
				data, _, err := adapter.LogoImages.cache.Get(ctx, payload.URL, 30*24*time.Hour, 90*24*time.Hour, func(ctx context.Context) (string, error) { return adapter.LogoImages.fetch(ctx, payload.URL) })
				return data, 30 * 24 * time.Hour, err
			})
			result[index].LogoData = logo
		}(index, listing)
	}
	group.Wait()
	return result
}

type marketLogoImages struct {
	client *http.Client
	cache  *Cache[string]
}

func newMarketLogoImages(client *http.Client) *marketLogoImages {
	secured := *client
	secured.Timeout = 3 * time.Second
	secured.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !allowedMarketLogoURL(req.URL.String()) {
			return errors.New("market logo redirect rejected")
		}
		return nil
	}
	return &marketLogoImages{client: &secured, cache: NewCache[string](time.Minute)}
}
func allowedMarketLogoURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || (parsed.Port() != "" && parsed.Port() != "443") {
		return false
	}
	return parsed.Hostname() == "logo.twelvedata.com" || (parsed.Hostname() == "api.twelvedata.com" && strings.HasPrefix(parsed.Path, "/logo/"))
}
func (images *marketLogoImages) fetch(ctx context.Context, value string) (string, error) {
	if !allowedMarketLogoURL(value) {
		return "", errors.New("market logo host rejected")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, value, nil)
	if err != nil {
		return "", err
	}
	response, err := images.client.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Request == nil || !allowedMarketLogoURL(response.Request.URL.String()) {
		return "", errors.New("market logo unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, sportsLogoMaxBytes+1))
	if err != nil || len(body) > sportsLogoMaxBytes {
		return "", errors.New("invalid market logo")
	}
	data, err := normalizeSportsLogo(body, response.Header.Get("Content-Type"))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}
