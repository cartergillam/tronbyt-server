package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"tronbyt-server/internal/data"
	"tronbyt-server/internal/providers"
	"tronbyt-server/internal/renderer"
	"tronbyt-server/web"

	securejoin "github.com/cyphar/filepath-securejoin"
	"gorm.io/gorm"
)

// RenderApp consolidates the logic for rendering an app for a device.
// It handles config overrides, timezone/locale injection, dwell time, and filters.
func (s *Server) RenderApp(ctx context.Context, device *data.Device, app *data.App, appPath string, configOverrides map[string]any) ([]byte, []string, error) {
	// Config
	var config map[string]any
	switch {
	case configOverrides != nil:
		config = maps.Clone(configOverrides)
	case app != nil && app.Config != nil:
		config = maps.Clone(app.Config)
	default:
		config = make(map[string]any)
	}

	// Timezone & Locale
	var deviceTimezone string
	var locale *string
	supports2x := false

	if device != nil {
		deviceTimezone = device.GetTimezone()
		applyDeviceRenderContext(config, device)
		locale = device.Locale
		supports2x = device.Type.Supports2x()
	}
	if device != nil && app != nil {
		s.injectManagedProviderData(ctx, device, app, config)
	}

	// Dwell Time
	var appInterval int
	if device != nil {
		appInterval = device.GetEffectiveDwellTime(app)
	} else {
		appInterval = 15 // Default fallback if no device context
	}

	// Filters
	var filters []string
	if device != nil {
		filters = s.getEffectiveFilters(device, app)
	}

	// App-level rendering overrides are optional when rendering previews.
	var showFullAnimation *bool
	if app != nil {
		showFullAnimation = app.ShowFullAnimation
	}

	// 2. Static WebP App
	if strings.HasSuffix(strings.ToLower(appPath), ".webp") {
		content, err := os.ReadFile(appPath)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read static webp app: %w", err)
		}
		return content, nil, nil
	}

	return renderer.Render(
		ctx,
		appPath,
		config,
		64, 32,
		time.Duration(appInterval)*time.Second,
		30*time.Second,
		true,
		supports2x,
		&deviceTimezone,
		locale,
		filters,
		showFullAnimation,
	)
}

