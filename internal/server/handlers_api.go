package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tronbyt-server/internal/apps"
	"tronbyt-server/internal/data"

	securejoin "github.com/cyphar/filepath-securejoin"
	"gorm.io/gorm"
)

// --- API Handlers ---

// DeviceUpdate represents the updatable fields for a device via API.
type DeviceUpdate struct {
	Brightness           *int                 `json:"brightness"`
	IntervalSec          *int                 `json:"intervalSec"`
	NightModeEnabled     *bool                `json:"nightModeEnabled"`
	NightModeActive      *bool                `json:"nightModeActive"`
	NightModeApp         *string              `json:"nightModeApp"`
	NightModeBrightness  *int                 `json:"nightModeBrightness"`
	NightModeStartTime   *string              `json:"nightModeStartTime"`
	NightModeEndTime     *string              `json:"nightModeEndTime"`
	DimModeActive        *bool                `json:"dimModeActive"`
	DimModeEnabled       *bool                `json:"dimModeEnabled"`
	DimModeStartTime     *string              `json:"dimModeStartTime"`
	DimModeBrightness    *int                 `json:"dimModeBrightness"`
	PinnedApp            *string              `json:"pinnedApp"`
	AutoDim              *bool                `json:"autoDim"` // Legacy
	Location             *data.DeviceLocation `json:"location"`
	Sleeping             *bool                `json:"sleeping"`
	ExpectedStateVersion *uint64              `json:"expectedStateVersion"`
	MutationID           string               `json:"mutationID"`
}

// DevicePayload represents the full device data returned via API.
type DevicePayload struct {
	ID                  string              `json:"id"`
	Type                data.DeviceType     `json:"type"`
	DisplayName         string              `json:"displayName"`
	Notes               string              `json:"notes"`
	IntervalSec         int                 `json:"intervalSec"`
	Brightness          int                 `json:"brightness"`
	NightMode           NightMode           `json:"nightMode"`
	DimMode             DimMode             `json:"dimMode"`
	PinnedApp           *string             `json:"pinnedApp"`
	Interstitial        Interstitial        `json:"interstitial"`
	LastSeen            *string             `json:"lastSeen"`
	Info                DeviceInfo          `json:"info"`
	AutoDim             bool                `json:"autoDim"`
	Location            data.DeviceLocation `json:"location"`
	Timezone            string              `json:"timezone"`
	Sleeping            bool                `json:"sleeping"`
	EffectiveBrightness int                 `json:"effectiveBrightness"`
	StateVersion        uint64              `json:"stateVersion"`
}

// NightMode represents night mode settings in the API payload.
type NightMode struct {
	Enabled       bool    `json:"enabled"`
	Active        bool    `json:"active"`
	App           string  `json:"app"`
	StartTime     string  `json:"startTime"`
	EndTime       string  `json:"endTime"`
	Brightness    int     `json:"brightness"`
	OverrideUntil *string `json:"overrideUntil,omitempty"`
}

// DimMode represents dim mode settings in the API payload.
type DimMode struct {
	Enabled       bool    `json:"enabled"`
	Active        bool    `json:"active"`
	StartTime     *string `json:"startTime"`
	Brightness    *int    `json:"brightness"`
	OverrideUntil *string `json:"overrideUntil,omitempty"`
}

// Interstitial represents interstitial app settings in the API payload.
type Interstitial struct {
	Enabled bool    `json:"enabled"`
	App     *string `json:"app"`
}

// DeviceInfo represents device firmware and protocol information in the API payload.
type DeviceInfo struct {
	FirmwareVersion    string  `json:"firmwareVersion"`
	FirmwareType       string  `json:"firmwareType"`
	ProtocolVersion    *int    `json:"protocolVersion,omitempty"`
	MACAddress         string  `json:"macAddress"`
	ProtocolType       string  `json:"protocolType"`
	SSID               *string `json:"ssid,omitempty"`
	WifiPowerSave      *int    `json:"wifiPowerSave,omitempty"`
	SkipDisplayVersion *bool   `json:"skipDisplayVersion,omitempty"`
	SkipBootAnimation  *bool   `json:"skipBootAnimation,omitempty"`
	APMode             *bool   `json:"apMode,omitempty"`
	PreferIPv6         *bool   `json:"preferIPv6,omitempty"`
	SwapColors         *bool   `json:"swapColors,omitempty"`
	ImageURL           *string `json:"imageUrl,omitempty"`
	Hostname           *string `json:"hostname,omitempty"`
	SNTPServer         *string `json:"sntpServer,omitempty"`
	SyslogAddr         *string `json:"syslogAddr,omitempty"`
}

// toDevicePayload converts a data.Device model to a DevicePayload for API responses.
func (s *Server) toDevicePayload(d *data.Device) DevicePayload {
	now := deviceTimeNow(d)
	info := DeviceInfo{
		FirmwareVersion:    d.Info.FirmwareVersion,
		FirmwareType:       d.Info.FirmwareType,
		ProtocolVersion:    d.Info.ProtocolVersion,
		MACAddress:         d.Info.MACAddress,
		ProtocolType:       string(d.Info.ProtocolType),
		SSID:               d.Info.SSID,
		WifiPowerSave:      d.Info.WifiPowerSave,
		SkipDisplayVersion: d.Info.SkipDisplayVersion,
		SkipBootAnimation:  d.Info.SkipBootAnimation,
		APMode:             d.Info.APMode,
		PreferIPv6:         d.Info.PreferIPv6,
		SwapColors:         d.Info.SwapColors,
		ImageURL:           d.Info.ImageURL,
		Hostname:           d.Info.Hostname,
		SNTPServer:         d.Info.SNTPServer,
		SyslogAddr:         d.Info.SyslogAddr,
	}

	var lastSeen *string
	if d.LastSeen != nil {
		iso := d.LastSeen.Format(time.RFC3339)
		lastSeen = &iso
	}

	var dimBrightnessPtr *int
	if d.DimBrightness != nil {
		val := int(*d.DimBrightness)
		dimBrightnessPtr = &val
	}

	var nightModeOverrideUntil *string
	if d.GetNightModeOverrideActiveAt(now) && d.NightModeOverrideUntil != nil {
		formatted := d.NightModeOverrideUntil.In(now.Location()).Format(time.RFC3339)
		nightModeOverrideUntil = &formatted
	}
	var dimModeOverrideUntil *string
	if d.GetDimModeOverrideActiveAt(now) && d.DimModeOverrideUntil != nil {
		formatted := d.DimModeOverrideUntil.In(now.Location()).Format(time.RFC3339)
		dimModeOverrideUntil = &formatted
	}

	return DevicePayload{
		ID:          d.ID,
		Type:        d.Type,
		DisplayName: d.Name,
		Notes:       d.Notes,
		IntervalSec: d.DefaultInterval,
		Brightness:  int(d.Brightness),
		NightMode: NightMode{
			Enabled:       d.NightModeEnabled,
			Active:        d.GetNightModeIsActive(),
			App:           d.NightModeApp,
			StartTime:     d.NightStart,
			EndTime:       d.NightEnd,
			Brightness:    int(d.NightBrightness),
			OverrideUntil: nightModeOverrideUntil,
		},
		DimMode: DimMode{
			Enabled:       d.DimModeEnabled,
			Active:        d.GetDimModeIsActive(),
			StartTime:     d.DimTime,
			Brightness:    dimBrightnessPtr,
			OverrideUntil: dimModeOverrideUntil,
		},
		PinnedApp: d.PinnedApp,
		Interstitial: Interstitial{
			Enabled: d.InterstitialEnabled,
			App:     d.InterstitialApp,
		},
		LastSeen:            lastSeen,
		Info:                info,
		AutoDim:             d.NightModeEnabled,
		Location:            d.Location,
		Timezone:            d.GetTimezone(),
		Sleeping:            d.Sleeping,
		EffectiveBrightness: int(d.GetEffectiveBrightness()),
		StateVersion:        d.StateVersion,
	}
}

