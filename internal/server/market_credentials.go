package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"tronbyt-server/internal/data"
	"tronbyt-server/internal/providers"
)

func isMarketProvider(p string) bool {
	return p == "twelve-data" || p == "twelvedata" || p == "market"
}

// Legacy schema default market-primary means automatic when an assignment
// exists. Other nonempty IDs are installation overrides authorized by owners.
func (s *Server) marketCredential(ctx context.Context, d *data.Device, explicit string) (string, error) {
	id := strings.TrimSpace(explicit)
	if id == "" || id == "market-primary" || id == "__device__" {
		id = d.MarketCredentialID
		if id == "" {
			id = "market-primary"
		}
	}
	if s.CredentialStore != nil {
		m, err := s.CredentialStore.Metadata(ctx, id, "server_owner", d.Username)
		if err != nil || !m.Enabled || !isMarketProvider(m.Provider) {
			return "", providers.MissingCredential("Market data")
		}
	}
	return id, nil
}

func (s *Server) handleGetMarketCredential(w http.ResponseWriter, r *http.Request) {
	d := GetDevice(r)
	id, err := s.marketCredential(r.Context(), d, "")
	result := map[string]any{"configured": err == nil && s.CredentialStore != nil}
	if principalKindFromContext(r.Context()) == apiPrincipalOwner {
		result["credentialID"] = d.MarketCredentialID
		if err == nil && s.CredentialStore != nil {
			m, _ := s.CredentialStore.Metadata(r.Context(), id, "server_owner", d.Username)
			result["label"] = m.Label
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// RequireOwnerAPI and RequireDevice protect this handler at registration.
func (s *Server) handlePutMarketCredential(w http.ResponseWriter, r *http.Request) {
	d := GetDevice(r)
	var request struct {
		CredentialID string `json:"credentialID"`
	}
	if !decodeAPIJSON(w, r, &request) {
		return
	}
	id := strings.TrimSpace(request.CredentialID)
	if id != "" {
		if s.CredentialStore == nil {
			writeAPIError(w, 503, "credential_store_unavailable", "Provider credentials are not configured", nil)
			return
		}
		m, err := s.CredentialStore.Metadata(r.Context(), id, "server_owner", d.Username)
		if err != nil || !m.Enabled || !isMarketProvider(m.Provider) {
			writeAPIError(w, 422, "invalid_credential_assignment", "Choose an enabled market credential owned by this device's owner", nil)
			return
		}
	}
	if err := s.DB.WithContext(r.Context()).Model(&data.Device{ID: d.ID}).Update("market_credential_id", id).Error; err != nil {
		writeAPIError(w, 500, "assignment_failed", "Credential assignment could not be saved", nil)
		return
	}
	d.MarketCredentialID = id
	// Only Market Watch's render hash includes assignment; quote caches persist.
	s.handleGetMarketCredential(w, r)
}

func memberMarketCredentialChangeForbidden(r *http.Request, appName string, existing, patch map[string]any) bool {
	if appName != "market-watch" || principalKindFromContext(r.Context()) != apiPrincipalMember {
		return false
	}
	next, changed := patch["credential_id"]
	if !changed {
		return false
	}
	old, _ := existing["credential_id"].(string)
	value, ok := next.(string)
	if !ok {
		return true
	}
	if old == value {
		return false
	}
	auto := func(v string) bool { return v == "" || v == "market-primary" || v == "__device__" }
	return !(auto(old) && auto(value))
}