func (s *Server) injectManagedProviderData(ctx context.Context, device *data.Device, app *data.App, config map[string]any) {
	credentialID, _ := config["credential_id"].(string)
	credentialID = strings.TrimSpace(credentialID)
	setError := func(err error) {
		code := "provider_temporarily_unavailable"
		message := "Provider data is temporarily unavailable"
		var sanitized providers.SanitizedError
		if errors.As(err, &sanitized) {
			code, message = sanitized.Code, sanitized.Message
		}
		encoded, _ := json.Marshal(map[string]any{"code": code, "message": message})
		config["$provider_error"] = string(encoded)
	}
	switch app.Name {
	case "market-watch":
		if credentialID == "" {
			setError(providers.MissingCredential("Market data"))
			return
		}
		if s.MarketProvider == nil {
			setError(providers.ProviderSetupRequired("Market data"))
			return
		}
		symbols := providerSymbols(config["symbols"])
		quotes, err := s.MarketProvider.Quotes(ctx, providers.MarketRequest{Symbols: symbols, CredentialID: credentialID, ScopeType: "server_owner", ScopeID: device.Username})
		if err != nil {
			setError(err)
			return
		}
		encoded, _ := json.Marshal(quotes)
		config["$provider_data"] = string(encoded)
	case "local-weather":
		if credentialID == "" {
			setError(providers.MissingCredential("Weather"))
			return
		}
		if s.WeatherProvider == nil {
			setError(providers.TemporarilyUnavailable())
			return
		}
		location := providers.Location{Latitude: device.Location.Lat, Longitude: device.Location.Lng, Timezone: device.GetTimezone(), Label: device.Location.Description}
		if raw, ok := config["custom_location"].(string); ok && strings.TrimSpace(raw) != "" {
			var custom data.DeviceLocation
			if json.Unmarshal([]byte(raw), &custom) == nil {
				location = providers.Location{Latitude: custom.Lat, Longitude: custom.Lng, Timezone: custom.Timezone, Label: custom.Description}
			}
		}
		units, _ := config["units"].(string)
		if units == "" {
			units = "metric"
		}
		snapshot, err := s.WeatherProvider.Weather(ctx, providers.WeatherRequest{Location: location, Units: units, CredentialID: credentialID, ScopeType: "server_owner", ScopeID: device.Username})
		if err != nil {
			setError(err)
			return
		}
		encoded, _ := json.Marshal(snapshot)
		config["$provider_data"] = string(encoded)
	case "nhl-live":
		if s.SportsProvider == nil {
			setError(providers.SportsUnavailable())
			return
		}
		timezone := device.GetTimezone()
		mode, _ := config["mode"].(string)
		teamID := providerTeamID(config["teamid"])
		// Historical NHL Live installs used teamid=0 for random/all teams.
		// Preserve that behavior by mapping it to the deterministic all-live mode.
		if mode == "all_live" || teamID == "0" {
			snapshot, err := s.SportsProvider.LiveGames(ctx, providers.SportsLiveRequest{League: providers.LeagueNHL, Timezone: timezone})
			if err != nil {
				setError(err)
				return
			}
			encoded := s.encodedSportsSnapshot(ctx, snapshot)
			config["$provider_data"] = string(encoded)
			return
		}
		if teamID == "" {
			teamID = "10"
		}
		snapshot, err := s.SportsProvider.Schedule(ctx, providers.SportsScheduleRequest{League: providers.LeagueNHL, TeamID: teamID, Timezone: timezone})
		if err != nil {
			setError(err)
			return
		}
		encoded := s.encodedSportsSnapshot(ctx, snapshot)
		config["$provider_data"] = string(encoded)
	case "cfl-scores":
		if s.SportsProvider == nil {
			setError(providers.SportsUnavailable())
			return
		}
		timezone := device.GetTimezone()
		mode, _ := config["scoreMode"].(string)
		teamID := providerTeamID(config["selectedTeam"])
		// Existing Auto semantics: a numeric selection follows that team;
		// All Teams resolves to the league-wide live view.
		if mode == "league" || teamID == "" || teamID == "all" {
			snapshot, err := s.SportsProvider.LiveGames(ctx, providers.SportsLiveRequest{League: providers.LeagueCFL, Timezone: timezone})
			if err != nil {
				setError(err)
				return
			}
			encoded := s.encodedSportsSnapshot(ctx, snapshot)
			config["$sports_data"] = string(encoded)
			return
		}
		limit := providerPositiveInt(config["upcomingGames"], 1, 3)
		snapshot, err := s.SportsProvider.Schedule(ctx, providers.SportsScheduleRequest{League: providers.LeagueCFL, TeamID: teamID, Timezone: timezone, Limit: limit})
		if err != nil {
			setError(err)
			return
		}
		encoded := s.encodedSportsSnapshot(ctx, snapshot)
		config["$sports_data"] = string(encoded)
	case "nba-live":
		if s.SportsProvider == nil {
			setError(providers.SportsUnavailable())
			return
		}
		timezone := device.GetTimezone()
		mode, _ := config["mode"].(string)
		teamID := providerTeamID(config["teamid"])
		if mode == "all_live" {
			snapshot, err := s.SportsProvider.LiveGames(ctx, providers.SportsLiveRequest{League: providers.LeagueNBA, Timezone: timezone})
			if err != nil {
				setError(err)
				return
			}
			encoded := s.encodedSportsSnapshot(ctx, snapshot)
			config["$sports_data"] = string(encoded)
			return
		}
		if teamID == "" {
			teamID = "28"
		}
		snapshot, err := s.SportsProvider.Schedule(ctx, providers.SportsScheduleRequest{League: providers.LeagueNBA, TeamID: teamID, Timezone: timezone})
		if err != nil {
			setError(err)
			return
		}
		encoded := s.encodedSportsSnapshot(ctx, snapshot)
		config["$sports_data"] = string(encoded)
	case "nfl-live":
		if s.SportsProvider == nil {
			setError(providers.SportsUnavailable())
			return
		}
		timezone := device.GetTimezone()
		mode, _ := config["mode"].(string)
		teamID := providerTeamID(config["teamid"])
		if mode == "all_live" {
			snapshot, err := s.SportsProvider.LiveGames(ctx, providers.SportsLiveRequest{League: providers.LeagueNFL, Timezone: timezone})
			if err != nil {
				setError(err)
				return
			}
			encoded := s.encodedSportsSnapshot(ctx, snapshot)
			config["$sports_data"] = string(encoded)
			return
		}
		if teamID == "" {
			teamID = "2"
		}
		snapshot, err := s.SportsProvider.Schedule(ctx, providers.SportsScheduleRequest{League: providers.LeagueNFL, TeamID: teamID, Timezone: timezone})
		if err != nil {
			setError(err)
			return
		}
		encoded := s.encodedSportsSnapshot(ctx, snapshot)
		config["$sports_data"] = string(encoded)
	}
}