func validateDeviceLocation(location data.DeviceLocation) error {
	if location == (data.DeviceLocation{}) {
		return nil
	}
	if location.Lat < -90 || location.Lat > 90 {
		return fmt.Errorf("latitude must be between -90 and 90")
	}
	if location.Lng < -180 || location.Lng > 180 {
		return fmt.Errorf("longitude must be between -180 and 180")
	}
	if strings.TrimSpace(location.Timezone) == "" {
		return fmt.Errorf("timezone is required")
	}
	if _, err := time.LoadLocation(location.Timezone); err != nil {
		return fmt.Errorf("timezone must be a valid IANA identifier")
	}
	if strings.TrimSpace(location.Description) == "" && strings.TrimSpace(location.Locality) == "" {
		return fmt.Errorf("description or locality is required")
	}
	if len(location.Description) > 200 || len(location.Locality) > 100 || len(location.Region) > 100 || len(location.Country) > 100 {
		return fmt.Errorf("location description fields are too long")
	}
	if len(location.Provider) > 40 || len(location.PlaceID) > 200 {
		return fmt.Errorf("location provider identifier is too long")
	}
	return nil
}

func appUsesDeviceLocation(app *data.App) bool {
	if value, present := app.Config["location"]; present {
		return value == nil || value == "" || value == "__device__"
	}
	name := strings.ToLower(app.Name)
	return strings.Contains(name, "weather") || strings.Contains(name, "clock") || strings.Contains(name, "time") ||
		strings.Contains(name, "sunrise") || strings.Contains(name, "sunset") || strings.Contains(name, "mlb") || strings.Contains(name, "local")
}

// AppPayload represents the API response for an app installation.
type AppPayload struct {
	ID                  string `json:"id"`
	AppID               string `json:"appID"`
	Enabled             bool   `json:"enabled"`
	Pinned              bool   `json:"pinned"`
	Pushed              bool   `json:"pushed"`
	RenderIntervalMin   int    `json:"renderIntervalMin"`
	DisplayTimeSec      int    `json:"displayTimeSec"`
	LastRenderAt        *int64 `json:"lastRenderAt"`
	IsInactive          bool   `json:"isInactive"`
	LastRenderResult    string `json:"lastRenderResult,omitempty"`
	LastRenderMessage   string `json:"lastRenderMessage,omitempty"`
	NextRenderAt        *int64 `json:"nextEligibleRenderAt,omitempty"`
	ConsecutiveFailures int    `json:"consecutiveFailureCount"`
	ConsecutiveHidden   int    `json:"consecutiveHiddenCount"`
	LastVisibleRenderAt *int64 `json:"lastVisibleSuccessfulRenderAt,omitempty"`
	LocationSource      string `json:"locationSource,omitempty"`
	StateVersion        uint64 `json:"stateVersion"`

	// Schedule fields
	StartTime *string  `json:"startTime"`
	EndTime   *string  `json:"endTime"`
	Days      []string `json:"days"`

	// Recurrence fields
	UseCustomRecurrence bool                `json:"useCustomRecurrence"`
	RecurrenceType      data.RecurrenceType `json:"recurrenceType"`
	RecurrenceInterval  int                 `json:"recurrenceInterval"`
	RecurrencePattern   map[string]any      `json:"recurrencePattern"`
	RecurrenceStartDate *string             `json:"recurrenceStartDate"`
	RecurrenceEndDate   *string             `json:"recurrenceEndDate"`
}

func (s *Server) toAppPayload(device *data.Device, app *data.App) AppPayload {
	pinned := device.PinnedApp != nil && *device.PinnedApp == app.Iname
	var lastRenderAt *int64
	if !app.LastRender.IsZero() {
		value := app.LastRender.Unix()
		lastRenderAt = &value
	}
	var nextRenderAt *int64
	if app.NextRenderAt != nil {
		value := app.NextRenderAt.Unix()
		nextRenderAt = &value
	}
	var lastVisibleRenderAt *int64
	if app.LastSuccessfulRender != nil {
		value := app.LastSuccessfulRender.Unix()
		lastVisibleRenderAt = &value
	}
	return AppPayload{
		ID:                  app.Iname,
		AppID:               app.Name,
		Enabled:             app.Enabled,
		Pinned:              pinned,
		Pushed:              app.Pushed,
		RenderIntervalMin:   app.UInterval,
		DisplayTimeSec:      app.DisplayTime,
		LastRenderAt:        lastRenderAt,
		IsInactive:          app.EmptyLastRender,
		LastRenderResult:    app.LastRenderResult,
		LastRenderMessage:   app.LastRenderMessage,
		NextRenderAt:        nextRenderAt,
		ConsecutiveFailures: app.ConsecutiveFailures,
		ConsecutiveHidden:   app.ConsecutiveHidden,
		LastVisibleRenderAt: lastVisibleRenderAt,
		LocationSource:      appLocationSource(app, device),
		StateVersion:        device.StateVersion,

		StartTime: app.StartTime,
		EndTime:   app.EndTime,
		Days:      app.Days,

		UseCustomRecurrence: app.UseCustomRecurrence,
		RecurrenceType:      app.RecurrenceType,
		RecurrenceInterval:  app.RecurrenceInterval,
		RecurrencePattern:   app.RecurrencePattern,
		RecurrenceStartDate: app.RecurrenceStartDate,
		RecurrenceEndDate:   app.RecurrenceEndDate,
	}
}

// ListDevicesPayload represents the response for listing devices.
type ListDevicesPayload struct {
	Devices []DevicePayload `json:"devices"`
}

