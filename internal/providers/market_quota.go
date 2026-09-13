package providers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const marketSearchTTL = 24 * time.Hour

// Counters contain no listing, user, credential, or request URL information.
type MarketDiagnostics struct {
	ProviderRequests, SymbolsRefreshed, EstimatedQuoteCredits, CacheHits, CacheMisses, QuotaDefers uint64
	LastBatchSize                                                                                  int
}
type marketCreditReservation struct {
	At   time.Time
	Cost int
}
type marketUsage struct {
	Reservations                    []marketCreditReservation
	MinuteUntil                     time.Time
	Remaining                       int
	ObservedAt                      time.Time
	ObservedLeft, ObservedLimit     int
	Day                             string
	LocalCredits, LocalQuoteCredits int // Only this process's usage, never claimed as provider daily remaining.
}

type marketCreditDeferred struct{ SanitizedError }

func (e marketCreditDeferred) Unwrap() error { return e.SanitizedError }
func marketQuotaError() error {
	return marketCreditDeferred{SanitizedError{Code: "provider_rate_limited", Message: "Market data is temporarily rate limited", Retryable: true}}
}
func (a *TwelveDataAdapter) Diagnostics() MarketDiagnostics {
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	return a.diagnostics
}
func (a *TwelveDataAdapter) reserveCredits(secret string, cost int, quote bool) bool {
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	key := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	u := a.usage[key]
	now := time.Now()
	if !now.Before(u.MinuteUntil) {
		u.Remaining = 8
		u.MinuteUntil = now.Add(time.Minute)
	}
	spent := 0
	reservations := u.Reservations[:0]
	for _, r := range u.Reservations {
		if now.Sub(r.At) < time.Minute {
			spent += r.Cost
			reservations = append(reservations, r)
		}
	}
	u.Reservations = reservations
	day := now.UTC().Format("2006-01-02")
	if u.Day != day {
		u.Day = day
		u.LocalCredits = 0
		u.LocalQuoteCredits = 0
	}
	// Leave room for search, validation, errors and other users of the key.
	if cost > min(u.Remaining, 8-spent) || u.LocalCredits+cost > 750 || (quote && u.LocalQuoteCredits+cost > 700) {
		a.usage[key] = u
		a.diagnostics.QuotaDefers++
		return false
	}
	u.Reservations = append(u.Reservations, marketCreditReservation{At: now, Cost: cost})
	u.Remaining -= cost
	u.LocalCredits += cost
	a.diagnostics.ProviderRequests++
	if quote {
		u.LocalQuoteCredits += cost
		a.diagnostics.EstimatedQuoteCredits += uint64(cost)
		a.diagnostics.LastBatchSize = cost
	}
	a.usage[key] = u
	return true
}
func (a *TwelveDataAdapter) observeCredits(secret string, headers http.Header) {
	left, err := strconv.Atoi(headers.Get("api-credits-left"))
	if err != nil || left < 0 || left > 1000000 {
		return
	}
	used, usedErr := strconv.Atoi(headers.Get("api-credits-used"))
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	key := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	u := a.usage[key]
	u.ObservedAt = time.Now()
	u.ObservedLeft = left
	// Never raise locally reserved headroom: concurrent requests may still be in flight.
	u.Remaining = min(u.Remaining, left)
	if usedErr == nil && used >= 0 && used <= 1000000 {
		u.ObservedLimit = left + used
	}
	a.usage[key] = u
}
func (a *TwelveDataAdapter) quoteBody(ctx context.Context, query url.Values, secret string, cost int) ([]byte, int, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, 0, MissingCredential("Market data")
	}
	if !a.reserveCredits(secret, cost, true) {
		return nil, 0, marketQuotaError()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.BaseURL, "/")+"/quote?"+query.Encode(), nil)
	if err != nil {
		return nil, 0, TemporarilyUnavailable()
	}
	req.Header.Set("Authorization", "apikey "+strings.TrimSpace(secret))
	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, 0, TemporarilyUnavailable()
	}
	defer resp.Body.Close()
	a.observeCredits(secret, resp.Header)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024+1))
	if err != nil || len(body) > 512*1024 {
		return nil, 0, SanitizedError{Code: "provider_response_invalid", Message: "Market data could not be read", Retryable: true}
	}
	return body, resp.StatusCode, nil
}