func (s *Server) encodedSportsSnapshot(ctx context.Context, snapshot providers.SportsSnapshot) []byte {
	if s.SportsLogos != nil {
		snapshot = s.SportsLogos.Hydrate(ctx, snapshot)
	}
	encoded, _ := json.Marshal(snapshot)
	return encoded
}

func providerTeamID(value any) providers.ProviderTeamID {
	switch typed := value.(type) {
	case string:
		return providers.ProviderTeamID(strings.TrimSpace(typed))
	case float64:
		return providers.ProviderTeamID(fmt.Sprintf("%.0f", typed))
	case int:
		return providers.ProviderTeamID(fmt.Sprintf("%d", typed))
	case int64:
		return providers.ProviderTeamID(fmt.Sprintf("%d", typed))
	default:
		return ""
	}
}

func providerPositiveInt(value any, fallback, maximum int) int {
	result := 0
	switch typed := value.(type) {
	case string:
		result, _ = strconv.Atoi(strings.TrimSpace(typed))
	case float64:
		result = int(typed)
	case int:
		result = typed
	case int64:
		result = int(typed)
	}
	if result < 1 {
		return fallback
	}
	if result > maximum {
		return maximum
	}
	return result
}

func providerSymbols(value any) []string {
	var values []string
	switch typed := value.(type) {
	case string:
		values = strings.FieldsFunc(typed, func(r rune) bool { return r == ',' || r == '\n' })
	case []string:
		values = typed
	case []any:
		for _, item := range typed {
			if symbol, ok := item.(string); ok {
				values = append(values, symbol)
			}
		}
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if symbol := strings.TrimSpace(value); symbol != "" {
			if normalized, err := providers.NormalizeMarketSymbol(symbol); err == nil {
				if seen[normalized] {
					continue
				}
				seen[normalized] = true
				result = append(result, normalized)
				continue
			}
			// Keep malformed input for the provider contract, which returns a
			// sanitized invalid_symbol response instead of silently ignoring it.
			result = append(result, symbol)
		}
	}
	return result
}

func applyDeviceRenderContext(config map[string]any, device *data.Device) {
	// The legacy "$tz" variable remains for custom apps; location-aware apps
	// receive both an explicit device object and a backwards-compatible default.
	config["$tz"] = device.GetTimezone()
	if !device.HasLocation() {
		return
	}
	encoded, err := json.Marshal(device.Location)
	if err != nil {
		return
	}
	locationJSON := string(encoded)
	config["$location"] = locationJSON
	if current, present := config["location"]; !present || current == nil || current == "" || current == "__device__" {
		config["location"] = locationJSON
	}
}

