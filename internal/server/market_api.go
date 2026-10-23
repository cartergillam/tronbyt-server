package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"tronbyt-server/internal/data"
	"tronbyt-server/internal/providers"
)

type marketQuoteDiagnosticResponse struct {
	ProviderID   string                            `json:"providerID"`
	CredentialID string                            `json:"credentialID"`
	Quotes       []providers.MarketQuoteDiagnostic `json:"quotes"`
	Quota        *providers.MarketQuotaSnapshot    `json:"quota,omitempty"`
	Limits       *marketQuotaDiagnosticLimits      `json:"limits,omitempty"`
}

type marketQuotaDiagnosticLimits struct {
	MaxSymbols           int `json:"maxSymbols"`
	OpenRefreshSeconds   int `json:"openRefreshSeconds"`
	ClosedRefreshSeconds int `json:"closedRefreshSeconds"`
	DailyCredits         int `json:"dailyCredits"`
	MinuteCredits        int `json:"minuteCredits"`
	NewSymbolsPerDay     int `json:"newSymbolsPerDay"`
}

// handleMarketQuoteDiagnostics is owner-only at route registration. It reads
// installed configuration and in-memory adapter state without refreshing data.
func (s *Server) handleMarketQuoteDiagnostics(w http.ResponseWriter, r *http.Request) {
	policy := marketQuotaPolicyFromConfig(s.Config)
	limits := &marketQuotaDiagnosticLimits{MaxSymbols: policy.MaxSymbols, OpenRefreshSeconds: int(policy.OpenRefresh / time.Second), ClosedRefreshSeconds: int(policy.ClosedRefresh / time.Second), DailyCredits: policy.DailyCredits, MinuteCredits: policy.MinuteCredits, NewSymbolsPerDay: policy.NewSymbolsPerDay}
	device := GetDevice(r)
	var app *data.App
	for i := range device.Apps {
		if device.Apps[i] != nil && device.Apps[i].Name == "market-watch" {
			app = device.Apps[i]
			break
		}
	}
	if app == nil {
		writeAPIError(w, http.StatusNotFound, "market_watch_not_installed", "Market Watch is not installed on this device", nil)
		return
	}
	credentialID, _ := app.Config["credential_id"].(string)
	logicalCredentialID := strings.TrimSpace(credentialID)
	if logicalCredentialID == "" || logicalCredentialID == "market-primary" || logicalCredentialID == "__device__" {
		logicalCredentialID = strings.TrimSpace(device.MarketCredentialID)
		if logicalCredentialID == "" {
			logicalCredentialID = "market-primary"
		}
	}
	credentialID, err := s.marketCredential(r.Context(), device, credentialID)
	if err != nil {
		request, requestErr := marketDiagnosticsRequest(device, app, logicalCredentialID, s.marketMaxSymbols())
		if requestErr != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "market_diagnostics_unavailable", "Market Watch diagnostics could not be read", nil)
			return
		}
		response := marketQuoteDiagnosticResponse{ProviderID: "twelve-data", CredentialID: logicalCredentialID, Quotes: make([]providers.MarketQuoteDiagnostic, 0, len(request.Symbols)+len(request.Listings)), Limits: limits}
		if request.Listings != nil {
			for _, listing := range request.Listings {
				response.Quotes = append(response.Quotes, providers.MarketQuoteDiagnostic{Symbol: listing.Symbol, ProviderID: "twelve-data", CredentialID: logicalCredentialID, FailureReason: "INVALID_CREDENTIAL"})
			}
		} else {
			for _, symbol := range request.Symbols {
				response.Quotes = append(response.Quotes, providers.MarketQuoteDiagnostic{Symbol: symbol, ProviderID: "twelve-data", CredentialID: logicalCredentialID, FailureReason: "INVALID_CREDENTIAL"})
			}
		}
		writeMarketQuoteDiagnostics(w, response)
		return
	}
	request, err := marketDiagnosticsRequest(device, app, credentialID, s.marketMaxSymbols())
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "market_diagnostics_unavailable", "Market Watch diagnostics could not be read", nil)
		return
	}
	provider, ok := s.MarketProvider.(providers.MarketQuoteDiagnosticProvider)
	if !ok {
		writeAPIError(w, http.StatusServiceUnavailable, "market_diagnostics_unavailable", "Market Watch diagnostics are unavailable", nil)
		return
	}
	quotes, err := provider.QuoteDiagnostics(r.Context(), request)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "market_diagnostics_unavailable", "Market Watch diagnostics could not be read", nil)
		return
	}
	response := marketQuoteDiagnosticResponse{ProviderID: diagnosticProviderID(quotes), CredentialID: logicalCredentialID, Quotes: quotes, Limits: limits}
	if quotaProvider, ok := s.MarketProvider.(providers.MarketQuotaDiagnosticProvider); ok {
		if usage, policy, quotaErr := quotaProvider.CredentialQuotaDiagnostics(r.Context(), credentialID, "server_owner", device.Username); quotaErr == nil {
			response.Quota = &usage
			response.Limits = &marketQuotaDiagnosticLimits{MaxSymbols: policy.MaxSymbols, OpenRefreshSeconds: int(policy.OpenRefresh / time.Second), ClosedRefreshSeconds: int(policy.ClosedRefresh / time.Second), DailyCredits: policy.DailyCredits, MinuteCredits: policy.MinuteCredits, NewSymbolsPerDay: policy.NewSymbolsPerDay}
		}
	}
	writeMarketQuoteDiagnostics(w, response)
}