type quoteInput struct {
	key, symbol string
	listing     *MarketListing
	query       url.Values
}

func (a *TwelveDataAdapter) listingQuotes(ctx context.Context, r MarketRequest) ([]MarketQuote, error) {
	// This mutex also prevents two overlapping watchlists from independently
	// refreshing the same expired listing. No render mode/device ID enters keys.
	a.quoteMu.Lock()
	defer a.quoteMu.Unlock()
	scope, scopeErr := a.credentialCacheScope(ctx, r.CredentialID, r.ScopeType, r.ScopeID)
	if scopeErr != nil {
		return nil, MissingCredential("Market data")
	}
	inputs := []quoteInput{}
	if r.Listings != nil {
		raw, _ := json.Marshal(r.Listings)
		listings, err := ParseMarketWatchlist(string(raw))
		if err != nil {
			return nil, err
		}
		for i := range listings {
			l := listings[i]
			inputs = append(inputs, quoteInput{key: scope + ":" + l.ID + ":" + l.Currency, symbol: l.Symbol, listing: &l, query: listingQuery(l)})
		}
	} else {
		for _, raw := range r.Symbols {
			symbol, _ := NormalizeMarketSymbol(raw)
			inputs = append(inputs, quoteInput{key: scope + ":" + symbol, symbol: symbol, query: url.Values{"symbol": {symbol}}})
		}
	}
	now := time.Now()
	groups := map[string][]int{}
	groupOrder := []string{}
	for i, in := range inputs {
		a.Cache.mu.Lock()
		entry, found := a.Cache.entries[in.key]
		a.Cache.mu.Unlock()
		if found && now.Before(entry.freshUntil) {
			a.usageMu.Lock()
			a.diagnostics.CacheHits++
			a.usageMu.Unlock()
			continue
		}
		if a.recentFailure(in.key) != nil {
			continue
		}
		q := url.Values{}
		for k, v := range in.query {
			if k != "symbol" {
				q[k] = v
			}
		}
		// Legacy exchange-qualified inputs use a separate group so AUTO and venue
		// selection never silently become interchangeable.
		groupKey := q.Encode()
		if in.listing == nil && strings.Contains(in.symbol, ":") {
			groupKey += "|" + strings.SplitN(in.symbol, ":", 2)[1]
		}
		if _, ok := groups[groupKey]; !ok {
			groupOrder = append(groupOrder, groupKey)
		}
		groups[groupKey] = append(groups[groupKey], i)
		a.usageMu.Lock()
		a.diagnostics.CacheMisses++
		a.usageMu.Unlock()
	}
	// A Basic key cannot pay for ten simultaneous symbols. Five-symbol groups
	// leave minute headroom and defer the second group until capacity resets.
	splitGroups := map[string][]int{}
	splitOrder := []string{}
	for _, key := range groupOrder {
		indices := groups[key]
		for start := 0; start < len(indices); start += 5 {
			name := key + "#" + strconv.Itoa(start)
			splitOrder = append(splitOrder, name)
			splitGroups[name] = indices[start:min(start+5, len(indices))]
		}
	}
	groups, groupOrder = splitGroups, splitOrder
	fetched := map[string]MarketQuote{}
	failures := map[string]error{}
	for _, group := range groupOrder {
		indices := groups[group]
		var err error
		if a.rateLimited(scope) {
			err = marketQuotaError()
		}
		secret := ""
		if err == nil {
			secret, err = a.Credentials.Resolve(ctx, r.CredentialID, r.ScopeType, r.ScopeID)
			if err != nil {
				err = MissingCredential("Market data")
			}
		}
		var body []byte
		var status int
		if err == nil {
			query := inputs[indices[0]].query
			symbols := []string{}
			for _, i := range indices {
				symbols = append(symbols, inputs[i].symbol)
			}
			query.Set("symbol", strings.Join(symbols, ","))
			body, status, err = a.quoteBody(ctx, query, secret, len(indices))
		}
		var batch map[string]json.RawMessage
		if err == nil && len(indices) > 1 {
			if json.Unmarshal(body, &batch) != nil {
				err = SanitizedError{Code: "provider_response_invalid", Message: "Market data could not be read", Retryable: true}
			}
		}
		for _, i := range indices {
			in := inputs[i]
			symbolErr := err
			var quote MarketQuote
			if symbolErr == nil {
				data := body
				if len(indices) > 1 {
					data = batch[in.symbol]
					if len(data) == 0 {
						data = body
					}
				}
				quote, symbolErr = decodeMarketQuote(data, status, in.symbol)
				if symbolErr == nil && in.listing != nil {
					l := in.listing
					if !strings.EqualFold(strings.Split(quote.Symbol, ":")[0], l.Symbol) || (l.MIC != "" && !strings.EqualFold(l.MIC, quote.MIC)) || (l.MIC == "" && l.Exchange != "" && !strings.EqualFold(l.Exchange, quote.Exchange)) || (l.Currency != "" && !strings.EqualFold(l.Currency, quote.Currency)) {
						symbolErr = SanitizedError{Code: "listing_mismatch", Message: "The provider returned a different listing", Retryable: false}
					} else {
						quote.ListingID = l.ID
					}
				}
			}
			if symbolErr != nil {
				failures[in.key] = symbolErr
				a.recordFailure(in.key, symbolErr)
				if err == nil {
					a.recordBackoff(scope, symbolErr)
				}
			} else {
				fetched[in.key] = quote
				a.clearFailure(in.key)
				a.usageMu.Lock()
				a.diagnostics.SymbolsRefreshed++
				a.usageMu.Unlock()
			}
		}
	}
	result := make([]MarketQuote, 0, len(inputs))
	var firstErr error
	for _, in := range inputs {
		quotes, stale, err := a.Cache.GetWithTTL(ctx, in.key, marketStaleTTL, func(context.Context) ([]MarketQuote, time.Duration, error) {
			if q, ok := fetched[in.key]; ok {
				ttl := marketQuoteTTL(q, time.Now())
				// For >8 active listings, a modest freshness relaxation avoids
				// spending 780 credits just on an ordinary trading session.
				if len(inputs) > 8 && q.MarketStatus == MarketOpen {
					ttl = max(ttl, 6*time.Minute)
				}
				return []MarketQuote{q}, ttl, nil
			}
			if err := failures[in.key]; err != nil {
				return nil, 0, err
			}
			if err := a.recentFailure(in.key); err != nil {
				return nil, 0, err
			}
			return nil, 0, marketQuotaError()
		})
		if err != nil {
			if classified := a.recentFailure(in.key); classified != nil {
				err = classified
			}
			if firstErr == nil {
				firstErr = err
			}
			code := "provider_unavailable"
			var classified SanitizedError
			if errors.As(err, &classified) {
				code = classified.Code
			}
			q := MarketQuote{Symbol: in.symbol, ErrorCode: code, MarketStatus: MarketUnknown}
			if in.listing != nil {
				l := in.listing
				q.ListingID = l.ID
				q.Exchange = l.Exchange
				q.MIC = l.MIC
				q.Currency = l.Currency
				q.DisplayName = l.Name
			}
			result = append(result, q)
		} else {
			q := quotes[0]
			q.Stale = stale
			result = append(result, q)
		}
	}
	// Preserve the existing single-symbol error contract, including INVALID KEY.
	if len(result) == 1 && result[0].ErrorCode != "" && inputs[0].listing == nil {
		return nil, firstErr
	}
	return result, nil
}