func (s *Server) possiblyRender(ctx context.Context, app *data.App, device *data.Device, user *data.User) bool {
	// 1. Pushed App (Pre-rendered)
	if app.Pushed {
		if app.PushKind != persistentPushKind {
			return false
		}
		webpDir, err := s.ensureDeviceImageDir(device.ID)
		if err != nil {
			return false
		}
		if _, err := os.Stat(s.getAppWebpPath(webpDir, app)); err != nil {
			s.removeMissingPersistentPush(ctx, device, app)
			return false
		}
		if app.AutoPin {
			s.handleAutoPin(ctx, app, device, user, true)
		}
		return true
	}

	if app.Path == nil || *app.Path == "" {
		return false
	}

	appPath, err := securejoin.SecureJoin(s.DataDir, *app.Path)
	if err != nil {
		slog.Error("Failed to resolve app path", "path", *app.Path, "error", err)
		return false
	}
	appBasename := fmt.Sprintf("%s-%s", app.Name, app.Iname)
	webpDir, err := s.ensureDeviceImageDir(device.ID)
	if err != nil {
		slog.Error("Failed to get device webp directory for rendering", "device_id", device.ID, "error", err)
		return false
	}
	webpPath, err := securejoin.SecureJoin(webpDir, fmt.Sprintf("%s.webp", appBasename))
	if err != nil {
		slog.Error("Path traversal attempt in webp path", "app", appBasename, "error", err)
		return false
	}

	// 2. Static WebP App
	if strings.HasSuffix(strings.ToLower(*app.Path), ".webp") {
		if _, err := os.Stat(webpPath); os.IsNotExist(err) {
			// Copy from source
			if _, err := os.Stat(appPath); err == nil {
				if err := copyFile(appPath, webpPath); err != nil {
					slog.Error("Failed to copy static webp file", "src", appPath, "dst", webpPath, "error", err)
					return false
				}
			} else {
				slog.Warn("Source WebP not found", "path", appPath)
				return false
			}
		}
		return true // Exists
	}

	// 3. Starlark App - Check interval or an app-provided eligibility boundary.
	now := time.Now()
	contextHash := renderContextHash(device, app)
	contextChanged := app.RenderContextHash != contextHash
	shouldRender := contextChanged || renderDue(now, app)
	cacheDecision := "hit"
	if contextChanged {
		cacheDecision = "miss_context_changed"
	} else if shouldRender {
		cacheDecision = "miss_render_due"
	}
	if trace := selectionTrace(ctx); trace != nil {
		trace.CacheDecision = cacheDecision
	}
	localNow := now.In(deviceLocation(device))
	slog.Debug("Render cache decision",
		"app", appBasename,
		"render_timestamp", now.UTC().Format(time.RFC3339Nano),
		"device_local_timestamp", localNow.Format(time.RFC3339Nano),
		"device_timezone", device.GetTimezone(),
		"cache_decision", cacheDecision,
		"context_hash", contextHash,
	)
	if shouldRender {
		slog.Info("Rendering app", "app", appBasename)

		startTime := time.Now()
		imgBytes, messages, err := s.RenderApp(ctx, device, app, appPath, nil)
		renderDur := time.Since(startTime)

		for _, msg := range messages {
			slog.Debug("Render message", "app", appBasename, "message", msg)
		}

		empty := len(imgBytes) == 0
		success := err == nil && !empty
		result, message, nextRenderAt := classifyRenderResult(now, imgBytes, messages, err, app.UInterval)
		visibleMinute := markerValue(messages, visibleMinuteMarker)
		if visibleMinute == "" {
			visibleMinute = localNow.Format("2006-01-02T15:04-07:00")
		}
		returnedDwell := effectiveFrameDwell(now, device.GetEffectiveDwellTime(app), &data.App{NextRenderAt: nextRenderAt})
		slog.Info("Render timing",
			"app", appBasename,
			"render_timestamp", now.UTC().Format(time.RFC3339Nano),
			"device_local_timestamp", localNow.Format(time.RFC3339Nano),
			"next_render_timestamp", optionalTime(nextRenderAt),
			"cache_decision", cacheDecision,
			"returned_dwell_seconds", returnedDwell,
			"rendered_visible_minute", visibleMinute,
		)

		s.metrics.renderDuration.Observe(renderDur.Seconds())
		switch {
		case err != nil:
			s.metrics.renderTotal.WithLabelValues("error").Inc()
			slog.Error("Error rendering app", "app", appBasename, "error", err)
		case empty:
			s.metrics.renderTotal.WithLabelValues("empty").Inc()
			slog.Debug("No output from app", "app", appBasename)
		default:
			s.metrics.renderTotal.WithLabelValues("success").Inc()
		}

		// Update App State in DB - This is our atomic check-and-update.
		// If the app was deleted while we were rendering, RowsAffected will be 0.
		appUpdates := data.App{
			LastRender:        now,
			LastRenderDur:     renderDur,
			EmptyLastRender:   !success,
			RenderMessages:    data.StringSlice(messages),
			LastRenderResult:  result,
			LastRenderMessage: message,
			NextRenderAt:      nextRenderAt,
			RenderContextHash: contextHash,
		}
		switch result {
		case "visible":
			appUpdates.ConsecutiveFailures = 0
			appUpdates.ConsecutiveHidden = 0
		case "hidden":
			appUpdates.ConsecutiveFailures = 0
			appUpdates.ConsecutiveHidden = app.ConsecutiveHidden + 1
		default:
			appUpdates.ConsecutiveFailures = app.ConsecutiveFailures + 1
			appUpdates.ConsecutiveHidden = 0
		}

		q := gorm.G[data.App](s.DB).Where("id = ?", app.ID)
		if success {
			appUpdates.LastSuccessfulRender = &now
			q = q.Select(
				"LastRender", "LastRenderDur", "EmptyLastRender", "RenderMessages",
				"LastRenderResult", "LastRenderMessage", "NextRenderAt", "RenderContextHash",
				"ConsecutiveFailures", "ConsecutiveHidden", "LastSuccessfulRender",
			)
		} else {
			q = q.Select(
				"LastRender", "LastRenderDur", "EmptyLastRender", "RenderMessages",
				"LastRenderResult", "LastRenderMessage", "NextRenderAt", "RenderContextHash",
				"ConsecutiveFailures", "ConsecutiveHidden",
			)
		}

		rowsAffected, err := q.Updates(ctx, appUpdates)
		if err != nil {
			slog.Error("Failed to update app state in DB", "app", appBasename, "error", err)
			return false
		}

		if rowsAffected == 0 {
			slog.Info("App no longer exists in DB, aborting", "app", appBasename)
			return false
		}

		if success {
			// Save WebP
			if err := os.WriteFile(webpPath, imgBytes, 0644); err != nil {
				slog.Error("Failed to write webp", "path", webpPath, "error", err)
			}
		}

		// Update in-memory object (passed pointer)
		app.LastRender = now
		if success {
			app.LastSuccessfulRender = &now
		}
		app.LastRenderDur = renderDur
		app.EmptyLastRender = !success
		app.RenderMessages = messages
		app.LastRenderResult = result
		app.LastRenderMessage = message
		app.NextRenderAt = nextRenderAt
		app.RenderContextHash = contextHash
		app.ConsecutiveFailures = appUpdates.ConsecutiveFailures
		app.ConsecutiveHidden = appUpdates.ConsecutiveHidden

		// Handle Autopin
		if app.AutoPin {
			s.handleAutoPin(ctx, app, device, user, success)
		}
		eventType := "render_" + result
		s.diagnosticsEvents.add(device.ID, eventType, message, app.Iname)
		return success
	}

	return true // Not time to render yet, assume existing is fine
}

