package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"time"

	"tronbyt-server/internal/data"
	"tronbyt-server/internal/providers"
	"tronbyt-server/internal/provisioning"

	"gorm.io/gorm"
)

var providerNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)

type pairingAttemptWindow struct {
	mu      sync.Mutex
	started time.Time
	count   int
}

func (s *Server) allowPairingAttempt(remoteAddress string) bool {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		host = remoteAddress
	}
	value, _ := s.pairingAttempts.LoadOrStore(host, &pairingAttemptWindow{started: time.Now()})
	window := value.(*pairingAttemptWindow)
	window.mu.Lock()
	defer window.mu.Unlock()
	now := time.Now()
	if now.Sub(window.started) >= time.Minute {
		window.started, window.count = now, 0
	}
	if window.count >= 5 {
		return false
	}
	window.count++
	return true
}

func requireOwnerMobileAPI(r *http.Request) (*data.User, error) {
	if _, err := MobilePrincipalFromContext(r.Context()); err == nil {
		return nil, provisioning.ErrForbidden
	}
	if _, err := DeviceFromContext(r.Context()); err == nil {
		return nil, provisioning.ErrForbidden
	}
	user, err := UserFromContext(r.Context())
	if err != nil {
		return nil, provisioning.ErrForbidden
	}
	return user, nil
}

func (s *Server) handleCreateHouseholdMember(w http.ResponseWriter, r *http.Request) {
	owner, err := requireOwnerMobileAPI(r)
	if err != nil {
		writeAPIError(w, http.StatusForbidden, "owner_required", "Only the server owner can provision members", nil)
		return
	}
	if s.Provisioning == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "pairing_unavailable", "Household pairing is not configured", nil)
		return
	}
	var request struct {
		DisplayName string   `json:"displayName"`
		DeviceIDs   []string `json:"deviceIDs"`
	}
	if !decodeAPIJSON(w, r, &request) {
		return
	}
	member, err := s.Provisioning.CreateMember(r.Context(), owner.Username, request.DisplayName, request.DeviceIDs)
	if err != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(err, provisioning.ErrForbidden) {
			status = http.StatusForbidden
		}
		writeAPIError(w, status, "member_provision_failed", "The member could not be provisioned for those devices", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"member": member, "deviceIDs": request.DeviceIDs})
}

func (s *Server) handleCreatePairingCode(w http.ResponseWriter, r *http.Request) {
	owner, err := requireOwnerMobileAPI(r)
	if err != nil {
		writeAPIError(w, http.StatusForbidden, "owner_required", "Only the server owner can create pairing codes", nil)
		return
	}
	if s.Provisioning == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "pairing_unavailable", "Household pairing is not configured", nil)
		return
	}
	var request struct {
		LifetimeSeconds int `json:"lifetimeSeconds"`
	}
	if !decodeAPIJSON(w, r, &request) {
		return
	}
	if request.LifetimeSeconds == 0 {
		request.LifetimeSeconds = 600
	}
	code, record, err := s.Provisioning.CreatePairingCode(r.Context(), owner.Username, r.PathValue("memberID"), time.Duration(request.LifetimeSeconds)*time.Second)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "pairing_code_failed", "A pairing code could not be created", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "expiresAt": record.ExpiresAt, "singleUse": true})
}

func (s *Server) handleRedeemPairingCode(w http.ResponseWriter, r *http.Request) {
	if s.Provisioning == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "pairing_unavailable", "Household pairing is not configured", nil)
		return
	}
	if !s.allowPairingAttempt(r.RemoteAddr) {
		w.Header().Set("Retry-After", "60")
		writeAPIError(w, http.StatusTooManyRequests, "pairing_rate_limited", "Too many pairing attempts; try again shortly", nil)
		return
	}
	var request struct {
		Code string `json:"code"`
	}
	if !decodeAPIJSON(w, r, &request) {
		return
	}
	result, err := s.Provisioning.Redeem(r.Context(), request.Code, 90*24*time.Hour)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "pairing_code_invalid", "The pairing code is invalid, expired, or already used", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"sessionToken": result.SessionToken, "sessionID": result.SessionID, "memberID": result.MemberID,
		"expiresAt": result.ExpiresAt, "deviceIDs": result.DeviceIDs,
	})
}

