package providers

import (
	"context"
	"errors"
	"strings"
)

// LocalYahooMarketRouter routes only the explicitly matched PLZ.UN/TSX/CAD
// listing to Yahoo. All other symbols retain the existing Twelve Data path.
// It is constructed only when both local-development and experimental flags
// are set and PRODUCTION is false.
type LocalYahooMarketRouter struct {
	TwelveData *TwelveDataAdapter
	Yahoo      *YahooFinanceAdapter
}

func NewLocalYahooMarketRouter(twelveData *TwelveDataAdapter, yahoo *YahooFinanceAdapter) *LocalYahooMarketRouter {
	return &LocalYahooMarketRouter{TwelveData: twelveData, Yahoo: yahoo}
}

func (r *LocalYahooMarketRouter) Quotes(ctx context.Context, request MarketRequest) ([]MarketQuote, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	type target struct {
		listing *MarketListing
		symbol  string
	}
	targets := []target{}
	for i := range request.Listings {
		listing := request.Listings[i]
		targets = append(targets, target{listing: &listing, symbol: listing.Symbol})
	}
	if request.Listings == nil {
		for _, symbol := range request.Symbols {
			targets = append(targets, target{symbol: symbol})
		}
	}
	out := make([]MarketQuote, len(targets))
	var twelveRequest MarketRequest
	var twelveIndexes []int
	var yahooIndexes []int
	for i, item := range targets {
		if item.listing != nil && strings.EqualFold(item.symbol, yahooDisplay) && isYahooPLZListing(*item.listing) {
			yahooIndexes = append(yahooIndexes, i)
			continue
		}
		twelveIndexes = append(twelveIndexes, i)
		if item.listing != nil {
			twelveRequest.Listings = append(twelveRequest.Listings, *item.listing)
		} else {
			twelveRequest.Symbols = append(twelveRequest.Symbols, item.symbol)
		}
	}
	if len(twelveIndexes) > 0 {
		twelveRequest.CredentialID, twelveRequest.ScopeType, twelveRequest.ScopeID = request.CredentialID, request.ScopeType, request.ScopeID
		twelveRequest.MaxSymbols = request.MaxSymbols
		quotes, err := r.TwelveData.Quotes(ctx, twelveRequest)
		if err != nil {
			if len(yahooIndexes) == 0 {
				return nil, err
			}
			for _, index := range twelveIndexes {
				out[index] = MarketQuote{Symbol: targets[index].symbol, ErrorCode: yahooErrorCode(err)}
			}
		} else {
			for i, index := range twelveIndexes {
				if i < len(quotes) {
					out[index] = quotes[i]
				}
			}
		}
	}
	for _, index := range yahooIndexes {
		item := targets[index]
		listing := MarketListing{Symbol: yahooDisplay}
		yahooRequest := MarketRequest{Symbols: []string{yahooDisplay}}
		if item.listing != nil {
			listing = *item.listing
			yahooRequest = MarketRequest{Listings: []MarketListing{listing}}
		}
		quotes, err := r.Yahoo.Quotes(ctx, yahooRequest)
		if err != nil {
			out[index] = MarketQuote{Symbol: yahooDisplay, DisplayName: "Plaza Retail REIT", Currency: "CAD", Exchange: "Toronto Stock Exchange", MIC: "XTSE", Delayed: true, ErrorCode: yahooErrorCode(err)}
			continue
		}
		if len(quotes) > 0 {
			out[index] = quotes[0]
			out[index].ListingID = listing.ID
		}
	}
	return out, nil
}

func yahooErrorCode(err error) string {
	classified, ok := errors.AsType[SanitizedError](err)
	if ok {
		return classified.Code
	}
	return "provider_error"
}

func (r *LocalYahooMarketRouter) QuoteDiagnostics(ctx context.Context, request MarketRequest) ([]MarketQuoteDiagnostic, error) {
	var result []MarketQuoteDiagnostic
	if request.Listings != nil {
		var td, yahoo []MarketListing
		for _, listing := range request.Listings {
			if strings.EqualFold(listing.Symbol, yahooDisplay) && isYahooPLZListing(listing) {
				yahoo = append(yahoo, listing)
			} else {
				td = append(td, listing)
			}
		}
		if len(yahoo) > 0 {
			rows, err := r.Yahoo.QuoteDiagnostics(ctx, MarketRequest{Listings: yahoo})
			if err != nil {
				return nil, err
			}
			result = append(result, rows...)
		}
		if len(td) > 0 {
			rows, err := r.TwelveData.QuoteDiagnostics(ctx, MarketRequest{Listings: td, CredentialID: request.CredentialID, ScopeType: request.ScopeType, ScopeID: request.ScopeID})
			if err != nil {
				return nil, err
			}
			result = append(result, rows...)
		}
		return result, nil
	}
	td := append([]string(nil), request.Symbols...)
	if len(td) > 0 {
		rows, err := r.TwelveData.QuoteDiagnostics(ctx, MarketRequest{Symbols: td, CredentialID: request.CredentialID, ScopeType: request.ScopeType, ScopeID: request.ScopeID})
		if err != nil {
			return nil, err
		}
		result = append(result, rows...)
	}
	return result, nil
}

func (r *LocalYahooMarketRouter) HydrateQuotes(ctx context.Context, request MarketRequest, quotes []MarketQuote) []MarketQuote {
	return r.TwelveData.HydrateQuotes(ctx, request, quotes)
}

func (r *LocalYahooMarketRouter) Search(ctx context.Context, request MarketSearchRequest) ([]MarketListing, error) {
	return r.TwelveData.Search(ctx, request)
}

func (r *LocalYahooMarketRouter) ValidateCredential(ctx context.Context, id, scopeType, scopeID string) error {
	return r.TwelveData.ValidateCredential(ctx, id, scopeType, scopeID)
}

func (r *LocalYahooMarketRouter) CredentialQuotaDiagnostics(ctx context.Context, id, scopeType, scopeID string) (MarketQuotaSnapshot, MarketQuotaPolicy, error) {
	return r.TwelveData.CredentialQuotaDiagnostics(ctx, id, scopeType, scopeID)
}