func renderDue(now time.Time, app *data.App) bool {
	if app == nil || app.LastRender.IsZero() || app.LastRender.After(now.Add(5*time.Minute)) {
		return true
	}
	if app.NextRenderAt != nil {
		return !now.Before(*app.NextRenderAt)
	}
	return now.Sub(app.LastRender) > time.Duration(app.UInterval)*time.Minute
}

const (
	nextRenderMarker    = "TRONBYT-NEXT-RENDER:"
	visibleMinuteMarker = "TRONBYT-VISIBLE-MINUTE:"
	hiddenUntilMarker   = "TRONBYT-HIDDEN-UNTIL:"
	renderFailureMarker = "TRONBYT-RENDER-FAILURE:"
)

func renderContextHash(device *data.Device, app *data.App) string {
	return renderContextHashAt(time.Now(), device, app)
}

func renderContextHashAt(now time.Time, device *data.Device, app *data.App) string {
	context := map[string]any{
		"deviceID":            device.ID,
		"config":              app.Config,
		"timezone":            device.GetTimezone(),
		"location":            device.Location,
		"supports2x":          device.Type.Supports2x(),
		"stateVersion":        device.StateVersion,
		"effectiveBrightness": device.GetEffectiveBrightness(),
		"rotationIndex":       device.LastAppIndex,
		"sleeping":            device.Sleeping,
		"pinnedApp":           device.PinnedApp,
		"activeShowNowApp":    device.ActiveShowNowApp,
		"showNowRestoreApp":   device.ShowNowRestoreApp,
	}
	if isClockInstallation(app) {
		context["visibleLocalMinute"] = now.In(deviceLocation(device)).Format("2006-01-02T15:04")
	}
	if device.Locale != nil {
		context["locale"] = *device.Locale
	}
	encoded, err := json.Marshal(context)
	if err != nil {
		return "unavailable"
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:8])
}