func (s *Server) handleRevokeMobileSession(w http.ResponseWriter, r *http.Request) {
	owner, err := requireOwnerMobileAPI(r)
	if err != nil {
		writeAPIError(w, http.StatusForbidden, "owner_required", "Only the server owner can revoke member sessions", nil)
		return
	}
	if s.Provisioning == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "pairing_unavailable", "Household pairing is not configured", nil)
		return
	}
	if err := s.Provisioning.Revoke(r.Context(), owner.Username, r.PathValue("sessionID")); err != nil {
		writeAPIError(w, http.StatusNotFound, "session_not_found", "The mobile session was not found", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListHouseholdMembers(w http.ResponseWriter, r *http.Request) {
	owner, _ := requireOwnerMobileAPI(r)
	if s.Provisioning == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "pairing_unavailable", "Household pairing is not configured", nil)
		return
	}
	members, err := s.Provisioning.ListMembers(r.Context(), owner.Username)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "members_unavailable", "Household members could not be loaded", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"members": members})
}

func (s *Server) handlePatchHouseholdMember(w http.ResponseWriter, r *http.Request) {
	owner, _ := requireOwnerMobileAPI(r)
	if s.Provisioning == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "pairing_unavailable", "Household pairing is not configured", nil)
		return
	}
	var request struct {
		DeviceIDs []string `json:"deviceIDs"`
	}
	if !decodeAPIJSON(w, r, &request) {
		return
	}
	member, err := s.Provisioning.UpdateAssignments(r.Context(), owner.Username, r.PathValue("memberID"), request.DeviceIDs)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "member_update_failed", "The member could not be assigned to those devices", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(member)
}

func (s *Server) handlePutProviderCredential(w http.ResponseWriter, r *http.Request) {
	owner, err := requireOwnerMobileAPI(r)
	if err != nil {
		writeAPIError(w, http.StatusForbidden, "owner_required", "Provider credentials are available only to the server owner", nil)
		return
	}
	if s.CredentialStore == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "credential_store_unavailable", "Provider credential encryption is not configured", nil)
		return
	}
	var request struct {
		Provider  string `json:"provider"`
		ScopeType string `json:"scopeType"`
		ScopeID   string `json:"scopeID"`
		Secret    string `json:"secret"`
	}
	if !decodeAPIJSON(w, r, &request) {
		return
	}
	request.Provider = strings.ToLower(strings.TrimSpace(request.Provider))
	if request.ScopeType == "" {
		request.ScopeType, request.ScopeID = "server_owner", owner.Username
	}
	if !providerNamePattern.MatchString(request.Provider) || !s.ownerControlsCredentialScope(r, owner, request.ScopeType, request.ScopeID) {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_credential_scope", "The provider or credential scope is invalid", nil)
		return
	}
	metadata, err := s.CredentialStore.Put(r.Context(), r.PathValue("credentialID"), request.Provider, request.ScopeType, request.ScopeID, request.Secret)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "credential_store_failed", "The provider credential could not be stored", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(metadata)
}