func diagnosticProviderID(quotes []providers.MarketQuoteDiagnostic) string {
	providerID := ""
	for _, quote := range quotes {
		if quote.ProviderID == "" {
			continue
		}
		if providerID == "" {
			providerID = quote.ProviderID
		} else if providerID != quote.ProviderID {
			return "mixed"
		}
	}
	if providerID == "" {
		return "twelve-data"
	}
	return providerID
}

func marketDiagnosticsRequest(device *data.Device, app *data.App, credentialID string, maxSymbols ...int) (providers.MarketRequest, error) {
	limit := providers.MaxMarketSymbols
	if len(maxSymbols) > 0 && maxSymbols[0] > 0 && maxSymbols[0] < limit {
		limit = maxSymbols[0]
	}
	request := providers.MarketRequest{CredentialID: credentialID, ScopeType: "server_owner", ScopeID: device.Username, MaxSymbols: limit}
	if app.Config == nil {
		return request, errors.New("missing Market Watch config")
	}
	if raw, ok := app.Config["watchlist"].(string); ok && strings.TrimSpace(raw) != "" {
		listings, err := providers.ParseMarketWatchlistLimit(raw, request.MaxSymbols)
		if err != nil {
			return request, err
		}
		request.Listings = listings
		return request, nil
	}
	request.Symbols = providerSymbols(app.Config["symbols"])
	return request, request.Validate()
}

func writeMarketQuoteDiagnostics(w http.ResponseWriter, response marketQuoteDiagnosticResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(response)
}

// Device authorization determines the credential owner; clients never choose a scope.
func (s *Server) handleMarketSearch(w http.ResponseWriter, r *http.Request) {
	searcher, ok := s.MarketProvider.(providers.MarketSearcher)
	if !ok {
		writeAPIError(w, 503, "provider_setup_required", "Market search is not configured", nil)
		return
	}
	credential := strings.TrimSpace(r.URL.Query().Get("credentialID"))
	if principalKindFromContext(r.Context()) == apiPrincipalMember {
		credential = "" // Members cannot select arbitrary owner credentials.
	}
	resolved, err := s.marketCredential(r.Context(), GetDevice(r), credential)
	if err != nil {
		writeAPIError(w, 422, "provider_credential_missing", "Market data is not configured", nil)
		return
	}
	credential = resolved
	items, err := searcher.Search(r.Context(), providers.MarketSearchRequest{Query: r.URL.Query().Get("q"), CredentialID: credential, ScopeType: "server_owner", ScopeID: GetDevice(r).Username})
	if err != nil {
		var classified providers.SanitizedError
		if errors.As(err, &classified) {
			status := http.StatusBadGateway
			if classified.Code == "invalid_search" {
				status = 400
			}
			if classified.Code == "provider_rate_limited" {
				status = 429
			}
			writeAPIError(w, status, classified.Code, classified.Message, nil)
		} else {
			writeAPIError(w, 502, "provider_temporarily_unavailable", "Market search is unavailable", nil)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"listings": items})
}
