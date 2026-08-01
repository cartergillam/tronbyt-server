package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"tronbyt-server/internal/data"
	"tronbyt-server/internal/version"

	"gorm.io/gorm"
)

type diagnosticEvent struct {
	Time           time.Time `json:"time"`
	Type           string    `json:"type"`
	Message        string    `json:"message"`
	InstallationID string    `json:"installationID,omitempty"`
}

type deviceEventTimeline struct {
	mu       sync.RWMutex
	capacity int
	byDevice map[string][]diagnosticEvent
	health   map[string]string
}

func newDeviceEventTimeline(capacity int) *deviceEventTimeline {
	return &deviceEventTimeline{capacity: capacity, byDevice: map[string][]diagnosticEvent{}, health: map[string]string{}}
}

func (t *deviceEventTimeline) add(deviceID, eventType, message, installationID string) {
	if t == nil || deviceID == "" {
		return
	}
	event := diagnosticEvent{Time: time.Now().UTC(), Type: eventType, Message: sanitizedRenderMessage(message), InstallationID: installationID}
	t.mu.Lock()
	defer t.mu.Unlock()
	values := append(t.byDevice[deviceID], event)
	if len(values) > t.capacity {
		values = append([]diagnosticEvent(nil), values[len(values)-t.capacity:]...)
	}
	t.byDevice[deviceID] = values
}

func (t *deviceEventTimeline) setHealth(deviceID, health string) {
	if t == nil || deviceID == "" || health == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	previous := t.health[deviceID]
	if previous == health {
		return
	}
	t.health[deviceID] = health
	var event diagnosticEvent
	switch {
	case health == "connected" && (previous == "stale" || previous == "offline"):
		event = diagnosticEvent{Time: time.Now().UTC(), Type: "device_resumed", Message: "Device resumed polling"}
	case health == "stale" || health == "offline":
		event = diagnosticEvent{Time: time.Now().UTC(), Type: "device_stale", Message: "Device polling became " + health}
	default:
		return
	}
	values := append(t.byDevice[deviceID], event)
	if len(values) > t.capacity {
		values = append([]diagnosticEvent(nil), values[len(values)-t.capacity:]...)
	}
	t.byDevice[deviceID] = values
}