// PushAppData represents the data for pushing an app configuration.
type PushAppData struct {
	Config               map[string]any `json:"config"`
	AppID                string         `json:"app_id"`
	InstallationID       string         `json:"installationID"`
	InstallationIDAlt    string         `json:"installationId"`
	CoalesceID           string         `json:"coalesceID"`
	Background           bool           `json:"background"`
	Persistent           bool           `json:"persistent"`
	ExpectedStateVersion *uint64        `json:"expectedStateVersion"`
	MutationID           string         `json:"mutationID"`
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	user := GetUser(r)

	// If using an API key associated with a specific device, this endpoint might not make sense
	// or should return only that device. The legacy behavior (Python) returns all devices for the user.
	// Since APIAuthMiddleware populates user with all devices preloaded, we can just use that.

	devicePayloads := make([]DevicePayload, 0, len(user.Devices))
	for i := range user.Devices {
		devicePayloads = append(devicePayloads, s.toDevicePayload(&user.Devices[i]))
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(ListDevicesPayload{Devices: devicePayloads}); err != nil {
		slog.Error("Failed to encode devices JSON", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *Server) handlePushApp(w http.ResponseWriter, r *http.Request) {
	user := GetUser(r)
	device := GetDevice(r)

	var dataReq PushAppData
	if !decodeAPIJSON(w, r, &dataReq) {
		return
	}

	// Determine installationID
	installationID := dataReq.InstallationID
	if installationID == "" {
		installationID = dataReq.InstallationIDAlt
	}
	if installationID == "" && dataReq.AppID == "" {
		http.Error(w, "app_id is required when no valid installationID is provided", http.StatusBadRequest)
		return
	}
	var showNowRestoreID *string
	if !dataReq.Persistent {
		unlock := s.lockDevicePoll(device.ID)
		defer unlock()
		fresh, err := gorm.G[data.Device](s.DB).Preload("Apps", orderedAppsPreload).Where("id = ?", device.ID).First(r.Context())
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "device_reload_failed", "Device state could not be refreshed", nil)
			return
		}
		device = &fresh
		if dataReq.ExpectedStateVersion != nil && *dataReq.ExpectedStateVersion != device.StateVersion {
			writeAPIError(w, http.StatusConflict, "stale_state", "Device state changed before Show Now was applied", nil)
			return
		}
		restore := displayRestoreTarget(device)
		if restore == nil {
			writeAPIError(w, http.StatusConflict, "no_restore_target", "Enable at least one normal app before using Show Now", nil)
			return
		}
		value := restore.Iname
		showNowRestoreID = &value
	}

	// Look up existing pushed app first (by path), then fall back to iname lookup.
	var existingApp *data.App
	var appPath string
	if installationID != "" {
		if dataReq.Persistent {
			existingApp = device.GetPushedApp(installationID)
		}
		if existingApp == nil {
			existingApp = device.GetApp(installationID)
			if existingApp != nil && existingApp.Pushed && !dataReq.Persistent {
				existingApp = nil
			}
		}
		// Only derive appPath from non-pushed installations; "pushed:<id>" is not a real app path.
		if existingApp != nil && existingApp.Path != nil && *existingApp.Path != "" &&
			!strings.HasPrefix(*existingApp.Path, "pushed:") {
			appPath, _ = securejoin.SecureJoin(s.DataDir, *existingApp.Path)
		}
	}

	// For pushed apps with a cached image and no new config/app being sent, skip
	// re-rendering and re-push the existing image directly.
	if dataReq.Persistent && existingApp != nil && existingApp.Pushed &&
		existingApp.Path != nil && strings.HasPrefix(*existingApp.Path, "pushed:") &&
		len(dataReq.Config) == 0 && dataReq.AppID == "" {
		cachedID := strings.TrimPrefix(*existingApp.Path, "pushed:")
		pushedImagePath, err := securejoin.SecureJoin(filepath.Join(s.DataDir, "webp", device.ID, "pushed"), cachedID+".webp")
		if err != nil {
			slog.Error("Failed to resolve pushed image path", "error", err)
			http.Error(w, "Image not found", http.StatusNotFound)
			return
		}
		imgBytes, err := os.ReadFile(pushedImagePath)
		if err != nil {
			// No cached image — fall through to the render path so the caller can
			// recover by providing an appID and config.
			slog.Warn("Cached pushed image missing, falling through to render", "path", pushedImagePath)
		} else {
			if !dataReq.Background {
				s.Broadcaster.Notify(device.ID, imgBytes)
			}
			if err := s.ensurePersistentPushedApp(r.Context(), device.ID, cachedID); err != nil {
				slog.Error("Error adding pushed app", "error", err)
			}
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte("App pushed.")); err != nil {
				slog.Error("Failed to write response", "error", err)
			}
			return
		}
	}

	// If we couldn't get appPath from installation, look it up by app_id
	if appPath == "" {
		if dataReq.AppID == "" {
			http.Error(w, "app_id is required when no valid installationID is provided", http.StatusBadRequest)
			return
		}

		// 1. Check System Apps
		for _, app := range s.ListSystemApps() {
			if app.ID == dataReq.AppID {
				appPath = filepath.Join(s.DataDir, app.Path)
				break
			}
		}

		// 2. Check User Apps
		if appPath == "" && user != nil {
			userApps := apps.ListUserApps(s.DataDir, user.Username)
			for _, app := range userApps {
				if app.ID == dataReq.AppID { // AppID for user apps is folder name
					appPath = filepath.Join(s.DataDir, app.Path)
					break
				}
			}
		}

		if appPath == "" {
			http.Error(w, "App not found", http.StatusNotFound)
			return
		}
	}

	imgBytes, _, err := s.RenderApp(r.Context(), device, existingApp, appPath, dataReq.Config)
	if err != nil {
		slog.Error("Failed to render app", "error", err)
		http.Error(w, "Rendering failed", http.StatusInternalServerError)
		return
	}

	if len(imgBytes) == 0 {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("Empty image, not pushing")); err != nil {
			slog.Error("Failed to write empty image response", "error", err)
		}
		return
	}

	if dataReq.Persistent && installationID != "" {
		// Ensure app record exists
		if err := s.ensurePersistentPushedApp(r.Context(), device.ID, installationID); err != nil {
			slog.Error("Failed to ensure pushed app", "error", err)
		}
	}

	// Notify device via Websocket only if this is a foreground push
	sent := false
	if !dataReq.Background {
		sent = s.Broadcaster.Notify(device.ID, imgBytes)
	}

	if !sent || dataReq.Persistent {
		persistentID := ""
		coalesceID := dataReq.CoalesceID
		if dataReq.Persistent {
			persistentID = installationID
		} else if coalesceID == "" && installationID != "" {
			coalesceID = "show-now-" + installationID
		}
		if err := s.savePushedImage(device.ID, persistentID, coalesceID, imgBytes); err != nil {
			http.Error(w, "Failed to save image", http.StatusInternalServerError)
			return
		}
	}
	if dataReq.Persistent {
		s.diagnosticsEvents.add(device.ID, "persistent_push_created", "Persistent pushed frame created", installationID)
	} else {
		mutationID := strings.TrimSpace(dataReq.MutationID)
		if mutationID == "" {
			mutationID = newFrameRequestID()
		}
		if len(mutationID) > 64 {
			mutationID = mutationID[:64]
		}
		updates := data.Device{
			ActiveShowNowApp:   &installationID,
			ShowNowRestoreApp:  showNowRestoreID,
			LastMutationID:     mutationID,
			LastMutationResult: "show_now_queued",
			StateVersion:       device.StateVersion + 1,
		}
		if _, err := gorm.G[data.Device](s.DB).Where("id = ?", device.ID).
			Select("ActiveShowNowApp", "ShowNowRestoreApp", "LastMutationID", "LastMutationResult", "StateVersion").
			Updates(r.Context(), updates); err != nil {
			_, _ = s.clearTemporaryPushFiles(device.ID)
			writeAPIError(w, http.StatusInternalServerError, "show_now_state_failed", "Show Now could not be queued safely", nil)
			return
		}
		updatedDevice := *device
		updatedDevice.StateVersion = updates.StateVersion
		updatedDevice.LastMutationID = mutationID
		recordDeviceInvalidation(&updatedDevice, device.StateVersion, "show_now", mutationID)
		s.diagnosticsEvents.add(device.ID, "temporary_push_created", "Temporary Show Now frame created", installationID)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "mutationID": mutationID, "activeApp": installationID,
			"restoreTarget": optionalString(showNowRestoreID), "stateVersion": updates.StateVersion,
		})
		return
	}

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("App pushed.")); err != nil {
		slog.Error("Failed to write response", "error", err)
	}
}

