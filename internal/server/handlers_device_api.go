package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"tronbyt-server/internal/data"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func newFrameRequestID() string {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(value[:])
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// handleNextApp is the handler for GET /{id}/next.
func (s *Server) handleNextApp(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := newFrameRequestID()
	s.metrics.devicePolls.Inc()
	id := r.PathValue("id")

	var device *data.Device
	if d, err := DeviceFromContext(r.Context()); err == nil {
		device = d
	} else if u, err := UserFromContext(r.Context()); err == nil {
		for i := range u.Devices {
			if u.Devices[i].ID == id {
				device = &u.Devices[i]
				break
			}
		}
	} else {
		// Fallback: Fetch from DB directly (No Auth required for device operation)
		d, err := gorm.G[data.Device](s.DB).Preload("Apps", nil).Where("id = ?", id).First(r.Context())
		if err == nil {
			device = &d
		}
	}

	if device == nil {
		slog.Info("HTTP frame request", "request_id", requestID, "device_id", id, "authenticated", false, "status", http.StatusNotFound, "duration_ms", time.Since(started).Milliseconds())
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	if device.RequireAPIKey {
		if key := extractDeviceKey(r); key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(device.APIKey)) != 1 {
			s.diagnosticsEvents.add(id, "authentication_failure", "HTTP frame authentication rejected", "")
			slog.Warn("HTTP frame request", "request_id", requestID, "device_id", id, "authenticated", false, "status", http.StatusUnauthorized, "duration_ms", time.Since(started).Milliseconds())
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	unlockPoll := s.lockDevicePoll(device.ID)
	defer unlockPoll()
	if fresh, err := gorm.G[data.Device](s.DB).Preload("Apps", nil).Where("id = ?", device.ID).First(r.Context()); err == nil {
		device = &fresh
	}

	if len(device.Apps) == 0 {
		reloaded, err := gorm.G[data.Device](s.DB).Preload("Apps", nil).Where("id = ?", device.ID).First(r.Context())
		if err == nil {
			device = &reloaded
		}
	}

	user, _ := UserFromContext(r.Context())
	if user == nil {
		owner, err := gorm.G[data.User](s.DB).Where("username = ?", device.Username).First(r.Context())
		if err != nil {
			slog.Error("Failed to find device owner", "username", device.Username, "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		user = &owner
	}

	// Update device info if needed
	// We use a transaction with locking to avoid race conditions with other requests
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		// Lock the row to ensure we read the latest state and no one else updates it
		freshDevice, err := gorm.G[data.Device](tx, clause.Locking{Strength: "UPDATE"}).Where("id = ?", device.ID).First(r.Context())
		if err != nil {
			return err
		}

		updated := false
		if freshDevice.Info.ProtocolType != data.ProtocolHTTP {
			slog.Debug("Updating protocol_type to HTTP on /next request", "device", device.ID)
			freshDevice.Info.ProtocolType = data.ProtocolHTTP
			updated = true
		}

		// Check for firmware version header
		if fwVersion := r.Header.Get("X-Firmware-Version"); fwVersion != "" {
			if freshDevice.Info.FirmwareVersion != fwVersion {
				slog.Debug("Updating firmware_version on /next request", "device", device.ID, "version", fwVersion)
				freshDevice.Info.FirmwareVersion = fwVersion
				updated = true
			}
		}

		if updated {
			if _, err := gorm.G[data.Device](tx).Where("id = ?", freshDevice.ID).Update(r.Context(), "info", freshDevice.Info); err != nil {
				return err
			}
			// Update the in-memory device object so subsequent logic uses the new values
			device.Info = freshDevice.Info
		}
		return nil
	})

	if err != nil {
		slog.Error("Failed to update device info transaction", "device", device.ID, "error", err)
	}

	lastIndexBefore := device.LastAppIndex
	displayingBefore := optionalString(device.DisplayingApp)
	restoreBefore := optionalString(device.DisplayRestoreApp)
	trace := &frameSelectionTrace{}
	ctx := withFrameSelectionTrace(r.Context(), trace)
	imgData, app, err := s.GetNextAppImage(ctx, device, user)
	if err != nil {
		// Send default image if error (or not found)
		slog.Error("Failed to get next app image", "device", device.ID, "error", err)
		s.sendDefaultImage(w, r, device)
		return
	}

	// For HTTP devices, we assume "Sent" equals "Displaying" (or roughly so).
	// We update DisplayingApp here so the Preview uses the explicit field instead of fallback.
	if app != nil {
		if _, err := gorm.G[data.Device](s.DB).Where("id = ?", device.ID).Update(r.Context(), "displaying_app", app.Iname); err != nil {
			slog.Error("Failed to update displaying_app for HTTP device", "device", device.ID, "error", err)
		}
		device.DisplayingApp = &app.Iname
	}
	now := time.Now()
	if _, err := gorm.G[data.Device](s.DB).Where("id = ?", device.ID).Update(r.Context(), "last_seen", &now); err == nil {
		device.LastSeen = &now
	}

	// Send Headers
	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")

	s.sendPendingHTTPDeviceHeaders(w, r.Context(), device)

	// Determine Brightness
	brightness := device.GetEffectiveBrightness()
	w.Header().Set("Tronbyt-Brightness", fmt.Sprintf("%d", brightness))

	dwell := effectiveFrameDwell(now, device.GetEffectiveDwellTime(app), app)
	w.Header().Set("Tronbyt-Dwell-Secs", fmt.Sprintf("%d", dwell))
	if app != nil {
		w.Header().Set("Tronbyt-App", app.Name)
		w.Header().Set("Tronbyt-Installation", app.Iname)
	} else if trace.InstallationID != "" {
		w.Header().Set("Tronbyt-Installation", trace.InstallationID)
	}

	if _, err := w.Write(imgData); err != nil {
		slog.Error("Failed to write image data to response", "error", err)
		// Log error, but can't change HTTP status after writing headers.
	}
	hash := sha256.Sum256(imgData)
	appID, iname, renderResult, renderMessage := "", "", "", ""
	if app != nil {
		appID, iname = app.Name, app.Iname
		renderResult, renderMessage = app.LastRenderResult, app.LastRenderMessage
	} else if trace.InstallationID != "" {
		iname = trace.InstallationID
	}
	s.pollDiagnostics.Store(device.ID, pollDiagnostic{
		LastSuccess: now.UTC(), LatencyMS: time.Since(started).Milliseconds(),
		FrameHash: hex.EncodeToString(hash[:6]), App: iname,
	})
	s.diagnosticsEvents.add(device.ID, "device_poll", "HTTP frame delivered: "+trace.Reason, iname)
	slog.Info("HTTP frame request",
		"request_id", requestID,
		"device_id", device.ID,
		"authenticated", true,
		"selection_reason", trace.Reason,
		"classification", trace.Classification,
		"installation_id", iname,
		"app_name", appID,
		"last_app_index_before", lastIndexBefore,
		"last_app_index_after", device.LastAppIndex,
		"displaying_app_before", displayingBefore,
		"displaying_app_after", optionalString(device.DisplayingApp),
		"display_restore_app", restoreBefore,
		"render_result", renderResult,
		"render_message", renderMessage,
		"cache_decision", trace.CacheDecision,
		"webp_path", filepath.Base(trace.WebPPath),
		"sha256_prefix", hex.EncodeToString(hash[:6]),
		"response_bytes", len(imgData),
		"dwell_seconds", dwell,
		"brightness", brightness,
		"status", http.StatusOK,
		"last_seen", now.UTC().Format(time.RFC3339),
		"duration_ms", time.Since(started).Milliseconds(),
	)
}

func effectiveFrameDwell(now time.Time, configured int, app *data.App) int {
	configured = max(configured, 1)
	if app == nil || app.NextRenderAt == nil || !app.NextRenderAt.After(now) {
		return configured
	}
	untilBoundary := int(app.NextRenderAt.Sub(now).Round(time.Second) / time.Second)
	if untilBoundary < 1 {
		untilBoundary = 1
	}
	return min(configured, untilBoundary)
}