func (t *deviceEventTimeline) recent(deviceID string, limit int) []diagnosticEvent {
	if t == nil {
		return []diagnosticEvent{}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	values := t.byDevice[deviceID]
	if limit <= 0 || limit > len(values) {
		limit = len(values)
	}
	result := append([]diagnosticEvent(nil), values[len(values)-limit:]...)
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

type pollDiagnostic struct {
	LastSuccess time.Time
	LatencyMS   int64
}

type diagnosticFrame struct {
	Exists     bool   `json:"exists"`
	Bytes      int64  `json:"bytes"`
	HashPrefix string `json:"hashPrefix,omitempty"`
}

type diagnosticApp struct {
	InstallationID     string          `json:"installationID"`
	Name               string          `json:"name"`
	Enabled            bool            `json:"enabled"`
	Pinned             bool            `json:"pinned"`
	RenderResult       string          `json:"renderResult"`
	LastAttempt        *time.Time      `json:"lastAttempt,omitempty"`
	LastVisibleSuccess *time.Time      `json:"lastVisibleSuccess,omitempty"`
	NextEligibleRender *time.Time      `json:"nextEligibleRender,omitempty"`
	ConsecutiveFailure int             `json:"consecutiveFailureCount"`
	ConsecutiveHidden  int             `json:"consecutiveHiddenCount"`
	Message            string          `json:"message,omitempty"`
	LocationSource     string          `json:"locationSource"`
	Frame              diagnosticFrame `json:"frame"`
}

type deviceDiagnostics struct {
	DeviceID                string            `json:"deviceID"`
	Protocol                string            `json:"protocol"`
	FirmwareVersion         string            `json:"firmwareVersion,omitempty"`
	LastSeen                *time.Time        `json:"lastSeen,omitempty"`
	SecondsSinceLastSeen    *int64            `json:"secondsSinceLastSeen,omitempty"`
	Connection              string            `json:"connection"`
	PollIntervalSeconds     int               `json:"pollIntervalSeconds"`
	LastSuccessfulPoll      *time.Time        `json:"lastSuccessfulPoll,omitempty"`
	RecentPollLatencyMS     *int64            `json:"recentPollLatencyMS,omitempty"`
	DisplayingApp           string            `json:"displayingApp,omitempty"`
	LastRotationIndex       int               `json:"lastRotationIndex"`
	EnabledAppCount         int               `json:"enabledAppCount"`
	HiddenAppCount          int               `json:"hiddenAppCount"`
	FailingAppCount         int               `json:"failingAppCount"`
	TemporaryPushCount      int               `json:"temporaryPushCount"`
	PersistentPushCount     int               `json:"persistentPushCount"`
	PendingRestore          string            `json:"pendingRestore,omitempty"`
	PinnedApp               string            `json:"pinnedApp,omitempty"`
	EffectiveBrightnessMode string            `json:"effectiveBrightnessMode"`
	EffectiveBrightness     int               `json:"effectiveBrightness"`
	Timezone                string            `json:"timezone"`
	LocationSummary         string            `json:"locationSummary,omitempty"`
	Apps                    []diagnosticApp   `json:"apps"`
	Events                  []diagnosticEvent `json:"events"`
}

func appLocationSource(app *data.App, device *data.Device) string {
	if value, ok := app.Config["location"]; ok {
		if value != nil && value != "" && value != "__device__" {
			return "custom"
		}
		if device.HasLocation() {
			return "device"
		}
		return "missing"
	}
	if device.HasLocation() {
		return "device"
	}
	if appUsesDeviceLocation(app) {
		return "fallback"
	}
	return "missing"
}

func (s *Server) frameDiagnostic(deviceID string, app *data.App) diagnosticFrame {
	dir := filepath.Join(s.DataDir, "webp", deviceID)
	path := s.getAppWebpPath(dir, app)
	content, err := os.ReadFile(path)
	if err != nil {
		return diagnosticFrame{}
	}
	sum := sha256.Sum256(content)
	return diagnosticFrame{Exists: true, Bytes: int64(len(content)), HashPrefix: hex.EncodeToString(sum[:6])}
}

func effectiveBrightnessMode(device *data.Device) string {
	switch {
	case device.GetNightModeIsActive():
		return "night"
	case device.GetDimModeIsActive():
		return "dim"
	case device.Brightness == 0:
		return "off"
	default:
		return "normal"
	}
}

func (s *Server) buildDeviceDiagnostics(device *data.Device) deviceDiagnostics {
	now := time.Now()
	interval := max(device.DefaultInterval, 15)
	result := deviceDiagnostics{
		DeviceID: device.ID, Protocol: string(device.Info.ProtocolType), FirmwareVersion: device.Info.FirmwareVersion,
		LastSeen: device.LastSeen, PollIntervalSeconds: interval, DisplayingApp: optionalString(device.DisplayingApp),
		LastRotationIndex: device.LastAppIndex, PendingRestore: optionalString(device.DisplayRestoreApp), PinnedApp: optionalString(device.PinnedApp),
		EffectiveBrightnessMode: effectiveBrightnessMode(device), EffectiveBrightness: int(device.GetEffectiveBrightness()),
		Timezone: device.GetTimezone(), Apps: []diagnosticApp{},
	}
	if device.HasLocation() {
		result.LocationSummary = strings.TrimSpace(strings.Trim(device.Location.Description+" · "+device.Location.Timezone, " ·"))
	}
	if device.LastSeen == nil {
		result.Connection = "offline"
	} else {
		seconds := int64(now.Sub(*device.LastSeen).Seconds())
		if seconds < 0 {
			seconds = 0
		}
		result.SecondsSinceLastSeen = &seconds
		switch {
		case seconds <= int64(max(interval*3, 45)):
			result.Connection = "connected"
		case seconds <= int64(max(interval*10, 300)):
			result.Connection = "stale"
		default:
			result.Connection = "offline"
		}
	}
	if value, ok := s.pollDiagnostics.Load(device.ID); ok {
		poll := value.(pollDiagnostic)
		result.LastSuccessfulPoll = &poll.LastSuccess
		result.RecentPollLatencyMS = &poll.LatencyMS
	}
	s.diagnosticsEvents.setHealth(device.ID, result.Connection)
	result.Events = s.diagnosticsEvents.recent(device.ID, 50)
	for _, app := range device.Apps {
		if app.Pushed {
			if app.PushKind == persistentPushKind {
				result.PersistentPushCount++
			}
			continue
		}
		if app.Enabled {
			result.EnabledAppCount++
		}
		if app.LastRenderResult == "hidden" {
			result.HiddenAppCount++
		}
		if app.LastRenderResult == "failure" || app.LastRenderResult == "upstream_failure" || app.LastRenderResult == "empty" {
			result.FailingAppCount++
		}
		var lastAttempt *time.Time
		if !app.LastRender.IsZero() {
			value := app.LastRender
			lastAttempt = &value
		}
		result.Apps = append(result.Apps, diagnosticApp{
			InstallationID: app.Iname, Name: app.Name, Enabled: app.Enabled, Pinned: result.PinnedApp == app.Iname,
			RenderResult: app.LastRenderResult, LastAttempt: lastAttempt, LastVisibleSuccess: app.LastSuccessfulRender,
			NextEligibleRender: app.NextRenderAt, ConsecutiveFailure: app.ConsecutiveFailures, ConsecutiveHidden: app.ConsecutiveHidden,
			Message: sanitizedRenderMessage(app.LastRenderMessage), LocationSource: appLocationSource(app, device), Frame: s.frameDiagnostic(device.ID, app),
		})
	}
	pushedDir := filepath.Join(s.DataDir, "webp", device.ID, "pushed")
	if entries, err := os.ReadDir(pushedDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasPrefix(entry.Name(), "__") && strings.HasSuffix(entry.Name(), ".webp") {
				result.TemporaryPushCount++
			}
		}
	}
	sort.SliceStable(result.Apps, func(i, j int) bool { return result.Apps[i].InstallationID < result.Apps[j].InstallationID })
	return result
}

func (s *Server) handleDeviceDiagnostics(w http.ResponseWriter, r *http.Request) {
	device := GetDevice(r)
	fresh, err := gorm.G[data.Device](s.DB).Preload("Apps", orderedAppsPreload).Where("id = ?", device.ID).First(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "diagnostics_unavailable", "Device diagnostics could not be loaded", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.buildDeviceDiagnostics(&fresh))
}

func (s *Server) handleRetryInstallationRender(w http.ResponseWriter, r *http.Request) {
	device := GetDevice(r)
	iname := r.PathValue("iname")
	app := device.GetApp(iname)
	if app == nil || app.Pushed {
		writeAPIError(w, http.StatusNotFound, "app_not_found", "Installation not found", nil)
		return
	}
	if _, err := gorm.G[data.App](s.DB).Where("id = ?", app.ID).Updates(r.Context(), data.App{LastRender: time.Time{}, NextRenderAt: nil}); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "retry_failed", "Render retry could not be scheduled", nil)
		return
	}
	s.diagnosticsEvents.add(device.ID, "render_retry", "Render retry scheduled", iname)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) clearTemporaryPushFiles(deviceID string) (int, error) {
	dir := filepath.Join(s.DataDir, "webp", deviceID, "pushed")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "__") || !strings.HasSuffix(entry.Name(), ".webp") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func (s *Server) handleClearTemporaryDisplay(w http.ResponseWriter, r *http.Request) {
	device := GetDevice(r)
	removed, err := s.clearTemporaryPushFiles(device.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "temporary_clear_failed", "Temporary display state could not be cleared", nil)
		return
	}
	s.diagnosticsEvents.add(device.ID, "temporary_push_cleaned", fmt.Sprintf("Cleared %d temporary frame(s)", removed), "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRestoreNormalRotation(w http.ResponseWriter, r *http.Request) {
	device := GetDevice(r)
	if err := s.DB.Model(&data.Device{ID: device.ID}).Updates(map[string]any{"pinned_app": nil, "display_restore_app": nil, "displaying_app": nil, "last_app_index": -1}).Error; err != nil {
		writeAPIError(w, http.StatusInternalServerError, "restore_failed", "Normal rotation could not be restored", nil)
		return
	}
	_, _ = s.clearTemporaryPushFiles(device.ID)
	s.diagnosticsEvents.add(device.ID, "rotation_restored", "Normal rotation restored", "")
	w.WriteHeader(http.StatusNoContent)
}