func isClockInstallation(app *data.App) bool {
	if app == nil {
		return false
	}
	if strings.EqualFold(app.Name, "og-clock") {
		return true
	}
	return app.Path != nil && strings.Contains(strings.ToLower(*app.Path), "/ogclock/")
}

func recordDeviceInvalidation(device *data.Device, previous uint64, reason, mutationID string) {
	slog.Info("Device render state invalidated",
		"device_id", device.ID,
		"previous_version", previous,
		"new_version", device.StateVersion,
		"mutation_id", mutationID,
		"invalidation_reason", reason,
	)
}

func deviceLocation(device *data.Device) *time.Location {
	location, err := time.LoadLocation(device.GetTimezone())
	if err != nil {
		return time.UTC
	}
	return location
}

func markerValue(messages []string, prefix string) string {
	for _, raw := range messages {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func optionalTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func classifyRenderResult(now time.Time, image []byte, messages []string, renderErr error, intervalMinutes int) (string, string, *time.Time) {
	hidden := false
	upstreamFailure := false
	failureMessage := ""
	var next *time.Time
	for _, raw := range messages {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, hiddenUntilMarker):
			hidden = true
			next = parseBoundedRenderTime(now, strings.TrimSpace(strings.TrimPrefix(line, hiddenUntilMarker)))
		case strings.Contains(line, "APPLET HIDDEN FROM ROTATION"):
			hidden = true
		case strings.HasPrefix(line, renderFailureMarker):
			upstreamFailure = true
			failureMessage = sanitizedRenderMessage(strings.TrimSpace(strings.TrimPrefix(line, renderFailureMarker)))
		case strings.HasPrefix(line, nextRenderMarker):
			next = parseBoundedRenderTime(now, strings.TrimSpace(strings.TrimPrefix(line, nextRenderMarker)))
		}
	}
	result := "visible"
	message := ""
	if renderErr != nil {
		result = "failure"
		message = sanitizedRenderMessage(renderErr.Error())
		next = nil
	} else if upstreamFailure {
		result = "upstream_failure"
		message = failureMessage
		next = nil
	} else if hidden {
		result = "hidden"
		message = "Intentionally hidden"
	}
	if len(image) == 0 && result == "visible" {
		result = "empty"
		message = "Render produced no visible output"
	}
	if next == nil {
		var delay time.Duration
		switch result {
		case "hidden":
			delay = 30 * time.Minute
		case "failure", "upstream_failure":
			delay = 2 * time.Minute
		case "empty":
			delay = 5 * time.Minute
		default:
			delay = time.Duration(intervalMinutes) * time.Minute
		}
		value := now.Add(delay)
		next = &value
	}
	return result, message, next
}

func parseBoundedRenderTime(now time.Time, value string) *time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	// Minute-boundary apps commonly render during the final second of a minute.
	// Reject stale markers, but preserve even a sub-second future boundary so
	// the HTTP dwell can be shortened and the display polls on time.
	if err != nil || !parsed.After(now) || parsed.After(now.Add(24*time.Hour)) {
		return nil
	}
	return &parsed
}

