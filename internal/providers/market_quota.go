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

// MarketDiagnostics contains no listing, user, credential, or request URL information.
type MarketDiagnostics struct {
	ProviderRequests, SymbolsRefreshed, EstimatedQuoteCredits, CacheHits, CacheMisses, QuotaDefers uint64
	LastBatchSize                                                                                  int
}
type marketUsage struct {
	MinuteBucket                              int64
	MinuteUsed                                int
	ObservedAt                                time.Time
	ObservedLeft, ObservedLimit, ObservedUsed int
	Day                                       string
	LocalCredits                              int
	DaySymbols                                map[string]bool
	Blocked                                   uint64
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

func (a *TwelveDataAdapter) CredentialQuotaDiagnostics(ctx context.Context, credentialID, scopeType, scopeID string) (MarketQuotaSnapshot, MarketQuotaPolicy, error) {
	secret, err := a.Credentials.Resolve(ctx, credentialID, scopeType, scopeID)
	if err != nil {
		return MarketQuotaSnapshot{}, a.quotaPolicy, MissingCredential("Market data")
	}
	hash := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	key := fmt.Sprintf("%x", hash[:])
	if a.quotaStoreRequired && a.quotaStore == nil {
		return MarketQuotaSnapshot{}, a.quotaPolicy, errors.New("quota ledger unavailable")
	}
	if a.quotaStore != nil {
		snapshot, err := a.quotaStore.ReadMarketQuota(ctx, key, a.quotaPolicy, time.Now())
		return snapshot, a.quotaPolicy, err
	}
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	u := a.usage[hash]
	now := time.Now()
	minute, day := now.UTC().Truncate(time.Minute).Unix(), now.UTC().Format("2006-01-02")
	snapshot := MarketQuotaSnapshot{DailyCreditsRemaining: a.quotaPolicy.DailyCredits, MinuteCreditsRemaining: a.quotaPolicy.MinuteCredits, BlockedRequests: u.Blocked}
	if u.MinuteBucket == minute {
		snapshot.MinuteCreditsUsed = u.MinuteUsed
		snapshot.MinuteCreditsRemaining = max(0, a.quotaPolicy.MinuteCredits-u.MinuteUsed)
		if u.ObservedAt.UTC().Truncate(time.Minute).Unix() == minute {
			snapshot.ObservedCreditsLeft, snapshot.ProviderCreditsUsed, snapshot.ObservedAt = u.ObservedLeft, u.ObservedUsed, u.ObservedAt
			snapshot.MinuteCreditsRemaining = min(snapshot.MinuteCreditsRemaining, u.ObservedLeft)
		}
	}
	if u.Day == day {
		snapshot.DailyCreditsUsed = u.LocalCredits
		snapshot.DailyCreditsRemaining = max(0, a.quotaPolicy.DailyCredits-u.LocalCredits)
		snapshot.NewSymbolsToday = len(u.DaySymbols)
	}
	if snapshot.MinuteCreditsRemaining == 0 {
		snapshot.NextEligibleAt = now.UTC().Truncate(time.Minute).Add(time.Minute)
	}
	if snapshot.DailyCreditsRemaining == 0 || snapshot.NewSymbolsToday >= a.quotaPolicy.NewSymbolsPerDay {
		snapshot.NextEligibleAt = time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day()+1, 0, 0, 0, 0, time.UTC)
	}
	return snapshot, a.quotaPolicy, nil
}
func (a *TwelveDataAdapter) reserveCredits(secret string, cost int, quote bool) bool {
	return a.reserveCreditsForSymbols(context.Background(), secret, cost, quote, nil)
}