type capabilityResponse struct {
	ServerVersion      string   `json:"serverVersion"`
	APIVersion         string   `json:"apiVersion"`
	Features           []string `json:"features"`
	SchemaFieldTypes   []string `json:"schemaFieldTypes"`
	IconRevision       string   `json:"iconRevision"`
	MinimumIOSVersion  string   `json:"minimumIOSVersion,omitempty"`
	AuthorizationScope string   `json:"authorizationScope"`
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	revision := s.catalogueRevision(GetUser(r))
	response := capabilityResponse{
		ServerVersion: version.Version, APIVersion: "0.2",
		Features:         []string{"capabilities", "device-diagnostics", "diagnostic-actions", "installation-preview", "catalogue-pagination", "catalogue-filters", "catalogue-compatibility", "icon-etag", "location-v1", "location-source", "temporary-push-v2", "firmware-update", "schema-v1"},
		SchemaFieldTypes: []string{"string", "multiline", "integer", "number", "boolean", "enum", "date", "time", "datetime", "secret", "colour", "location"},
		IconRevision:     revision,
	}
	if _, err := DeviceFromContext(r.Context()); err == nil {
		response.AuthorizationScope = "device"
	} else {
		response.AuthorizationScope = "user"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=300")
	_ = json.NewEncoder(w).Encode(response)
}
