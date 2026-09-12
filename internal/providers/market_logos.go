package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

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
	response, err := adapter.Client.Do(req)
	if err != nil {
		return TemporarilyUnavailable()
	}
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
				data, _, err := adapter.LogoImages.cache.Get(ctx, payload.URL, 7*24*time.Hour, 30*24*time.Hour, func(ctx context.Context) (string, error) { return adapter.LogoImages.fetch(ctx, payload.URL) })
				return data, 7 * 24 * time.Hour, err
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