func (a *TwelveDataAdapter) reserveCreditsForSymbols(ctx context.Context, secret string, cost int, quote bool, symbols []string) bool {
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	key := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	now := time.Now()
	hash := fmt.Sprintf("%x", key[:])
	if a.quotaStoreRequired && a.quotaStore == nil {
		a.diagnostics.QuotaDefers++
		return false
	}
	if a.quotaStore != nil {
		snapshot, allowed, err := a.quotaStore.ReserveMarketCredits(ctx, hash, cost, symbols, a.quotaPolicy, now)
		if err != nil || !allowed {
			a.diagnostics.QuotaDefers++
			return false
		}
		_ = snapshot
	} else {
		u := a.usage[key]
		minute := now.UTC().Truncate(time.Minute).Unix()
		day := now.UTC().Format("2006-01-02")
		if u.MinuteBucket != minute {
			u.MinuteBucket, u.MinuteUsed, u.ObservedLeft = minute, 0, 0
		}
		if u.Day != day {
			u.Day, u.LocalCredits, u.DaySymbols = day, 0, make(map[string]bool)
		}
		newSymbols := 0
		for _, symbol := range symbols {
			if !u.DaySymbols[symbol] {
				newSymbols++
			}
		}
		minuteCap := a.quotaPolicy.MinuteCredits
		if u.ObservedAt.UTC().Truncate(time.Minute).Unix() == minute && u.ObservedLeft < minuteCap {
			minuteCap = u.ObservedLeft
		}
		if u.MinuteUsed+cost > minuteCap || u.LocalCredits+cost > a.quotaPolicy.DailyCredits || len(u.DaySymbols)+newSymbols > a.quotaPolicy.NewSymbolsPerDay {
			u.Blocked++
			a.usage[key] = u
			a.diagnostics.QuotaDefers++
			return false
		}
		u.MinuteUsed += cost
		u.LocalCredits += cost
		for _, symbol := range symbols {
			u.DaySymbols[symbol] = true
		}
		a.usage[key] = u
	}
	a.diagnostics.ProviderRequests++
	if quote {
		a.diagnostics.EstimatedQuoteCredits += uint64(cost)
		a.diagnostics.LastBatchSize = cost
	}
	return true
}
func (a *TwelveDataAdapter) observeCredits(secret string, headers http.Header) {
	left, err := strconv.Atoi(headers.Get("api-credits-left"))
	if err != nil || left < 0 || left > 1000000 {
		return
	}
	used, usedErr := strconv.Atoi(headers.Get("api-credits-used"))
	key := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	if a.quotaStore != nil {
		if usedErr != nil || used < 0 || used > 1000000 {
			used = 0
		}
		_ = a.quotaStore.ObserveMarketCredits(context.Background(), fmt.Sprintf("%x", key[:]), left, used, time.Now())
	}
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	u := a.usage[key]
	now := time.Now()
	minute := now.UTC().Truncate(time.Minute).Unix()
	if u.MinuteBucket != minute {
		u.MinuteBucket, u.MinuteUsed, u.ObservedLeft = minute, 0, left
	}
	// Out-of-order responses can only lower the observed headroom for this minute.
	if u.ObservedAt.UTC().Truncate(time.Minute).Unix() != minute || left < u.ObservedLeft {
		u.ObservedLeft = left
	}
	u.ObservedAt = now
	if usedErr == nil && used >= 0 && used <= 1000000 {
		u.ObservedLimit = left + used
		u.ObservedUsed = used
	}
	a.usage[key] = u
}
func (a *TwelveDataAdapter) quoteBody(ctx context.Context, query url.Values, secret string, cost int, symbols []string) ([]byte, int, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, 0, MissingCredential("Market data")
	}
	if !a.reserveCreditsForSymbols(ctx, secret, cost, true, symbols) {
		return nil, 0, marketQuotaError()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.BaseURL, "/")+"/quote?"+query.Encode(), nil)
	if err != nil {
		return nil, 0, TemporarilyUnavailable()
	}
	req.Header.Set("Authorization", "apikey "+strings.TrimSpace(secret))
	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, 0, SanitizedError{Code: "provider_network_error", Message: "Market data is temporarily unavailable", Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()
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
		listings, err := ParseMarketWatchlistLimit(string(raw), r.MaxSymbols)
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
		attemptAt := time.Now()
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
			newSymbols := make([]string, 0, len(indices))
			for _, i := range indices {
				if inputs[i].listing != nil {
					newSymbols = append(newSymbols, inputs[i].listing.ID)
				} else {
					newSymbols = append(newSymbols, inputs[i].symbol)
				}
			}
			body, status, err = a.quoteBody(ctx, query, secret, len(indices), newSymbols)
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
				a.recordQuoteState(in.key, attemptAt, r.CredentialID, time.Time{}, marketDiagnosticFailureReason(symbolErr))
				if err == nil {
					a.recordBackoff(scope, symbolErr)
				}
			} else {
				fetched[in.key] = quote
				a.clearFailure(in.key)
				a.recordQuoteState(in.key, attemptAt, r.CredentialID, quote.QuoteTimestamp, "")
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
				ttl := a.marketQuoteTTL(q, time.Now())
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
			if classified, ok := errors.AsType[SanitizedError](err); ok {
				code = classified.Code
				if code == "provider_network_error" {
					// Keep the existing renderer-facing error contract; the richer
					// network category is exposed only through owner diagnostics.
					code = "provider_temporarily_unavailable"
				}
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

func marketDiagnosticFailureReason(err error) string {
	var classified SanitizedError
	if !errors.As(err, &classified) {
		return "PROVIDER_ERROR"
	}
	switch classified.Code {
	case "provider_rate_limited":
		return "RATE_LIMITED"
	case "provider_entitlement_required":
		return "PLAN_REQUIRED"
	case "provider_credential_invalid", "provider_credential_missing":
		return "INVALID_CREDENTIAL"
	case "invalid_symbol":
		return "INVALID_SYMBOL"
	case "provider_response_invalid", "listing_mismatch":
		return "BAD_RESPONSE"
	case "provider_network_error":
		return "NETWORK_ERROR"
	default:
		return "PROVIDER_ERROR"
	}
}

func (a *TwelveDataAdapter) recordQuoteState(key string, attempted time.Time, credentialID string, successfulQuote time.Time, failureReason string) {
	a.quoteStateMu.Lock()
	defer a.quoteStateMu.Unlock()
	state := a.quoteState[key]
	state.LastRefreshAttempt = attempted
	state.CredentialID = credentialID
	if !successfulQuote.IsZero() {
		state.LastSuccessfulQuoteTimestamp = successfulQuote
	}
	state.FailureReason = failureReason
	a.quoteState[key] = state
}

// QuoteDiagnostics reports the current in-memory quote/cache state only. It
// does not resolve a provider secret or initiate a provider request.
func (a *TwelveDataAdapter) QuoteDiagnostics(ctx context.Context, request MarketRequest) ([]MarketQuoteDiagnostic, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	scope, err := a.credentialCacheScope(ctx, request.CredentialID, request.ScopeType, request.ScopeID)
	if err != nil {
		return nil, MissingCredential("Market data")
	}
	inputs := make([]quoteInput, 0, max(len(request.Symbols), len(request.Listings)))
	if request.Listings != nil {
		raw, _ := json.Marshal(request.Listings)
		listings, err := ParseMarketWatchlistLimit(string(raw), request.MaxSymbols)
		if err != nil {
			return nil, err
		}
		for i := range listings {
			listing := listings[i]
			inputs = append(inputs, quoteInput{key: scope + ":" + listing.ID + ":" + listing.Currency, symbol: listing.Symbol, listing: &listing})
		}
	} else {
		for _, raw := range request.Symbols {
			symbol, _ := NormalizeMarketSymbol(raw)
			inputs = append(inputs, quoteInput{key: scope + ":" + symbol, symbol: symbol})
		}
	}

	now := time.Now()
	result := make([]MarketQuoteDiagnostic, 0, len(inputs))
	for _, input := range inputs {
		a.quoteStateMu.Lock()
		state := a.quoteState[input.key]
		a.quoteStateMu.Unlock()

		a.Cache.mu.Lock()
		entry, found := a.Cache.entries[input.key]
		a.Cache.mu.Unlock()
		stale := found && now.After(entry.freshUntil) && now.Before(entry.staleUntil)

		retryAt := time.Time{}
		a.failureMu.Lock()
		if failure, ok := a.failures[input.key]; ok && now.Before(failure.until) {
			retryAt = failure.until
		}
		a.failureMu.Unlock()
		a.backoffMu.Lock()
		if until := a.backoffUntil[scope]; now.Before(until) && until.After(retryAt) {
			retryAt = until
		}
		a.backoffMu.Unlock()

		diagnostic := MarketQuoteDiagnostic{
			Symbol: input.symbol, LastSuccessfulQuoteTimestamp: state.LastSuccessfulQuoteTimestamp,
			LastRefreshAttempt: state.LastRefreshAttempt, FailureReason: state.FailureReason,
			ProviderID: marketProviderID, CredentialID: request.CredentialID,
			Stale: stale, RetryBackoffActive: now.Before(retryAt), RetryAt: retryAt,
		}
		if state.CredentialID != "" {
			diagnostic.CredentialID = state.CredentialID
		}
		result = append(result, diagnostic)
	}
	return result, nil
}

// Stores may version cache scopes without decrypting the credential. Test and
// legacy resolvers retain their existing owner + credential identity.
func (a *TwelveDataAdapter) credentialCacheScope(ctx context.Context, id, scopeType, scopeID string) (string, error) {
	if resolver, ok := a.Credentials.(interface {
		CredentialCacheScope(ctx context.Context, id, scopeType, scopeID string) (string, error)
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

func (a *TwelveDataAdapter) marketQuoteTTL(q MarketQuote, now time.Time) time.Duration {
	ttl := a.quoteFreshTTL([]MarketQuote{q})
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

func marketQuoteTTL(q MarketQuote, now time.Time) time.Duration {
	return (&TwelveDataAdapter{quotaPolicy: DefaultMarketQuotaPolicy()}).marketQuoteTTL(q, now)
}