func (s *Server) handleGetProviderCredentialMetadata(w http.ResponseWriter, r *http.Request) {
	owner, err := requireOwnerMobileAPI(r)
	if err != nil || s.CredentialStore == nil {
		writeAPIError(w, http.StatusForbidden, "owner_required", "Provider credential metadata is available only to the server owner", nil)
		return
	}
	scopeType, scopeID := r.URL.Query().Get("scopeType"), r.URL.Query().Get("scopeID")
	if !s.ownerControlsCredentialScope(r, owner, scopeType, scopeID) {
		writeAPIError(w, http.StatusForbidden, "credential_scope_forbidden", "The credential scope is not available", nil)
		return
	}
	metadata, err := s.CredentialStore.Metadata(r.Context(), r.PathValue("credentialID"), scopeType, scopeID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "credential_not_found", "Provider credential metadata was not found", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(metadata)
}

func (s *Server) handleListProviderCredentials(w http.ResponseWriter, r *http.Request) {
	owner, _ := requireOwnerMobileAPI(r)
	if s.CredentialStore == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "credential_store_unavailable", "Provider credential encryption is not configured", nil)
		return
	}
	scopeType, scopeID := credentialScopeQuery(r, owner)
	if !s.ownerControlsCredentialScope(r, owner, scopeType, scopeID) {
		writeAPIError(w, http.StatusForbidden, "credential_scope_forbidden", "The credential scope is not available", nil)
		return
	}
	items, err := s.CredentialStore.List(r.Context(), scopeType, scopeID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "credentials_unavailable", "Provider credentials could not be loaded", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"credentials": items})
}

func (s *Server) handlePatchProviderCredential(w http.ResponseWriter, r *http.Request) {
	owner, _ := requireOwnerMobileAPI(r)
	if s.CredentialStore == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "credential_store_unavailable", "Provider credential encryption is not configured", nil)
		return
	}
	var request struct {
		ScopeType string `json:"scopeType"`
		ScopeID   string `json:"scopeID"`
		Enabled   *bool  `json:"enabled"`
	}
	if !decodeAPIJSON(w, r, &request) {
		return
	}
	if request.ScopeType == "" {
		request.ScopeType, request.ScopeID = "server_owner", owner.Username
	}
	if request.Enabled == nil || !s.ownerControlsCredentialScope(r, owner, request.ScopeType, request.ScopeID) {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_credential_update", "The credential update is invalid", nil)
		return
	}
	metadata, err := s.CredentialStore.SetEnabled(r.Context(), r.PathValue("credentialID"), request.ScopeType, request.ScopeID, *request.Enabled)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "credential_not_found", "Provider credential metadata was not found", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(metadata)
}

func (s *Server) handleDeleteProviderCredential(w http.ResponseWriter, r *http.Request) {
	owner, _ := requireOwnerMobileAPI(r)
	if s.CredentialStore == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "credential_store_unavailable", "Provider credential encryption is not configured", nil)
		return
	}
	scopeType, scopeID := credentialScopeQuery(r, owner)
	if !s.ownerControlsCredentialScope(r, owner, scopeType, scopeID) {
		writeAPIError(w, http.StatusForbidden, "credential_scope_forbidden", "The credential scope is not available", nil)
		return
	}
	if err := s.CredentialStore.Delete(r.Context(), r.PathValue("credentialID"), scopeType, scopeID); err != nil {
		writeAPIError(w, http.StatusNotFound, "credential_not_found", "Provider credential metadata was not found", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleValidateProviderCredential(w http.ResponseWriter, r *http.Request) {
	owner, _ := requireOwnerMobileAPI(r)
	if s.CredentialStore == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "credential_store_unavailable", "Provider credential encryption is not configured", nil)
		return
	}
	scopeType, scopeID := credentialScopeQuery(r, owner)
	if !s.ownerControlsCredentialScope(r, owner, scopeType, scopeID) {
		writeAPIError(w, http.StatusForbidden, "credential_scope_forbidden", "The credential scope is not available", nil)
		return
	}
	metadata, err := s.CredentialStore.Metadata(r.Context(), r.PathValue("credentialID"), scopeType, scopeID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "credential_not_found", "Provider credential metadata was not found", nil)
		return
	}
	var validator providers.CredentialValidator
	switch metadata.Provider {
	case "twelve-data", "twelvedata", "market":
		validator, _ = s.MarketProvider.(providers.CredentialValidator)
	case "openweather":
		validator, _ = s.WeatherProvider.(providers.CredentialValidator)
	}
	if validator == nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "provider_validation_unsupported", "This provider does not support validation", nil)
		return
	}
	if err := validator.ValidateCredential(r.Context(), metadata.ID, scopeType, scopeID); err != nil {
		_ = s.CredentialStore.MarkValidation(r.Context(), metadata.ID, "invalid")
		writeAPIError(w, http.StatusUnprocessableEntity, "provider_credential_invalid", "The provider credential could not be validated", nil)
		return
	}
	_ = s.CredentialStore.MarkValidation(r.Context(), metadata.ID, "valid")
	metadata, _ = s.CredentialStore.Metadata(r.Context(), metadata.ID, scopeType, scopeID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(metadata)
}