// Stores may version cache scopes without decrypting the credential. Test and
// legacy resolvers retain their existing owner + credential identity.
func (a *TwelveDataAdapter) credentialCacheScope(ctx context.Context, id, scopeType, scopeID string) (string, error) {
	if resolver, ok := a.Credentials.(interface {
		CredentialCacheScope(context.Context, string, string, string) (string, error)
	}); ok {
		return resolver.CredentialCacheScope(ctx, id, scopeType, scopeID)
	}
	return scopeType + ":" + scopeID + ":" + id, nil
}

// CachedQuoteRevision fingerprints display data without permitting a provider
// call. Render output may expire independently of quote/provider caches.
func (a *TwelveDataAdapter) CachedQuoteRevision(ctx context.Context, r MarketRequest) string {
	scope, err := a.credentialCacheScope(ctx, r.CredentialID, r.ScopeType, r.ScopeID)
	if err != nil {
		return "unconfigured"
	}
	keys := []string{}
	if r.Listings != nil {
		for _, listing := range r.Listings {
			l, err := listing.Normalize()
			if err != nil {
				return "invalid"
			}
			keys = append(keys, scope+":"+l.ID+":"+l.Currency)
		}
	} else {
		for _, raw := range r.Symbols {
			symbol, err := NormalizeMarketSymbol(raw)
			if err != nil {
				return "invalid"
			}
			keys = append(keys, scope+":"+symbol)
		}
	}
	quotes := []MarketQuote{}
	a.Cache.mu.Lock()
	for _, key := range keys {
		if entry, ok := a.Cache.entries[key]; ok && len(entry.value) > 0 {
			quote := entry.value[0]
			quote.Stale = time.Now().After(entry.freshUntil)
			quotes = append(quotes, quote)
		}
	}
	a.Cache.mu.Unlock()
	data, _ := json.Marshal(quotes)
	return strconv.FormatUint(uint64(len(quotes)), 10) + ":" + fmt.Sprintf("%x", sha256.Sum256(data))
}

