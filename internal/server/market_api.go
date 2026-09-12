package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"tronbyt-server/internal/providers"
)

// Device authorization determines the credential owner; clients never choose a scope.
func (s *Server) handleMarketSearch(w http.ResponseWriter, r *http.Request) {
	searcher, ok := s.MarketProvider.(providers.MarketSearcher)
	if !ok {
		writeAPIError(w, 503, "provider_setup_required", "Market search is not configured", nil)
		return
	}
	credential := strings.TrimSpace(r.URL.Query().Get("credentialID"))
	if credential == "" {
		credential = "market-primary"
	}
	if len(credential) > 128 {
		writeAPIError(w, 400, "invalid_search", "Invalid credential reference", nil)
		return
	}
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