func (s *Server) handleGetDevice(w http.ResponseWriter, r *http.Request) {
	device := GetDevice(r)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(s.toDevicePayload(device)); err != nil {
		slog.Error("Failed to encode device JSON", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *Server) handleListInstallations(w http.ResponseWriter, r *http.Request) {
	device := GetDevice(r)

	installations := make([]AppPayload, 0, len(device.Apps))
	for i := range device.Apps {
		if device.Apps[i].Pushed {
			continue
		}
		installations = append(installations, s.toAppPayload(device, device.Apps[i]))
	}

	response := map[string]any{
		"installations": installations,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		slog.Error("Failed to encode installations JSON", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *Server) handleGetInstallation(w http.ResponseWriter, r *http.Request) {
	iname := r.PathValue("iname")

	device := GetDevice(r)

	app := device.GetApp(iname)
	if app == nil || app.Pushed {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(s.toAppPayload(device, app)); err != nil {
		slog.Error("Failed to encode app JSON", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// PushData represents the data for pushing an image to a device.
type PushData struct {
	InstallationID    string `json:"installationID"`
	InstallationIDAlt string `json:"installationId"`
	CoalesceID        string `json:"coalesceID"`
	Image             string `json:"image"`
	Background        bool   `json:"background"`
	Persistent        bool   `json:"persistent"`
}

func (s *Server) handlePushImage(w http.ResponseWriter, r *http.Request) {
	device := GetDevice(r)

	var dataReq PushData
	if err := json.NewDecoder(r.Body).Decode(&dataReq); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	installID := dataReq.InstallationID
	if installID == "" {
		installID = dataReq.InstallationIDAlt
	}

	imgBytes, err := base64.StdEncoding.DecodeString(dataReq.Image)
	if err != nil {
		http.Error(w, "Invalid Base64 Image", http.StatusBadRequest)
		return
	}

	if dataReq.Persistent && installID != "" {
		if err := s.ensurePersistentPushedApp(r.Context(), device.ID, installID); err != nil {
			slog.Error("Error adding pushed app", "error", err)
		}
	}

	// Notify device via Websocket only if this is a foreground push
	sent := false
	if !dataReq.Background {
		sent = s.Broadcaster.Notify(device.ID, imgBytes)
	}

	if !sent || dataReq.Persistent {
		persistentID := ""
		coalesceID := dataReq.CoalesceID
		if dataReq.Persistent {
			persistentID = installID
		} else if coalesceID == "" && installID != "" {
			coalesceID = "show-now-" + installID
		}
		if err := s.savePushedImage(device.ID, persistentID, coalesceID, imgBytes); err != nil {
			http.Error(w, fmt.Sprintf("Failed to save image: %v", err), http.StatusInternalServerError)
			return
		}
	}
	if dataReq.Persistent {
		s.diagnosticsEvents.add(device.ID, "persistent_push_created", "Persistent pushed frame created", installID)
	} else {
		s.diagnosticsEvents.add(device.ID, "temporary_push_created", "Temporary pushed frame created", installID)
	}

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("WebP received.")); err != nil {
		slog.Error("Failed to write WebP received message", "error", err)
		// Non-fatal, response already 200
	}
}

func (s *Server) savePushedImage(deviceID, installID, coalesceID string, data []byte) error {
	dir, err := s.ensureDeviceImageDir(deviceID)
	if err != nil {
		return fmt.Errorf("failed to get device webp directory: %w", err)
	}

	dir = filepath.Join(dir, "pushed")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	var filename string
	if installID != "" {
		// Image push with installID: stable filename, always replaces
		filename = installID + ".webp"
	} else if coalesceID != "" {
		// Validate coalesceID to prevent path traversal and suffix collisions.
		if len(coalesceID) > 64 {
			return fmt.Errorf("coalesceID exceeds maximum length of 64 characters")
		}
		for _, r := range coalesceID {
			isValid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
			if !isValid {
				return fmt.Errorf("coalesceID contains invalid characters (only alphanumeric, underscore, and dash allowed)")
			}
		}

		// Coalesced push: delete existing file with same coalesceID, then save.
		// At most 1 pending push per coalesceID.
		// Filename format: __{timestamp}_{coalesceID}.webp
		// Extract coalesceID by splitting at the first underscore after the "__" prefix.
		if entries, err := os.ReadDir(dir); err == nil {
			for _, entry := range entries {
				name := entry.Name()
				if !entry.IsDir() && strings.HasPrefix(name, "__") && strings.HasSuffix(name, ".webp") {
					inner := name[2 : len(name)-5] // strip "__" and ".webp"
					if _, fileCoalesceID, found := strings.Cut(inner, "_"); found {
						if fileCoalesceID == coalesceID {
							if err := os.Remove(filepath.Join(dir, name)); err != nil {
								slog.Warn("Failed to remove coalesced push", "name", name, "error", err)
							}
						}
					}
				}
			}
		}
		filename = fmt.Sprintf("__%d_%s.webp", time.Now().UnixNano(), coalesceID)
	} else {
		// Anonymous push: unbounded ephemeral queue
		filename = fmt.Sprintf("__%d.webp", time.Now().UnixNano())
	}

	path, err := securejoin.SecureJoin(dir, filename)
	if err != nil {
		return err
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return err
	}

	// Clean up anonymous ephemeral files older than 24 hours.
	// Only runs when saving an anonymous push (coalesced and installID-based
	// pushes are already bounded). Runs in a background goroutine to avoid
	// blocking the HTTP response.
	if installID == "" && coalesceID == "" {
		go func() {
			cutoff := time.Now().UnixNano() - 24*int64(time.Hour)
			if entries, readErr := os.ReadDir(dir); readErr == nil {
				for _, entry := range entries {
					name := entry.Name()
					if !entry.IsDir() && strings.HasPrefix(name, "__") && strings.HasSuffix(name, ".webp") {
						// Anonymous pushes: __{nanos}.webp (no underscore between __ and .webp suffix)
						inner := name[2 : len(name)-5]
						if strings.Contains(inner, "_") {
							continue // coalesced push, not anonymous
						}
						if ts, parseErr := strconv.ParseInt(inner, 10, 64); parseErr == nil && ts < cutoff {
							if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
								slog.Warn("Failed to remove expired ephemeral image", "name", name, "error", err)
							}
						}
					}
				}
			}
		}()
	}

	return nil
}

func (s *Server) ensurePersistentPushedApp(ctx context.Context, deviceID, installID string) error {
	// Check if app exists by matching on installID (for pushed apps, we need to look up by installID)
	// Since installID might be non-numeric (e.g., "pushed:hasssolarlocal1"), we check via path/file
	count, err := gorm.G[data.App](s.DB).Where("device_id = ? AND pushed = ? AND path = ?", deviceID, true, "pushed:"+installID).Count(ctx, "*")
	if err != nil {
		slog.Error("Failed to check if app exists for image push", "error", err)
		return err
	}
	if count > 0 {
		return nil
	}

	// Generate a numeric iname for the pushed app (same as regular apps)
	newIname, err := generateUniqueIname(s.DB, deviceID)
	if err != nil {
		slog.Error("Failed to generate iname for pushed app", "error", err)
		return err
	}

	// Store installID in path so we can match on it later
	installPath := "pushed:" + installID

	newApp := data.App{
		DeviceID:    deviceID,
		Iname:       newIname,
		Name:        "pushed",
		UInterval:   10,
		DisplayTime: 0,
		Enabled:     true,
		Pushed:      true,
		PushKind:    "persistent",
		Path:        &installPath,
	}

	maxOrder, err := getMaxAppOrder(s.DB, deviceID)
	if err != nil {
		slog.Error("Failed to get max app order", "error", err)
		// Non-fatal, default to 0 for order (if maxOrder is 0)
	}
	newApp.Order = maxOrder + 1

	return gorm.G[data.App](s.DB).Create(ctx, &newApp)
}

func (s *Server) handlePatchDevice(w http.ResponseWriter, r *http.Request) {
	// Auth handled by middleware, get device
	device := GetDevice(r)
	unlock := s.lockDevicePoll(device.ID)
	defer unlock()
	fresh, err := gorm.G[data.Device](s.DB).Preload("Apps", orderedAppsPreload).Where("id = ?", device.ID).First(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "device_reload_failed", "Device state could not be refreshed", nil)
		return
	}
	device = &fresh
	previousVersion := device.StateVersion

	var update DeviceUpdate
	if !decodeAPIJSON(w, r, &update) {
		return
	}
	if update.ExpectedStateVersion != nil && *update.ExpectedStateVersion != device.StateVersion {
		writeAPIError(w, http.StatusConflict, "stale_state", "Device state changed before this update was applied", nil)
		return
	}
	if update.Brightness != nil && (*update.Brightness < 0 || *update.Brightness > 100) {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_brightness", "Brightness must be between 0 and 100", nil)
		return
	}

	previousBrightness := int(device.Brightness)
	locationChanged := false
	if update.Location != nil {
		if err := validateDeviceLocation(*update.Location); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		locationChanged = device.Location != *update.Location
		device.Location = *update.Location
		if update.Location.Timezone == "" {
			device.Timezone = nil
		} else {
			tz := update.Location.Timezone
			device.Timezone = &tz
		}
	}
	if update.Brightness != nil {
		device.Brightness = data.Brightness(*update.Brightness)
	}
	if update.Sleeping != nil && *update.Sleeping != device.Sleeping {
		if err := s.applySleepState(r.Context(), device, *update.Sleeping); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "sleep_transition_failed", "Display sleep state could not be changed", nil)
			return
		}
	}
	if update.Sleeping != nil && !*update.Sleeping {
		if err := s.restoreDisplayAfterPowerOn(r.Context(), device); err != nil {
			slog.Warn("Failed to restore normal rotation after wake", "device", device.ID, "error", err)
		}
	}
	if update.IntervalSec != nil {
		device.DefaultInterval = *update.IntervalSec
	}
	nightModeWasEnabled := device.NightModeEnabled
	nightStartWas := device.NightStart
	nightEndWas := device.NightEnd
	dimModeWasEnabled := device.DimModeEnabled
	var dimTimeWas string
	if device.DimTime != nil {
		dimTimeWas = *device.DimTime
	}
	if update.NightModeEnabled != nil {
		device.NightModeEnabled = *update.NightModeEnabled
	}
	if update.AutoDim != nil {
		device.NightModeEnabled = *update.AutoDim
	}
	if update.NightModeApp != nil {
		if *update.NightModeApp != "" {
			if device.GetApp(*update.NightModeApp) == nil {
				http.Error(w, "Night mode app not found", http.StatusBadRequest)
				return
			}
		}
		device.NightModeApp = *update.NightModeApp
	}
	if update.NightModeBrightness != nil {
		device.NightBrightness = data.Brightness(*update.NightModeBrightness)
	}
	if update.PinnedApp != nil {
		if *update.PinnedApp != "" {
			if device.GetApp(*update.PinnedApp) == nil {
				http.Error(w, "Pinned app not found", http.StatusBadRequest)
				return
			}
		}
		if *update.PinnedApp == "" {
			device.PinnedApp = nil
		} else {
			device.PinnedApp = update.PinnedApp
		}
	}

	if update.NightModeStartTime != nil {
		device.NightStart = *update.NightModeStartTime
	}
	if update.NightModeEndTime != nil {
		device.NightEnd = *update.NightModeEndTime
	}
	if update.DimModeStartTime != nil {
		device.DimTime = update.DimModeStartTime
	}
	if update.DimModeEnabled != nil {
		device.DimModeEnabled = *update.DimModeEnabled
	}
	if update.DimModeBrightness != nil {
		val := data.Brightness(*update.DimModeBrightness)
		device.DimBrightness = &val
	}

	if !device.NightModeEnabled || nightModeWasEnabled != device.NightModeEnabled || nightStartWas != device.NightStart || nightEndWas != device.NightEnd {
		clearNightModeOverride(device)
	}
	if update.NightModeActive != nil {
		if _, err := setNightModeOverride(device, *update.NightModeActive); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	currentDimTime := ""
	if device.DimTime != nil {
		currentDimTime = *device.DimTime
	}
	if !device.DimModeEnabled || dimModeWasEnabled != device.DimModeEnabled || dimTimeWas != currentDimTime {
		clearDimModeOverride(device)
	}
	if update.DimModeActive != nil {
		if _, err := setDimModeOverride(device, *update.DimModeActive); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	device.StateVersion++
	device.LastMutationID = strings.TrimSpace(update.MutationID)
	if len(device.LastMutationID) > 64 {
		device.LastMutationID = device.LastMutationID[:64]
	}
	if device.LastMutationID == "" {
		device.LastMutationID = newFrameRequestID()
	}
	device.LastMutationResult = "device_updated"
	if err := s.DB.Omit("Apps").Save(device).Error; err != nil {
		http.Error(w, "Failed to update device", http.StatusInternalServerError)
		return
	}
	recordDeviceInvalidation(device, previousVersion, "device_settings", device.LastMutationID)
	if locationChanged {
		invalidated := 0
		apps, err := gorm.G[data.App](s.DB).Where("device_id = ?", device.ID).Find(r.Context())
		if err != nil {
			slog.Warn("Failed to load apps for location invalidation", "device", device.ID, "error", err)
			apps = nil
		}
		for _, app := range apps {
			if !app.Pushed && appUsesDeviceLocation(&app) {
				if _, err := gorm.G[data.App](s.DB).Where("id = ?", app.ID).Select("LastRender", "NextRenderAt").Updates(r.Context(), data.App{LastRender: time.Time{}, NextRenderAt: nil}); err != nil {
					slog.Warn("Failed to invalidate location-aware render", "device", device.ID, "installation_id", app.Iname, "error", err)
					continue
				}
				app.LastRender = time.Time{}
				app.NextRenderAt = nil
				invalidated++
			}
		}
		s.diagnosticsEvents.add(device.ID, "location_changed", fmt.Sprintf("Device location changed; %d app(s) invalidated", invalidated), "")
	}
	if update.Brightness != nil && previousBrightness != *update.Brightness {
		s.diagnosticsEvents.add(device.ID, "brightness_changed", fmt.Sprintf("Brightness changed from %d to %d", previousBrightness, *update.Brightness), "")
	}
	if update.Brightness != nil {
		switch {
		case previousBrightness > 0 && *update.Brightness == 0:
			if err := s.prepareDisplayRestore(r.Context(), device); err != nil {
				slog.Warn("Failed to prepare display restoration", "device", device.ID, "error", err)
			}
		case previousBrightness == 0 && *update.Brightness > 0:
			if err := s.restoreDisplayAfterPowerOn(r.Context(), device); err != nil {
				slog.Warn("Failed to restore display after power on", "device", device.ID, "error", err)
			}
		}
	}

	// Notify Dashboard
	user := GetUser(r)
	s.notifyDashboard(user.Username, WSEvent{Type: "apps_changed", DeviceID: device.ID})

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(s.toDevicePayload(device)); err != nil {
		slog.Error("Failed to encode device", "error", err)
	}
}

// InstallationUpdate represents the updatable fields for an app installation via API.
type InstallationUpdate struct {
	Enabled              *bool   `json:"enabled"`
	Pinned               *bool   `json:"pinned"`
	RenderIntervalMin    *int    `json:"renderIntervalMin"`
	DisplayTimeSec       *int    `json:"displayTimeSec"`
	ExpectedStateVersion *uint64 `json:"expectedStateVersion"`
	MutationID           string  `json:"mutationID"`

	// Schedule fields
	StartTime *string   `json:"startTime"`
	EndTime   *string   `json:"endTime"`
	Days      *[]string `json:"days"`

	// Recurrence fields
	UseCustomRecurrence *bool                `json:"useCustomRecurrence"`
	RecurrenceType      *data.RecurrenceType `json:"recurrenceType"`
	RecurrenceInterval  *int                 `json:"recurrenceInterval"`
	RecurrencePattern   *map[string]any      `json:"recurrencePattern"`
	RecurrenceStartDate *string              `json:"recurrenceStartDate"`
	RecurrenceEndDate   *string              `json:"recurrenceEndDate"`
}

func (s *Server) handlePatchInstallation(w http.ResponseWriter, r *http.Request) {
	iname := r.PathValue("iname")

	device := GetDevice(r)
	unlock := s.lockDevicePoll(device.ID)
	defer unlock()
	fresh, err := gorm.G[data.Device](s.DB).Preload("Apps", orderedAppsPreload).Where("id = ?", device.ID).First(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "device_reload_failed", "Device state could not be refreshed", nil)
		return
	}
	device = &fresh
	previousVersion := device.StateVersion

	app := device.GetApp(iname)
	if app == nil || app.Pushed {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}

	var update InstallationUpdate
	if !decodeAPIJSON(w, r, &update) {
		return
	}
	if update.ExpectedStateVersion != nil && *update.ExpectedStateVersion != device.StateVersion {
		writeAPIError(w, http.StatusConflict, "stale_state", "Device state changed before this app update was applied", nil)
		return
	}

	disabledActiveApp := false
	if update.Enabled != nil {
		if !*update.Enabled && app.Enabled {
			enabledCount, err := gorm.G[data.App](s.DB).Where("device_id = ? AND pushed = ? AND enabled = ? AND id <> ?", device.ID, false, true, app.ID).Count(r.Context(), "*")
			if err != nil {
				http.Error(w, "Failed to validate enabled apps", http.StatusInternalServerError)
				return
			}
			if enabledCount == 0 {
				writeAPIError(w, http.StatusConflict, "sole_enabled_app", "At least one app must remain enabled", nil)
				return
			}
		}
		app.Enabled = *update.Enabled
		if !app.Enabled {
			if optionalString(device.DisplayingApp) == app.Iname {
				disabledActiveApp = true
				device.DisplayingApp = nil
				device.LastAppIndex = expandedIndexForInstallation(device, app.Iname)
			}
			if optionalString(device.DisplayRestoreApp) == app.Iname {
				device.DisplayRestoreApp = nil
			}
			if optionalString(device.PinnedApp) == app.Iname {
				device.PinnedApp = nil
			}
		} else {
			// Reset LastRender when app is enabled
			app.LastRender = time.Time{}
		}
	}
	if update.RenderIntervalMin != nil {
		app.UInterval = *update.RenderIntervalMin
	}
	if update.DisplayTimeSec != nil {
		app.DisplayTime = *update.DisplayTimeSec
	}
	if update.Pinned != nil {
		if *update.Pinned {
			if !app.Enabled || app.EmptyLastRender || app.LastRenderResult == "hidden" || app.LastRenderResult == "failure" || app.LastRenderResult == "upstream_failure" {
				writeAPIError(w, http.StatusConflict, "app_not_restorable", "A disabled, hidden, or failing app cannot be pinned", nil)
				return
			}
			device.PinnedApp = &app.Iname
		} else if device.PinnedApp != nil && *device.PinnedApp == app.Iname {
			device.PinnedApp = nil
		}
	}

	// Schedule fields
	if update.StartTime != nil {
		if *update.StartTime == "" {
			app.StartTime = nil
		} else {
			parsed, err := parseTimeInput(*update.StartTime)
			if err != nil {
				http.Error(w, fmt.Sprintf("Invalid startTime: %v", err), http.StatusBadRequest)
				return
			}
			app.StartTime = &parsed
		}
	}
	if update.EndTime != nil {
		if *update.EndTime == "" {
			app.EndTime = nil
		} else {
			parsed, err := parseTimeInput(*update.EndTime)
			if err != nil {
				http.Error(w, fmt.Sprintf("Invalid endTime: %v", err), http.StatusBadRequest)
				return
			}
			app.EndTime = &parsed
		}
	}
	if update.Days != nil {
		for _, day := range *update.Days {
			switch day {
			case "monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday":
				// valid
			default:
				http.Error(w, fmt.Sprintf("Invalid day: %s", day), http.StatusBadRequest)
				return
			}
		}
		app.Days = *update.Days
	}

	// Recurrence fields
	if update.UseCustomRecurrence != nil {
		app.UseCustomRecurrence = *update.UseCustomRecurrence
	}
	if update.RecurrenceType != nil {
		switch *update.RecurrenceType {
		case data.RecurrenceDaily, data.RecurrenceWeekly, data.RecurrenceMonthly, data.RecurrenceYearly:
			app.RecurrenceType = *update.RecurrenceType
		default:
			http.Error(w, "Invalid recurrenceType", http.StatusBadRequest)
			return
		}
	}
	if update.RecurrenceInterval != nil {
		app.RecurrenceInterval = *update.RecurrenceInterval
	}
	if update.RecurrencePattern != nil {
		app.RecurrencePattern = *update.RecurrencePattern
	}
	if update.RecurrenceStartDate != nil {
		if *update.RecurrenceStartDate == "" {
			app.RecurrenceStartDate = nil
		} else {
			if _, err := time.Parse("2006-01-02", *update.RecurrenceStartDate); err != nil {
				http.Error(w, "Invalid recurrenceStartDate: must be YYYY-MM-DD", http.StatusBadRequest)
				return
			}
			app.RecurrenceStartDate = update.RecurrenceStartDate
		}
	}
	if update.RecurrenceEndDate != nil {
		if *update.RecurrenceEndDate == "" {
			app.RecurrenceEndDate = nil
		} else {
			if _, err := time.Parse("2006-01-02", *update.RecurrenceEndDate); err != nil {
				http.Error(w, "Invalid recurrenceEndDate: must be YYYY-MM-DD", http.StatusBadRequest)
				return
			}
			app.RecurrenceEndDate = update.RecurrenceEndDate
		}
	}

	mutationID := strings.TrimSpace(update.MutationID)
	if mutationID == "" {
		mutationID = newFrameRequestID()
	}
	if len(mutationID) > 64 {
		mutationID = mutationID[:64]
	}
	device.StateVersion++
	device.LastMutationID = mutationID
	device.LastMutationResult = "installation_updated"
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(app).Error; err != nil {
			return err
		}
		return tx.Omit("Apps").Save(device).Error
	}); err != nil {
		http.Error(w, "Failed to update app", http.StatusInternalServerError)
		return
	}
	recordDeviceInvalidation(device, previousVersion, "installation_settings", mutationID)
	if disabledActiveApp && !device.Sleeping {
		if err := s.restoreDisplayAfterPowerOn(r.Context(), device); err != nil {
			slog.Warn("Failed to advance display after disabling active app", "device", device.ID, "installation", app.Iname, "error", err)
		}
	}

	// Notify Dashboard
	user := GetUser(r)
	s.notifyDashboard(user.Username, WSEvent{Type: "apps_changed", DeviceID: device.ID})

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(s.toAppPayload(device, app)); err != nil {
		slog.Error("Failed to encode app", "error", err)
	}
}

func (s *Server) handleDeleteInstallationAPI(w http.ResponseWriter, r *http.Request) {
	installID := filepath.Base(r.PathValue("iname"))

	device := GetDevice(r)
	unlock := s.lockDevicePoll(device.ID)
	defer unlock()
	fresh, reloadErr := gorm.G[data.Device](s.DB).Preload("Apps", orderedAppsPreload).Where("id = ?", device.ID).First(r.Context())
	if reloadErr != nil {
		writeAPIError(w, http.StatusInternalServerError, "device_reload_failed", "Device state could not be refreshed", nil)
		return
	}
	device = &fresh

	// First try to find the app by iname (server-generated ID)
	foundByPushedPath := false
	app, err := gorm.G[data.App](s.DB).Where("device_id = ? AND iname = ?", device.ID, installID).First(r.Context())
	if err != nil {
		// If not found by iname, try to find by installationID (stored in path as "pushed:{installationID}")
		app, err = gorm.G[data.App](s.DB).Where("device_id = ? AND path = ?", device.ID, "pushed:"+installID).First(r.Context())
		if err != nil {
			http.Error(w, "App not found", http.StatusNotFound)
			return
		}
		foundByPushedPath = true
	}
	if app.Pushed && !foundByPushedPath {
		writeAPIError(w, http.StatusUnprocessableEntity, "temporary_installation", "Temporary pushed content cannot be deleted as an installation", nil)
		return
	}
	if !app.Pushed && app.Enabled {
		enabledCount, countErr := gorm.G[data.App](s.DB).Where("device_id = ? AND pushed = ? AND enabled = ? AND id <> ?", device.ID, false, true, app.ID).Count(r.Context(), "*")
		if countErr != nil {
			http.Error(w, "Failed to validate enabled apps", http.StatusInternalServerError)
			return
		}
		if enabledCount == 0 {
			http.Error(w, "At least one restorable app must remain enabled", http.StatusConflict)
			return
		}
	}
	var deviceFields []string
	if device.PinnedApp != nil && *device.PinnedApp == app.Iname {
		device.PinnedApp = nil
		deviceFields = append(deviceFields, "PinnedApp")
	}
	if device.DisplayingApp != nil && *device.DisplayingApp == app.Iname {
		device.DisplayingApp = nil
		deviceFields = append(deviceFields, "DisplayingApp")
	}
	if device.DisplayRestoreApp != nil && *device.DisplayRestoreApp == app.Iname {
		device.DisplayRestoreApp = nil
		deviceFields = append(deviceFields, "DisplayRestoreApp")
	}
	if device.NightModeApp == app.Iname {
		device.NightModeApp = ""
		deviceFields = append(deviceFields, "NightModeApp")
	}
	previousVersion := device.StateVersion
	mutationID := newFrameRequestID()
	device.StateVersion++
	device.LastMutationID, device.LastMutationResult = mutationID, "installation_deleted"
	deviceFields = append(deviceFields, "StateVersion", "LastMutationID", "LastMutationResult")
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&data.App{}, app.ID).Error; err != nil {
			return err
		}
		if len(deviceFields) == 0 {
			return nil
		}
		err := tx.WithContext(r.Context()).Model(&data.Device{}).Where("id = ?", device.ID).
			Select(deviceFields).Updates(data.Device{
			PinnedApp:          device.PinnedApp,
			DisplayingApp:      device.DisplayingApp,
			DisplayRestoreApp:  device.DisplayRestoreApp,
			NightModeApp:       device.NightModeApp,
			StateVersion:       device.StateVersion,
			LastMutationID:     device.LastMutationID,
			LastMutationResult: device.LastMutationResult,
		}).Error
		return err
	}); err != nil {
		http.Error(w, "Failed to delete app", http.StatusInternalServerError)
		return
	}
	recordDeviceInvalidation(device, previousVersion, "installation_deleted", mutationID)

	// Clean up files using the actual iname
	webpDir, err := s.ensureDeviceImageDir(device.ID)
	if err != nil {
		slog.Error("Failed to get device webp directory for app delete cleanup", "device_id", device.ID, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	// Clean up pushed app image if applicable
	if app.Pushed && app.Path != nil && len(*app.Path) > 7 && (*app.Path)[:7] == "pushed:" {
		pushedID := (*app.Path)[7:]
		pushedWebpPath := filepath.Join(webpDir, "pushed", pushedID+".webp")
		if err := os.Remove(pushedWebpPath); err != nil && !os.IsNotExist(err) {
			slog.Error("Failed to remove pushed webp file", "path", pushedWebpPath, "error", err)
		}
	}

	matches, _ := filepath.Glob(filepath.Join(webpDir, fmt.Sprintf("*-%s.webp", app.Iname)))
	for _, match := range matches {
		if err := os.Remove(match); err != nil {
			slog.Error("Failed to remove webp file", "path", match, "error", err)
		}
	}

	// Notify Dashboard
	user := GetUser(r)
	s.notifyDashboard(user.Username, WSEvent{Type: "apps_changed", DeviceID: device.ID})

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("App deleted.")); err != nil {
		slog.Error("Failed to write response", "error", err)
	}
}