func marketQuoteTTL(q MarketQuote, now time.Time) time.Duration {
	ttl := quoteFreshTTL([]MarketQuote{q})
	// Small weekday/session bound for known North American venues. This is not
	// a holiday calendar: opening time merely allows a refresh to discover state.
	if q.MIC != "XNAS" && q.MIC != "XNGS" && q.MIC != "XNMS" && q.MIC != "XNCM" && q.MIC != "XNYS" && q.MIC != "ARCX" && q.MIC != "XTSE" {
		return ttl
	}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return ttl
	}
	local := now.In(loc)
	opening := time.Date(local.Year(), local.Month(), local.Day(), 9, 30, 0, 0, loc)
	closing := time.Date(local.Year(), local.Month(), local.Day(), 16, 0, 0, 0, loc)
	if local.Weekday() != time.Saturday && local.Weekday() != time.Sunday && local.Before(opening) {
		return min(ttl, opening.Sub(local))
	}
	lastClose := closing
	if local.Before(closing) || local.Weekday() == time.Saturday || local.Weekday() == time.Sunday {
		lastClose = lastClose.AddDate(0, 0, -1)
		for lastClose.Weekday() == time.Saturday || lastClose.Weekday() == time.Sunday {
			lastClose = lastClose.AddDate(0, 0, -1)
		}
	}
	if q.MarketStatus == MarketClosed && !q.QuoteTimestamp.Before(lastClose) && (local.Weekday() == time.Saturday || local.Weekday() == time.Sunday || !local.Before(closing)) {
		next := opening.AddDate(0, 0, 1)
		for next.Weekday() == time.Saturday || next.Weekday() == time.Sunday {
			next = next.AddDate(0, 0, 1)
		}
		return next.Sub(local)
	}
	return ttl
}