func credentialScopeQuery(r *http.Request, owner *data.User) (string, string) {
	scopeType, scopeID := r.URL.Query().Get("scopeType"), r.URL.Query().Get("scopeID")
	if scopeType == "" {
		return "server_owner", owner.Username
	}
	return scopeType, scopeID
}

func (s *Server) ownerControlsCredentialScope(r *http.Request, owner *data.User, scopeType, scopeID string) bool {
	switch scopeType {
	case "server_owner", "user":
		return scopeID == owner.Username
	case "household":
		count, _ := gorm.G[data.Household](s.DB).Where("id = ? AND owner_username = ?", scopeID, owner.Username).Count(r.Context(), "*")
		return count == 1
	case "device":
		count, _ := gorm.G[data.Device](s.DB).Where("id = ? AND username = ?", scopeID, owner.Username).Count(r.Context(), "*")
		return count == 1
	default:
		return false
	}
}

type starterBundleAppResult struct {
	AppID   string `json:"appID"`
	Status  string `json:"status"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func (s *Server) handleInstallStarterBundle(w http.ResponseWriter, r *http.Request) {
	bundle, ok := verifiedStarterBundles[r.PathValue("bundleID")]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "bundle_not_found", "Starter bundle not found", nil)
		return
	}
	results := make([]starterBundleAppResult, 0, len(bundle.AppIDs))
	failed := false
	for _, appID := range bundle.AppIDs {
		verified, exists := verifiedMetadataFor(appID)
		if !exists || !verified.Verified {
			results = append(results, starterBundleAppResult{AppID: appID, Status: "failed", Code: "not_verified", Message: "App is not in the verified manifest"})
			failed = true
			continue
		}
		item, findErr := s.findCatalogueApp(GetUser(r), appID)
		if findErr != nil {
			results = append(results, starterBundleAppResult{AppID: appID, Status: "failed", Code: "catalogue_missing", Message: "Verified app is unavailable in the current catalogue"})
			failed = true
			continue
		}
		interval := item.RecommendedInterval
		if interval < 1 {
			interval = 5
		}
		payload, _ := json.Marshal(installationCreateRequest{
			AppID: appID, Config: verified.PreferredConfigurationDefaults, DisplayTimeSec: 15,
			RenderIntervalMin: interval, MutationID: "bundle-" + bundle.ID + "-" + appID,
		})
		internal := r.Clone(r.Context())
		internal.Method = http.MethodPost
		internal.Body = io.NopCloser(bytes.NewReader(payload))
		internal.Header = r.Header.Clone()
		internal.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		s.handleInstallationCreate(recorder, internal)
		if recorder.Code == http.StatusCreated || recorder.Code == http.StatusOK {
			results = append(results, starterBundleAppResult{AppID: appID, Status: "installed"})
			continue
		}
		var response apiError
		_ = json.Unmarshal(recorder.Body.Bytes(), &response)
		if recorder.Code == http.StatusConflict && response.Error.Code == "duplicate_installation" {
			results = append(results, starterBundleAppResult{AppID: appID, Status: "already_installed"})
			continue
		}
		failed = true
		results = append(results, starterBundleAppResult{AppID: appID, Status: "failed", Code: response.Error.Code, Message: response.Error.Message})
	}
	status := http.StatusOK
	if failed {
		status = http.StatusMultiStatus
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"bundleID": bundle.ID, "name": bundle.Name, "results": results, "partialFailure": failed})
}