func (s *Server) handleRebootDeviceAPI(w http.ResponseWriter, r *http.Request) {
	device := GetDevice(r)

	if err := s.sendRebootCommand(r.Context(), device.ID); err != nil {
		slog.Error("Failed to send reboot command", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("Reboot command sent.")); err != nil {
		slog.Error("Failed to write response", "error", err)
	}
}

// FirmwareSettingsUpdate represents the updatable firmware settings via API.
type FirmwareSettingsUpdate struct {
	SkipDisplayVersion *bool   `json:"skipDisplayVersion"`
	SkipBootAnimation  *bool   `json:"skipBootAnimation"`
	PreferIPv6         *bool   `json:"preferIPv6"`
	APMode             *bool   `json:"apMode"`
	SwapColors         *bool   `json:"swapColors"`
	WifiPowerSave      *int    `json:"wifiPowerSave"`
	ImageURL           *string `json:"imageUrl"`
	Hostname           *string `json:"hostname"`
	SNTPServer         *string `json:"sntpServer"`
	SyslogAddr         *string `json:"syslogAddr"`
}

func (s *Server) handleUpdateFirmwareSettingsAPI(w http.ResponseWriter, r *http.Request) {
	device := GetDevice(r)

	var update FirmwareSettingsUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	payload := make(map[string]any)

	if update.SkipDisplayVersion != nil {
		payload["skip_display_version"] = *update.SkipDisplayVersion
	}
	if update.SkipBootAnimation != nil {
		payload["skip_boot_animation"] = *update.SkipBootAnimation
	}
	if update.PreferIPv6 != nil {
		payload["prefer_ipv6"] = *update.PreferIPv6
	}
	if update.APMode != nil {
		payload["ap_mode"] = *update.APMode
	}
	if update.SwapColors != nil {
		payload["swap_colors"] = *update.SwapColors
	}
	if update.WifiPowerSave != nil {
		payload["wifi_power_save"] = *update.WifiPowerSave
	}
	if update.ImageURL != nil {
		payload["image_url"] = *update.ImageURL
	}
	if update.Hostname != nil {
		payload["hostname"] = *update.Hostname
	}
	if update.SNTPServer != nil {
		payload["sntp_server"] = *update.SNTPServer
	}
	if update.SyslogAddr != nil {
		payload["syslog_addr"] = *update.SyslogAddr
	}

	if len(payload) == 0 {
		http.Error(w, "No settings provided", http.StatusBadRequest)
		return
	}

	if err := s.sendFirmwareSettingsCommand(r.Context(), device.ID, payload); err != nil {
		slog.Error("Failed to send firmware settings command", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("Firmware settings updated.")); err != nil {
		slog.Error("Failed to write response", "error", err)
	}
}

func (s *Server) SetupAPIRoutes() {
	s.Router.Handle("POST /v0/pairing/redeem", http.HandlerFunc(s.handleRedeemPairingCode))
	s.Router.Handle("GET /v0/capabilities", s.CatalogueAuthMiddleware(http.HandlerFunc(s.handleCapabilities)))
	// API v0 Group - authenticated with Middleware
	s.Router.Handle("GET /v0/catalogue", s.CatalogueAuthMiddleware(http.HandlerFunc(s.handleCatalogueList)))
	s.Router.Handle("GET /v0/catalogue/{appID}", s.CatalogueAuthMiddleware(http.HandlerFunc(s.handleCatalogueDetail)))
	s.Router.Handle("GET /v0/catalogue/{appID}/schema", s.CatalogueAuthMiddleware(http.HandlerFunc(s.handleCatalogueSchema)))
	s.Router.Handle("GET /v0/catalogue/{appID}/icon", s.CatalogueAuthMiddleware(http.HandlerFunc(s.handleCatalogueIcon)))
	s.Router.Handle("GET /v0/devices", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.handleListDevices)))
	s.Router.Handle("GET /v0/devices/{id}", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleGetDevice))))
	s.Router.Handle("GET /v0/devices/{id}/preview", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleMobilePreview))))
	s.Router.Handle("GET /v0/devices/{id}/installations/{iname}/preview", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleInstallationPreview))))
	s.Router.Handle("GET /v0/devices/{id}/diagnostics", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleDeviceDiagnostics))))
	s.Router.Handle("POST /v0/devices/{id}/diagnostics/clear-temporary", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleClearTemporaryDisplay))))
	s.Router.Handle("POST /v0/devices/{id}/diagnostics/restore-rotation", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleRestoreNormalRotation))))
	s.Router.Handle("POST /v0/devices/{id}/installations/{iname}/retry-render", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleRetryInstallationRender))))
	s.Router.Handle("POST /v0/devices/{id}/push", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handlePushImage))))
	s.Router.Handle("POST /v0/devices/{id}/push_app", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handlePushApp))))
	s.Router.Handle("POST /v0/devices/{id}/update_firmware_settings", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleUpdateFirmwareSettingsAPI))))
	s.Router.Handle("POST /v0/devices/{id}/reboot", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleRebootDeviceAPI))))
	s.Router.Handle("GET /v0/devices/{id}/installations", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleListInstallations))))
	s.Router.Handle("GET /v0/devices/{id}/installations/{iname}", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleGetInstallation))))
	s.Router.Handle("POST /v0/devices/{id}/installations", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleInstallationCreate))))
	s.Router.Handle("PATCH /v0/devices/{id}/installations/order", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleInstallationOrderPatch))))
	s.Router.Handle("GET /v0/devices/{id}/installations/{installationID}/config", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleInstallationConfigGet))))
	s.Router.Handle("PATCH /v0/devices/{id}/installations/{installationID}/config", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleInstallationConfigPatch))))
	s.Router.Handle("PATCH /v0/devices/{id}", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handlePatchDevice))))
	s.Router.Handle("PATCH /v0/devices/{id}/installations/{iname}", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handlePatchInstallation))))
	s.Router.Handle("DELETE /v0/devices/{id}/installations/{iname}", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleDeleteInstallationAPI))))
	s.Router.Handle("POST /v0/household/members", s.APIAuthMiddleware(s.RequireOwnerAPI(s.handleCreateHouseholdMember)))
	s.Router.Handle("POST /v0/household/members/{memberID}/pairing-codes", s.APIAuthMiddleware(s.RequireOwnerAPI(s.handleCreatePairingCode)))
	s.Router.Handle("DELETE /v0/mobile-sessions/{sessionID}", s.APIAuthMiddleware(s.RequireOwnerAPI(s.handleRevokeMobileSession)))
	s.Router.Handle("GET /v0/devices/{id}/market/search", s.APIAuthMiddleware(s.RequireMobileControlAPI(s.RequireDevice(s.handleMarketSearch))))
	s.Router.Handle("PUT /v0/provider-credentials/{credentialID}", s.APIAuthMiddleware(s.RequireOwnerAPI(s.handlePutProviderCredential)))
	s.Router.Handle("GET /v0/provider-credentials/{credentialID}", s.APIAuthMiddleware(s.RequireOwnerAPI(s.handleGetProviderCredentialMetadata)))
	s.Router.Handle("GET /v0/provider-credentials", s.APIAuthMiddleware(s.RequireOwnerAPI(s.handleListProviderCredentials)))
	s.Router.Handle("PATCH /v0/provider-credentials/{credentialID}", s.APIAuthMiddleware(s.RequireOwnerAPI(s.handlePatchProviderCredential)))
	s.Router.Handle("DELETE /v0/provider-credentials/{credentialID}", s.APIAuthMiddleware(s.RequireOwnerAPI(s.handleDeleteProviderCredential)))
	s.Router.Handle("POST /v0/provider-credentials/{credentialID}/validate", s.APIAuthMiddleware(s.RequireOwnerAPI(s.handleValidateProviderCredential)))
	s.Router.Handle("GET /v0/household/members", s.APIAuthMiddleware(s.RequireOwnerAPI(s.handleListHouseholdMembers)))
	s.Router.Handle("PATCH /v0/household/members/{memberID}", s.APIAuthMiddleware(s.RequireOwnerAPI(s.handlePatchHouseholdMember)))
	s.Router.Handle("POST /v0/devices/{id}/starter-bundles/{bundleID}", s.APIAuthMiddleware(s.RequireOwnerAPI(s.RequireDevice(s.handleInstallStarterBundle))))
}