func sanitizedRenderMessage(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.TrimSpace(value)
	fields := strings.Fields(value)
	for i, field := range fields {
		if query := strings.IndexByte(field, '?'); query >= 0 && strings.Contains(field[query+1:], "=") {
			fields[i] = field[:query] + "?[redacted]"
			continue
		}
		if separator := strings.IndexByte(field, '='); separator > 0 {
			key := strings.Trim(strings.ToLower(field[:separator]), "\"'[]{}(),:")
			switch key {
			case "token", "key", "api_key", "apikey", "authorization", "password", "secret":
				fields[i] = field[:separator+1] + "[redacted]"
			}
		}
	}
	value = strings.Join(fields, " ")
	if len(value) > 240 {
		value = value[:240]
	}
	return value
}

func (s *Server) handleAutoPin(ctx context.Context, app *data.App, device *data.Device, user *data.User, success bool) {
	shouldNotify := false
	if success {
		// Pin if not already pinned
		if device.PinnedApp == nil || *device.PinnedApp != app.Iname {
			if _, err := gorm.G[data.Device](s.DB).Where("id = ?", device.ID).Update(ctx, "pinned_app", app.Iname); err != nil {
				slog.Error("Failed to pin app", "app", app.Iname, "device_id", device.ID, "error", err)
			} else {
				device.PinnedApp = &app.Iname
				shouldNotify = true
			}
		}
	} else {
		// Unpin if currently pinned to this app
		if device.PinnedApp != nil && *device.PinnedApp == app.Iname {
			if _, err := gorm.G[data.Device](s.DB).Where("id = ?", device.ID).Update(ctx, "pinned_app", nil); err != nil {
				slog.Error("Failed to unpin app", "app", app.Iname, "device_id", device.ID, "error", err)
			} else {
				device.PinnedApp = nil
				shouldNotify = true
			}
		}
	}

	if shouldNotify && user != nil {
		s.notifyDashboard(user.Username, WSEvent{Type: "apps_changed", DeviceID: device.ID})
	}
}

func (s *Server) getEffectiveFilters(device *data.Device, app *data.App) []string {
	var filters []string

	// Determine base device filter
	var deviceFilter data.ColorFilter
	if device.GetNightModeIsActive() && device.NightColorFilter != nil {
		deviceFilter = *device.NightColorFilter
	} else if device.GetDimModeIsActive() && device.DimColorFilter != nil {
		deviceFilter = *device.DimColorFilter
	} else if device.ColorFilter != nil {
		deviceFilter = *device.ColorFilter
	} else {
		deviceFilter = data.ColorFilterNone
	}

	appFilter := data.ColorFilterInherit
	if app != nil && app.ColorFilter != nil {
		appFilter = *app.ColorFilter
	}

	if appFilter != data.ColorFilterInherit {
		if appFilter != data.ColorFilterNone {
			filters = append(filters, string(appFilter))
		}
	} else {
		// Inherit from device
		if deviceFilter != data.ColorFilterNone {
			filters = append(filters, string(deviceFilter))
		}
	}
	return filters
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if err := in.Close(); err != nil {
			slog.Error("Failed to close source file", "error", err)
		}
	}()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if err := out.Close(); err != nil {
			slog.Error("Failed to close destination file", "error", err)
		}
	}()

	_, err = io.Copy(out, in)
	return err
}

func (s *Server) sendDefaultImage(w http.ResponseWriter, r *http.Request, device *data.Device) {
	// Fallback if main image retrieval fails
	path := "static/images/default.webp"

	// Get default image from embedded assets
	f, err := web.Assets.Open(path)
	if err != nil {
		slog.Error("Failed to open default image from assets", "error", err)
		http.Error(w, "Image not found", http.StatusNotFound)
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Error("Failed to close default image file", "error", err)
		}
	}()

	stat, _ := f.Stat()

	// Headers
	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")

	brightness := device.GetEffectiveBrightness()
	w.Header().Set("Tronbyt-Brightness", fmt.Sprintf("%d", brightness))

	dwell := device.DefaultInterval
	w.Header().Set("Tronbyt-Dwell-Secs", fmt.Sprintf("%d", dwell))

	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "default.webp", stat.ModTime(), rs)
	} else {
		slog.Error("Embedded file does not implement ReadSeeker")
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}
