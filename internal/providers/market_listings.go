package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

var marketMICPattern = regexp.MustCompile(`^[A-Z0-9]{4}$`)
var marketCurrencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// MarketListing identifies a listing, never a currency conversion. An empty
// exchange/MIC is allowed only to preserve a legacy unqualified symbol.
type MarketListing struct {
	ID           string `json:"id"`
	Symbol       string `json:"symbol"`
	Name         string `json:"name"`
	Exchange     string `json:"exchange"`
	MIC          string `json:"mic"`
	Currency     string `json:"currency"`
	Country      string `json:"country"`
	RequiredPlan string `json:"requiredPlan,omitempty"`
}

func (listing MarketListing) Normalize() (MarketListing, error) {
	symbol, err := NormalizeMarketSymbol(listing.Symbol)
	if err != nil {
		return listing, err
	}
	parts := strings.SplitN(symbol, ":", 2)
	listing.Symbol = parts[0]
	if len(parts) == 2 {
		if listing.Exchange != "" && !strings.EqualFold(listing.Exchange, parts[1]) {
			return listing, errors.New("conflicting listing exchange")
		}
		listing.Exchange = parts[1]
	}
	listing.Exchange = strings.ToUpper(strings.TrimSpace(listing.Exchange))
	listing.MIC = strings.ToUpper(strings.TrimSpace(listing.MIC))
	listing.Currency = strings.ToUpper(strings.TrimSpace(listing.Currency))
	for _, value := range []string{listing.Exchange, listing.MIC, listing.Currency, listing.Country, listing.Name, listing.RequiredPlan} {
		if len(value) > 160 || strings.ContainsFunc(value, unicode.IsControl) {
			return listing, errors.New("invalid listing metadata")
		}
	}
	if listing.MIC != "" && !marketMICPattern.MatchString(listing.MIC) {
		return listing, errors.New("invalid listing MIC")
	}
	if listing.Currency != "" && !marketCurrencyPattern.MatchString(listing.Currency) {
		return listing, errors.New("invalid listing currency")
	}
	venue := listing.MIC
	if venue == "" {
		venue = listing.Exchange
		if mic := map[string]string{"TSX": "XTSE", "TORONTO STOCK EXCHANGE": "XTSE", "NASDAQ": "XNAS", "NYSE": "XNYS"}[venue]; mic != "" {
			venue = mic
		}
	}
	if venue == "" {
		venue = "AUTO"
	}
	listing.ID = "twelvedata:" + listing.Symbol + ":" + venue
	return listing, nil
}

// ParseMarketWatchlist uses an additive JSON string field so existing Pixlet
// schemas, saved symbol strings and older config clients remain compatible.
func ParseMarketWatchlist(raw string) ([]MarketListing, error) {
	if len(raw) > 8192 {
		return nil, errors.New("watchlist is too large")
	}
	var listings []MarketListing
	if err := json.Unmarshal([]byte(raw), &listings); err != nil {
		return nil, errors.New("watchlist must contain listings")
	}
	if len(listings) < 1 || len(listings) > MaxMarketSymbols {
		return nil, errors.New("choose between 1 and 5 stocks")
	}
	seen := map[string]bool{}
	for i, listing := range listings {
		value, err := listing.Normalize()
		if err != nil {
			return nil, err
		}
		if seen[value.ID] {
			return nil, errors.New("this listing is already in the watchlist")
		}
		seen[value.ID] = true
		listings[i] = value
	}
	return listings, nil
}

type MarketSearchRequest struct{ Query, CredentialID, ScopeType, ScopeID string }
type MarketSearcher interface {
	Search(context.Context, MarketSearchRequest) ([]MarketListing, error)
}
type MarketLogoHydrator interface {
	HydrateQuotes(context.Context, MarketRequest, []MarketQuote) []MarketQuote
}

func (adapter *TwelveDataAdapter) Search(ctx context.Context, request MarketSearchRequest) ([]MarketListing, error) {
	query := strings.TrimSpace(request.Query)
	if len([]rune(query)) < 2 || len(query) > 80 || strings.ContainsFunc(query, unicode.IsControl) {
		return nil, SanitizedError{Code: "invalid_search", Message: "Enter between 2 and 80 characters", Retryable: false}
	}
	key := request.ScopeType + ":" + request.ScopeID + ":" + request.CredentialID
	result, _, err := adapter.SearchCache.Get(ctx, key+":"+strings.ToUpper(query), marketSearchTTL, 7*24*time.Hour, func(ctx context.Context) ([]MarketListing, error) {
		if adapter.rateLimited(key) {
			return nil, marketQuotaError()
		}
		adapter.searchMu.Lock()
		last := adapter.searchLast[key]
		if time.Since(last) < time.Second {
			adapter.searchMu.Unlock()
			return nil, SanitizedError{Code: "provider_rate_limited", Message: "Wait a moment before searching again", Retryable: true}
		}
		adapter.searchLast[key] = time.Now()
		adapter.searchMu.Unlock()
		secret, err := adapter.Credentials.Resolve(ctx, request.CredentialID, request.ScopeType, request.ScopeID)
		if err != nil {
			return nil, MissingCredential("Market data")
		}
		var payload struct {
			Data []struct {
				Symbol   string `json:"symbol"`
				Name     string `json:"instrument_name"`
				Exchange string `json:"exchange"`
				MIC      string `json:"mic_code"`
				Currency string `json:"currency"`
				Country  string `json:"country"`
				Access   struct {
					Plan string `json:"plan"`
				} `json:"access"`
			} `json:"data"`
		}
		err = adapter.marketJSON(ctx, "/symbol_search", url.Values{"symbol": {query}, "outputsize": {"30"}, "show_plan": {"true"}}, secret, &payload)
		if err != nil {
			adapter.recordBackoff(key, err)
			return nil, err
		}
		items := []MarketListing{}
		seen := map[string]bool{}
		for _, item := range payload.Data {
			listing, err := (MarketListing{Symbol: item.Symbol, Name: item.Name, Exchange: item.Exchange, MIC: item.MIC, Currency: item.Currency, Country: item.Country, RequiredPlan: item.Access.Plan}).Normalize()
			if err != nil || listing.Currency == "" || listing.Exchange == "" || seen[listing.ID] {
				continue
			}
			seen[listing.ID] = true
			items = append(items, listing)
		}
		return items, nil
	})
	return append([]MarketListing{}, result...), err
}
