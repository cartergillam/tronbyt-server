package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"tronbyt-server/internal/providers"

	"tronbyt-server/internal/apps"
	"tronbyt-server/internal/data"
	"tronbyt-server/internal/renderer"

	securejoin "github.com/cyphar/filepath-securejoin"
	_ "golang.org/x/image/webp"
	"gorm.io/gorm"
)

const mobileAPIMaxBody = 1 << 20
const catalogueIconMaxBytes = 4 << 20
const catalogueIconMaxPixels = 4_194_304

type apiError struct {
	Error apiErrorDetail `json:"error"`
}

type apiErrorDetail struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

func writeAPIError(w http.ResponseWriter, status int, code, message string, fields map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{Error: apiErrorDetail{Code: code, Message: message, Fields: fields}})
}

func decodeAPIJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json", nil)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, mobileAPIMaxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "Request body is not valid JSON", nil)
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "Request body must contain one JSON value", nil)
		return false
	}
	return true
}

type normalizedSchema struct {
	Version string                  `json:"version"`
	Fields  []normalizedSchemaField `json:"fields"`
}

type normalizedSchemaField struct {
	Key         string                   `json:"key"`
	Title       string                   `json:"title"`
	Description string                   `json:"description,omitempty"`
	Type        string                   `json:"type"`
	Required    bool                     `json:"required"`
	Default     any                      `json:"default,omitempty"`
	Minimum     *float64                 `json:"minimum,omitempty"`
	Maximum     *float64                 `json:"maximum,omitempty"`
	Options     []normalizedSchemaOption `json:"options,omitempty"`
	Secret      bool                     `json:"secret"`
	Order       int                      `json:"order"`
	Visibility  any                      `json:"visibility,omitempty"`
	SourceType  string                   `json:"sourceType,omitempty"`
	Pattern     string                   `json:"pattern,omitempty"`
	Placeholder string                   `json:"placeholder,omitempty"`
}

type normalizedSchemaOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type pixletSchemaEnvelope struct {
	Version string                  `json:"version"`
	Schema  []pixletSchemaFieldJSON `json:"schema"`
}

type pixletSchemaFieldJSON struct {
	Type        string                   `json:"type"`
	ID          string                   `json:"id"`
	Name        string                   `json:"name"`
	Description string                   `json:"description"`
	Default     any                      `json:"default"`
	Options     []pixletSchemaOptionJSON `json:"options"`
	Secret      bool                     `json:"secret"`
	Visibility  any                      `json:"visibility"`
	Required    bool                     `json:"required"`
	Minimum     *float64                 `json:"minimum"`
	Maximum     *float64                 `json:"maximum"`
	Pattern     string                   `json:"pattern"`
	Placeholder string                   `json:"placeholder"`
}

type pixletSchemaOptionJSON struct {
	Display string `json:"display"`
	Text    string `json:"text"`
	Value   string `json:"value"`
}

func normalizeFieldType(source string, secret bool) string {
	if secret {
		return "secret"
	}
	switch strings.ToLower(source) {
	case "onoff", "boolean", "bool", "toggle":
		return "boolean"
	case "dropdown", "radio", "enum", "select", "typeahead", "locationbased":
		return "enum"
	case "integer", "int":
		return "integer"
	case "number", "float":
		return "number"
	case "color", "colour":
		return "colour"
	case "location":
		return "location"
	case "datetime", "date", "time":
		return strings.ToLower(source)
	case "text", "string", "oauth1", "oauth2":
		return "string"
	case "png":
		return "image"
	case "generated":
		return "group"
	default:
		return strings.ToLower(source)
	}
}

func normalizeDefault(field pixletSchemaFieldJSON, typ string) any {
	if field.Default == nil {
		return nil
	}
	if typ == "boolean" {
		switch value := field.Default.(type) {
		case bool:
			return value
		case string:
			parsed, err := strconv.ParseBool(value)
			if err == nil {
				return parsed
			}
		}
	}
	return field.Default
}

func normalizeSchemaBytes(raw []byte) (normalizedSchema, error) {
	if len(raw) == 0 {
		return normalizedSchema{Version: "1", Fields: []normalizedSchemaField{}}, nil
	}
	var source pixletSchemaEnvelope
	if err := json.Unmarshal(raw, &source); err != nil {
		return normalizedSchema{}, fmt.Errorf("decode schema: %w", err)
	}
	if source.Version == "" {
		source.Version = "1"
	}
	result := normalizedSchema{Version: source.Version, Fields: make([]normalizedSchemaField, 0, len(source.Schema))}
	for i, field := range source.Schema {
		typ := normalizeFieldType(field.Type, field.Secret)
		options := make([]normalizedSchemaOption, 0, len(field.Options))
		for _, option := range field.Options {
			label := option.Display
			if label == "" {
				label = option.Text
			}
			options = append(options, normalizedSchemaOption{Label: label, Value: option.Value})
		}
		required := field.Required
		// Pixlet's historical Dropdown schema does not consistently serialize a
		// required flag. An empty-default team selector is intentionally explicit:
		// callers must choose a team before installing or saving.
		if field.ID == "team" && strings.EqualFold(strings.TrimSpace(field.Name), "Team Focus") && typ == "enum" && len(options) > 0 {
			required = true
		}
		result.Fields = append(result.Fields, normalizedSchemaField{
			Key: field.ID, Title: field.Name, Description: field.Description,
			Type: typ, Required: required, Default: normalizeDefault(field, typ),
			Minimum: field.Minimum, Maximum: field.Maximum, Options: options,
			Secret: field.Secret, Order: i, Visibility: field.Visibility, SourceType: field.Type,
			Pattern: field.Pattern, Placeholder: field.Placeholder,
		})
	}
	return result, nil
}

type catalogueApp struct {
	ID                             string            `json:"id"`
	Name                           string            `json:"name"`
	Description                    string            `json:"description"`
	Author                         string            `json:"author"`
	Category                       string            `json:"category,omitempty"`
	Tags                           []string          `json:"tags"`
	Repository                     string            `json:"repository"`
	IconURL                        *string           `json:"iconURL"`
	Configurable                   bool              `json:"configurable"`
	Compatible                     *bool             `json:"compatible"`
	Published                      string            `json:"published,omitempty"`
	Updated                        string            `json:"updated,omitempty"`
	RecommendedInterval            int               `json:"recommendedRenderIntervalMin,omitempty"`
	Featured                       bool              `json:"featured"`
	LocationAware                  bool              `json:"locationAware"`
	Installed                      bool              `json:"installed"`
	Verified                       bool              `json:"verified"`
	Candidate                      bool              `json:"candidate,omitempty"`
	Recommended                    bool              `json:"recommended"`
	VerifiedVersion                string            `json:"verifiedVersion,omitempty"`
	VerificationDate               string            `json:"verificationDate,omitempty"`
	CompatibilityNotes             string            `json:"compatibilityNotes,omitempty"`
	PreferredConfigurationDefaults map[string]any    `json:"preferredConfigurationDefaults,omitempty"`
	Schema                         *normalizedSchema `json:"schema,omitempty"`
	meta                           apps.AppMetadata
}

func nonNilStrings(value []string) []string {
	if value == nil {
		return []string{}
	}
	return value
}

func (s *Server) catalogueForUser(user *data.User) []catalogueApp {
	var result []catalogueApp
	appendMetadata := func(meta apps.AppMetadata, repository string) {
		description := meta.Desc
		if description == "" {
			description = meta.Summary
		}
		var icon *string
		if meta.Preview != "" || meta.Preview2x != "" {
			value := "/v0/catalogue/" + url.PathEscape(meta.ID) + "/icon"
			icon = &value
		}
		configurable := !strings.EqualFold(filepath.Ext(meta.FileName), ".webp")
		item := catalogueApp{
			ID: meta.ID, Name: meta.Name, Description: description, Author: meta.Author,
			Category: meta.Category, Tags: nonNilStrings(meta.Tags), Repository: repository,
			IconURL: icon, Configurable: configurable, Published: meta.Published, Updated: meta.Updated,
			RecommendedInterval: meta.RecommendedInterval,
			Featured:            strings.EqualFold(meta.Category, "featured") || containsFold(meta.Tags, "featured"),
			LocationAware:       containsFold(meta.Tags, "location") || containsFold(meta.Tags, "weather") || strings.Contains(strings.ToLower(description), "location"),
			meta:                meta,
		}
		if verified, ok := verifiedMetadataFor(meta.ID); ok {
			item.Verified = verified.Verified
			item.Candidate = verified.Candidate
			item.Recommended = verified.Recommended
			item.VerifiedVersion = verified.VerifiedVersion
			item.VerificationDate = verified.VerificationDate
			item.CompatibilityNotes = verified.CompatibilityNotes
			item.PreferredConfigurationDefaults = verified.PreferredConfigurationDefaults
		}
		result = append(result, item)
	}
	for _, meta := range s.ListSystemApps() {
		appendMetadata(meta, "system")
	}
	if user != nil {
		for _, meta := range apps.ListUserApps(s.DataDir, user.Username) {
			repository := "custom"
			if strings.Contains(filepath.ToSlash(meta.Path), "/repo/apps/") {
				repository = "custom-repository"
			}
			appendMetadata(meta, repository)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, right := strings.ToLower(result[i].Name), strings.ToLower(result[j].Name)
		if left == right {
			if result[i].Repository == result[j].Repository {
				return result[i].ID < result[j].ID
			}
			return result[i].Repository < result[j].Repository
		}
		return left < right
	})
	return result
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func catalogueRevisionForItems(items []catalogueApp) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "verified-manifest:%s\n", verifiedAppsRevision())
	for _, item := range items {
		_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%s\x00%s\x00%t\x00%s\n", item.ID, item.Repository, item.Updated, item.meta.Preview, item.Verified, item.VerifiedVersion)
	}
	return hex.EncodeToString(hash.Sum(nil)[:8])
}

func (s *Server) catalogueRevision(user *data.User) string {
	return catalogueRevisionForItems(s.catalogueForUser(user))
}

func (s *Server) findCatalogueApp(user *data.User, id string) (*catalogueApp, error) {
	for _, item := range s.catalogueForUser(user) {
		if item.ID == id {
			copy := item
			return &copy, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

func (s *Server) catalogueAppPath(item *catalogueApp) (string, error) {
	path := item.meta.Path
	if item.Repository == "system" {
		path = filepath.Join(path, item.meta.FileName)
	}
	return securejoin.SecureJoin(s.DataDir, path)
}

func (s *Server) loadNormalizedSchema(ctx context.Context, item *catalogueApp, supports2x bool) (normalizedSchema, error) {
	path, err := s.catalogueAppPath(item)
	if err != nil {
		return normalizedSchema{}, err
	}
	if strings.EqualFold(filepath.Ext(path), ".webp") {
		return normalizedSchema{Version: "1", Fields: []normalizedSchemaField{}}, nil
	}
	raw, err := renderer.GetSchema(ctx, path, 64, 32, supports2x)
	if err != nil {
		return normalizedSchema{}, err
	}
	return normalizeSchemaBytes(raw)
}

func (s *Server) handleMobilePreview(w http.ResponseWriter, r *http.Request) {
	device := GetDevice(r)
	image, app, err := s.GetCurrentAppImage(r.Context(), device)
	if err != nil || len(image) == 0 {
		writeAPIError(w, http.StatusNotFound, "preview_not_found", "No rendered preview is available for this device", nil)
		return
	}
	sum := sha256.Sum256(image)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	modified := app.LastSuccessfulRender
	if modified == nil && !app.LastRender.IsZero() {
		modified = &app.LastRender
	}
	if modified != nil {
		w.Header().Set("Last-Modified", modified.UTC().Format(http.TimeFormat))
	}
	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Content-Length", strconv.Itoa(len(image)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(image)
}

func (s *Server) handleInstallationPreview(w http.ResponseWriter, r *http.Request) {
	device := GetDevice(r)
	app := device.GetApp(r.PathValue("iname"))
	if app == nil || app.Pushed {
		writeAPIError(w, http.StatusNotFound, "app_not_found", "Installation not found", nil)
		return
	}
	path := s.getAppWebpPath(filepath.Join(s.DataDir, "webp", device.ID), app)
	stat, err := os.Stat(path)
	if err != nil || stat.IsDir() || stat.Size() <= 0 || stat.Size() > 32<<20 {
		if code, message, failed := installationPreviewFailure(app); failed {
			writeAPIError(w, http.StatusFailedDependency, code, message, nil)
			return
		}
		writeAPIError(w, http.StatusNotFound, "preview_not_found", "No rendered preview is available for this installation", nil)
		return
	}
	content, err := os.ReadFile(path)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "preview_not_found", "No rendered preview is available for this installation", nil)
		return
	}
	sum := sha256.Sum256(content)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.Header().Set("Last-Modified", stat.ModTime().UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func installationPreviewFailure(app *data.App) (string, string, bool) {
	result := strings.ToLower(strings.TrimSpace(app.LastRenderResult))
	message := sanitizedRenderMessage(app.LastRenderMessage)
	if result == "empty" || app.EmptyLastRender && result == "" {
		return "preview_empty_frame", "The app rendered an empty frame", true
	}
	if result != "failure" && result != "upstream_failure" {
		return "", "", false
	}
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out"):
		return "preview_timeout", "The app preview render timed out", true
	case strings.Contains(lower, "configuration") || strings.Contains(lower, "required"):
		return "preview_invalid_config", firstNonEmpty(message, "The app configuration could not be rendered"), true
	case result == "upstream_failure" || strings.Contains(lower, "nws") || strings.Contains(lower, "http") || strings.Contains(lower, "network") || strings.Contains(lower, "dial"):
		return "preview_network_unavailable", firstNonEmpty(message, "A network dependency required by this app is unavailable"), true
	default:
		return "preview_render_failed", firstNonEmpty(message, "Pixlet could not render this app preview"), true
	}
}

func firstNonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func (s *Server) handleCatalogueList(w http.ResponseWriter, r *http.Request) {
	items := s.catalogueForUser(GetUser(r))
	revision := catalogueRevisionForItems(items)
	for i := range items {
		if items[i].IconURL != nil {
			value := *items[i].IconURL + "?revision=" + url.QueryEscape(revision)
			items[i].IconURL = &value
		}
	}
	search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search")))
	category := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("category")))
	repository := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("repository")))
	locationAware := r.URL.Query().Get("locationAware") == "true"
	sports := r.URL.Query().Get("sports") == "true"
	installedOnly := r.URL.Query().Get("installed") == "true"
	compatibleOnly := r.URL.Query().Get("compatible") == "true"
	deviceID := strings.TrimSpace(r.URL.Query().Get("deviceID"))
	deviceAuthorized := false
	if deviceID != "" {
		if principal, err := MobilePrincipalFromContext(r.Context()); err == nil {
			for _, assignedID := range principal.DeviceIDs {
				if assignedID == deviceID {
					deviceAuthorized = true
					break
				}
			}
		} else if scoped, err := DeviceFromContext(r.Context()); err == nil {
			deviceAuthorized = scoped.ID == deviceID
		} else if user := GetUser(r); user != nil {
			count, _ := gorm.G[data.Device](s.DB).Where("id = ? AND username = ?", deviceID, user.Username).Count(r.Context(), "*")
			deviceAuthorized = count == 1
		}
	}
	installedIDs := map[string]bool{}
	if (installedOnly || r.URL.Query().Has("installed")) && deviceAuthorized {
		appsForDevice, _ := gorm.G[data.App](s.DB).Where("device_id = ? AND pushed = ?", deviceID, false).Find(r.Context())
		for _, app := range appsForDevice {
			installedIDs[app.Name] = true
		}
	}
	filtered := make([]catalogueApp, 0, len(items))
	for _, item := range items {
		item.Installed = installedIDs[item.ID] || installedIDs[item.Name]
		if deviceAuthorized {
			compatible := true
			item.Compatible = &compatible
		}
		searchable := strings.ToLower(item.ID + " " + item.Name + " " + item.Description + " " + item.Author + " " + item.Category + " " + strings.Join(item.Tags, " "))
		if search != "" && !strings.Contains(searchable, search) {
			slog.Debug("Catalogue app filtered", "app_id", item.ID, "reason", "search_mismatch", "search_length", len(search))
			continue
		}
		if category != "" && !(category == "verified" && item.Verified) && strings.ToLower(item.Category) != category {
			slog.Debug("Catalogue app filtered", "app_id", item.ID, "reason", "category_mismatch", "requested_category", category)
			continue
		}
		if repository != "" && strings.ToLower(item.Repository) != repository {
			continue
		}
		if locationAware && !item.LocationAware {
			continue
		}
		if sports && !(containsFold(item.Tags, "sports") || strings.EqualFold(item.Category, "sports")) {
			continue
		}
		if installedOnly && !item.Installed {
			continue
		}
		if compatibleOnly && (item.Compatible == nil || !*item.Compatible) {
			continue
		}
		filtered = append(filtered, item)
	}
	if search != "" {
		sort.SliceStable(filtered, func(i, j int) bool {
			if filtered[i].Verified != filtered[j].Verified {
				return filtered[i].Verified
			}
			if filtered[i].Recommended != filtered[j].Recommended {
				return filtered[i].Recommended
			}
			return strings.ToLower(filtered[i].Name) < strings.ToLower(filtered[j].Name)
		})
	} else if r.URL.Query().Get("sort") == "recent" {
		sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Updated > filtered[j].Updated })
	} else if r.URL.Query().Get("sort") == "featured" {
		sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Featured && !filtered[j].Featured })
	}
	offset := 0
	if value := r.URL.Query().Get("offset"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_offset", "offset must be a non-negative integer", nil)
			return
		}
		offset = parsed
	}
	if offset < 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_offset", "offset must be non-negative", nil)
		return
	}
	limit := 50
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 200 {
			writeAPIError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 200", nil)
			return
		}
		limit = parsed
	}
	total := len(filtered)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	var nextOffset *int
	if end < total {
		value := end
		nextOffset = &value
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"apps": filtered[offset:end], "offset": offset, "limit": limit, "total": total, "nextOffset": nextOffset,
		"repositoryRevision": revision, "verifiedManifestRevision": verifiedAppsRevision(), "iconRevision": revision, "schemaVersion": "1",
	})
}

func (s *Server) handleCatalogueDetail(w http.ResponseWriter, r *http.Request) {
	item, err := s.findCatalogueApp(GetUser(r), r.PathValue("appID"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "app_not_found", "Catalogue app not found", nil)
		return
	}
	supports2x := false
	if device, err := DeviceFromContext(r.Context()); err == nil {
		supports2x = device.Type.Supports2x()
		value := true
		item.Compatible = &value
	}
	schema, err := s.loadNormalizedSchema(r.Context(), item, supports2x)
	if err != nil {
		slog.Error("Failed to load catalogue detail schema", "app_id", item.ID, "error", err)
		writeAPIError(w, http.StatusBadGateway, "schema_unavailable", "App schema could not be loaded", nil)
		return
	}
	item.Schema = &schema
	item.Configurable = len(schema.Fields) > 0
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(item)
}

func (s *Server) handleCatalogueSchema(w http.ResponseWriter, r *http.Request) {
	item, err := s.findCatalogueApp(GetUser(r), r.PathValue("appID"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "app_not_found", "Catalogue app not found", nil)
		return
	}
	supports2x := false
	if device, err := DeviceFromContext(r.Context()); err == nil {
		supports2x = device.Type.Supports2x()
	}
	schema, err := s.loadNormalizedSchema(r.Context(), item, supports2x)
	if err != nil {
		slog.Error("Failed to load catalogue schema", "app_id", item.ID, "error", err)
		writeAPIError(w, http.StatusBadGateway, "schema_unavailable", "App schema could not be loaded", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(schema)
}

func (s *Server) handleCatalogueIcon(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	s.metrics.catalogueIconActive.Inc()
	result := "success"
	defer func() {
		s.metrics.catalogueIconActive.Dec()
		s.metrics.catalogueIconTotal.WithLabelValues(result).Inc()
		s.metrics.catalogueIconDuration.Observe(time.Since(started).Seconds())
	}()
	item, err := s.findCatalogueApp(GetUser(r), r.PathValue("appID"))
	if err != nil {
		result = "not_found"
		http.NotFound(w, r)
		return
	}
	file := item.meta.Preview
	if item.meta.Supports2x && item.meta.Preview2x != "" {
		file = item.meta.Preview2x
	}
	if file == "" {
		result = "not_found"
		http.NotFound(w, r)
		return
	}
	path, err := securejoin.SecureJoin(filepath.Join(s.DataDir, filepath.Dir(item.meta.Path)), file)
	if err != nil {
		result = "invalid_path"
		http.NotFound(w, r)
		return
	}
	stat, err := os.Stat(path)
	if err != nil || stat.IsDir() {
		result = "not_found"
		http.NotFound(w, r)
		return
	}
	if stat.Size() <= 0 || stat.Size() > catalogueIconMaxBytes {
		result = "invalid_image"
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_icon", "Catalogue icon exceeds safe decode limits", nil)
		return
	}
	icon, err := os.Open(path)
	if err != nil {
		result = "not_found"
		http.NotFound(w, r)
		return
	}
	config, _, decodeErr := image.DecodeConfig(io.LimitReader(icon, catalogueIconMaxBytes+1))
	_ = icon.Close()
	if decodeErr != nil || config.Width <= 0 || config.Height <= 0 || config.Width > 4096 || config.Height > 4096 || config.Width*config.Height > catalogueIconMaxPixels {
		result = "invalid_image"
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_icon", "Catalogue icon could not be decoded safely", nil)
		return
	}
	etag := fmt.Sprintf(`W/"%x-%x"`, stat.ModTime().UnixNano(), stat.Size())
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=3600, stale-while-revalidate=86400")
	if r.Header.Get("If-None-Match") == etag {
		result = "not_modified"
		w.WriteHeader(http.StatusNotModified)
		return
	}
	http.ServeFile(w, r, path)
}

func findSchemaField(schema normalizedSchema, key string) *normalizedSchemaField {
	for i := range schema.Fields {
		if schema.Fields[i].Key == key {
			return &schema.Fields[i]
		}
	}
	return nil
}

func normalizeConfigValue(field *normalizedSchemaField, value any) (any, error) {
	switch field.Type {
	case "boolean":
		switch typed := value.(type) {
		case bool:
			return strconv.FormatBool(typed), nil
		case string:
			if _, err := strconv.ParseBool(typed); err == nil {
				return strings.ToLower(typed), nil
			}
		}
		return nil, errors.New("must be a boolean")
	case "integer":
		number, ok := value.(float64)
		if !ok || number != float64(int64(number)) {
			return nil, errors.New("must be an integer")
		}
		if field.Minimum != nil && number < *field.Minimum || field.Maximum != nil && number > *field.Maximum {
			return nil, errors.New("is outside the allowed range")
		}
		return int64(number), nil
	case "number":
		number, ok := value.(float64)
		if !ok {
			return nil, errors.New("must be a number")
		}
		if field.Minimum != nil && number < *field.Minimum || field.Maximum != nil && number > *field.Maximum {
			return nil, errors.New("is outside the allowed range")
		}
		return number, nil
	case "enum":
		text, ok := value.(string)
		if !ok {
			return nil, errors.New("must be a string")
		}
		for _, option := range field.Options {
			if option.Value == text {
				return text, nil
			}
		}
		return nil, errors.New("is not an allowed option")
	case "string", "secret", "colour", "date", "time", "datetime":
		text, ok := value.(string)
		if !ok {
			return nil, errors.New("must be a string")
		}
		if field.Pattern != "" {
			pattern, err := regexp.Compile(field.Pattern)
			if err == nil && !pattern.MatchString(text) {
				return nil, errors.New("does not match the required format")
			}
		}
	}
	return value, nil
}

func validateConfigPatch(schema normalizedSchema, existing, patch map[string]any) (map[string]any, map[string]string) {
	result := normalizedLegacyConfig(schema, existing)
	fieldErrors := map[string]string{}
	for key, value := range patch {
		field := findSchemaField(schema, key)
		if field == nil {
			if current, exists := existing[key]; exists && reflect.DeepEqual(current, value) {
				continue
			}
			fieldErrors[key] = "unknown configuration field"
			continue
		}
		if field.Secret && value == nil {
			continue
		}
		if marker, ok := value.(map[string]any); ok && field.Secret && marker["keepExisting"] == true {
			continue
		}
		normalized, err := normalizeConfigValue(field, value)
		if err != nil {
			fieldErrors[key] = err.Error()
			continue
		}
		result[key] = normalized
	}
	for _, field := range schema.Fields {
		if field.Required {
			if value, ok := result[field.Key]; !ok || value == nil || value == "" {
				fieldErrors[field.Key] = "is required"
			}
		}
	}
	if findSchemaField(schema, "watchlist") != nil && findSchemaField(schema, "symbols") != nil {
		if raw, ok := result["watchlist"].(string); ok && strings.TrimSpace(raw) != "" {
			listings, err := providers.ParseMarketWatchlist(raw)
			if err != nil {
				fieldErrors["watchlist"] = err.Error()
			} else {
				encoded, _ := json.Marshal(listings)
				result["watchlist"] = string(encoded)
			}
		}
	}
	return result, fieldErrors
}

func normalizedLegacyConfig(schema normalizedSchema, existing map[string]any) map[string]any {
	result := make(map[string]any, len(existing)+1)
	for key, value := range existing {
		result[key] = value
	}
	if findSchemaField(schema, "team_color_background_style") != nil {
		if _, stylePresent := result["team_color_background_style"]; !stylePresent {
			legacy, present := result["show_team_colored_logo_background"]
			if !present {
				legacy, present = result["show_team_coloured_logo_background"]
			}
			if enabled, ok := legacy.(bool); present && ok && !enabled {
				result["team_color_background_style"] = "off"
			} else {
				result["team_color_background_style"] = "full"
			}
		}
		delete(result, "show_team_colored_logo_background")
		delete(result, "show_team_coloured_logo_background")
	}
	return result
}

func sanitizeConfig(schema normalizedSchema, config map[string]any) (map[string]any, map[string]bool) {
	values := make(map[string]any)
	secrets := make(map[string]bool)
	for key, value := range config {
		field := findSchemaField(schema, key)
		if field != nil && field.Secret {
			secrets[key] = value != nil && value != ""
			continue
		}
		if field != nil && field.Type == "boolean" {
			if text, ok := value.(string); ok {
				if parsed, err := strconv.ParseBool(text); err == nil {
					value = parsed
				}
			}
		}
		values[key] = value
	}
	return values, secrets
}

func schemaDefaults(schema normalizedSchema) map[string]any {
	defaults := make(map[string]any)
	for i := range schema.Fields {
		field := &schema.Fields[i]
		if field.Default == nil || field.Secret {
			continue
		}
		value, err := normalizeConfigValue(field, field.Default)
		if err == nil {
			defaults[field.Key] = value
		}
	}
	return defaults
}

func (s *Server) installationAndSchema(ctx context.Context, device *data.Device, installationID string) (*data.App, normalizedSchema, error) {
	app := device.GetApp(installationID)
	if app == nil || app.DeviceID != device.ID || app.Path == nil || app.Pushed {
		return nil, normalizedSchema{}, gorm.ErrRecordNotFound
	}
	path := filepath.ToSlash(*app.Path)
	item := &catalogueApp{
		Repository: "custom",
		meta: apps.AppMetadata{
			Path: path,
			Manifest: apps.Manifest{
				ID:       app.Name,
				FileName: filepath.Base(path),
			},
		},
	}
	if strings.HasPrefix(path, "system-apps/") {
		item.Repository = "system"
		item.meta.Path = filepath.ToSlash(filepath.Dir(path))
	}
	schema, err := s.loadNormalizedSchema(ctx, item, device.Type.Supports2x())
	return app, schema, err
}

func (s *Server) installationConfigPayload(device *data.Device, app *data.App, schema normalizedSchema) map[string]any {
	config, secrets := sanitizeConfig(schema, normalizedLegacyConfig(schema, app.Config))
	return map[string]any{
		"installation": s.toAppPayload(device, app),
		"appID":        app.Name,
		"schema":       schema,
		"config":       config,
		"savedSecrets": secrets,
		"render": map[string]any{
			"status":           app.LastRenderResult,
			"message":          sanitizedRenderMessage(app.LastRenderMessage),
			"previewAvailable": app.LastSuccessfulRender != nil && !app.EmptyLastRender,
		},
	}
}

func (s *Server) handleInstallationConfigGet(w http.ResponseWriter, r *http.Request) {
	device := GetDevice(r)
	app, schema, err := s.installationAndSchema(r.Context(), device, r.PathValue("installationID"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "installation_not_found", "Installation not found", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.installationConfigPayload(device, app, schema))
}

func (s *Server) renderInstallation(ctx context.Context, device *data.Device, app *data.App, config map[string]any) ([]byte, []string, error) {
	path, err := securejoin.SecureJoin(s.DataDir, *app.Path)
	if err != nil {
		return nil, nil, err
	}
	// RenderApp injects legacy runtime values such as $tz into its config map.
	// Render a clone so those values are never persisted as user configuration.
	return s.RenderApp(ctx, device, app, path, maps.Clone(config))
}

func (s *Server) saveRenderedInstallationImage(device *data.Device, app *data.App, image []byte) error {
	dir, err := s.ensureDeviceImageDir(device.ID)
	if err != nil {
		return err
	}
	path, err := securejoin.SecureJoin(dir, fmt.Sprintf("%s-%s.webp", app.Name, app.Iname))
	if err != nil {
		return err
	}
	return os.WriteFile(path, image, 0644)
}

func (s *Server) handleInstallationConfigPatch(w http.ResponseWriter, r *http.Request) {
	device := GetDevice(r)
	unlock := s.lockDevicePoll(device.ID)
	defer unlock()
	fresh, err := gorm.G[data.Device](s.DB).Preload("Apps", orderedAppsPreload).Where("id = ?", device.ID).First(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "device_reload_failed", "Device state could not be refreshed", nil)
		return
	}
	device = &fresh
	app, schema, err := s.installationAndSchema(r.Context(), device, r.PathValue("installationID"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "installation_not_found", "Installation not found", nil)
		return
	}
	var request struct {
		Config               map[string]any `json:"config"`
		ExpectedStateVersion *uint64        `json:"expectedStateVersion"`
		MutationID           string         `json:"mutationID"`
	}
	if !decodeAPIJSON(w, r, &request) {
		return
	}
	if request.ExpectedStateVersion != nil && *request.ExpectedStateVersion != device.StateVersion {
		writeAPIError(w, http.StatusConflict, "stale_state", "Device state changed before this configuration was applied", nil)
		return
	}
	if memberMarketCredentialChangeForbidden(r, app.Name, app.Config, request.Config) {
		writeAPIError(w, 403, "owner_required", "Only the owner can change market credentials", nil)
		return
	}
	updated, fieldErrors := validateConfigPatch(schema, app.Config, request.Config)
	if len(fieldErrors) > 0 {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_config", "Configuration validation failed", fieldErrors)
		return
	}
	image, messages, err := s.renderInstallation(r.Context(), device, app, updated)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "render_failed", "Configuration could not be rendered: "+sanitizedRenderMessage(err.Error()), nil)
		return
	}
	now := time.Now()
	result, message, nextRenderAt := classifyRenderResult(now, image, messages, nil, app.UInterval)
	mutationID := strings.TrimSpace(request.MutationID)
	if mutationID == "" {
		mutationID = newFrameRequestID()
	}
	if len(mutationID) > 64 {
		mutationID = mutationID[:64]
	}
	previousVersion := device.StateVersion
	device.StateVersion++
	device.LastMutationID = mutationID
	device.LastMutationResult = "configuration_updated"
	contextApp := *app
	contextApp.Config = updated
	contextHash := s.marketRenderContextHash(r.Context(), device, &contextApp)
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		update := data.App{
			Config: updated, LastRender: now,
			EmptyLastRender:   len(image) == 0,
			RenderContextHash: contextHash,
			LastRenderResult:  result,
			LastRenderMessage: message,
			NextRenderAt:      nextRenderAt,
		}
		fields := []string{"Config", "LastRender", "EmptyLastRender", "RenderContextHash", "LastRenderResult", "LastRenderMessage", "NextRenderAt"}
		if len(image) > 0 {
			update.LastSuccessfulRender = &now
			fields = append(fields, "LastSuccessfulRender")
		}
		if err := tx.Model(&data.App{ID: app.ID}).Select(fields).Updates(update).Error; err != nil {
			return err
		}
		if err := tx.Omit("Apps").Save(device).Error; err != nil {
			return err
		}
		if len(image) > 0 {
			return s.saveRenderedInstallationImage(device, app, image)
		}
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "save_failed", "Configuration could not be saved", nil)
		return
	}
	app.Config, app.LastRender, app.EmptyLastRender, app.RenderContextHash = updated, now, len(image) == 0, contextHash
	app.LastRenderResult, app.LastRenderMessage, app.NextRenderAt = result, message, nextRenderAt
	if len(image) > 0 {
		app.LastSuccessfulRender = &now
	}
	recordDeviceInvalidation(device, previousVersion, "app_configuration", mutationID)
	s.notifyDashboard(GetUser(r).Username, WSEvent{Type: "apps_changed", DeviceID: device.ID})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.installationConfigPayload(device, app, schema))
}

type installationCreateRequest struct {
	AppID             string         `json:"appID"`
	Name              string         `json:"name"`
	Config            map[string]any `json:"config"`
	Enabled           *bool          `json:"enabled"`
	DisplayTimeSec    int            `json:"displayTimeSec"`
	RenderIntervalMin int            `json:"renderIntervalMin"`
	MutationID        string         `json:"mutationID"`
}

func (s *Server) handleInstallationCreate(w http.ResponseWriter, r *http.Request) {
	device, user := GetDevice(r), GetUser(r)
	unlock := s.lockDevicePoll(device.ID)
	defer unlock()
	fresh, reloadErr := gorm.G[data.Device](s.DB).Preload("Apps", orderedAppsPreload).Where("id = ?", device.ID).First(r.Context())
	if reloadErr != nil {
		writeAPIError(w, http.StatusInternalServerError, "device_reload_failed", "Device state could not be refreshed", nil)
		return
	}
	device = &fresh
	var request installationCreateRequest
	if !decodeAPIJSON(w, r, &request) {
		return
	}
	item, err := s.findCatalogueApp(user, request.AppID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "app_not_found", "Catalogue app not found", nil)
		return
	}
	if request.RenderIntervalMin < 1 || request.RenderIntervalMin > 1440 || request.DisplayTimeSec < 1 || request.DisplayTimeSec > 3600 {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_timing", "renderIntervalMin must be 1...1440 and displayTimeSec must be 1...3600", nil)
		return
	}
	path, err := s.catalogueAppPath(item)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_app", "Catalogue app path is invalid", nil)
		return
	}
	relativePath, err := filepath.Rel(s.DataDir, path)
	if err != nil || strings.HasPrefix(relativePath, "..") {
		writeAPIError(w, http.StatusBadRequest, "invalid_app", "Catalogue app path is invalid", nil)
		return
	}
	schema, err := s.loadNormalizedSchema(r.Context(), item, device.Type.Supports2x())
	if err != nil {
		slog.Error("Failed to load installation schema", "app_id", item.ID, "error", err)
		writeAPIError(w, http.StatusBadGateway, "schema_unavailable", "App schema could not be loaded", nil)
		return
	}
	mutationID := strings.TrimSpace(request.MutationID)
	if len(mutationID) > 64 {
		mutationID = mutationID[:64]
	}
	existing, duplicateErr := gorm.G[data.App](s.DB).Where("device_id = ? AND path = ?", device.ID, relativePath).First(r.Context())
	if duplicateErr == nil {
		if mutationID != "" && device.LastMutationID == mutationID && device.LastMutationResult == "installation_created" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(s.installationConfigPayload(device, &existing, schema))
			return
		}
		writeAPIError(w, http.StatusConflict, "duplicate_installation", "This app is already installed on the device", nil)
		return
	}
	if !errors.Is(duplicateErr, gorm.ErrRecordNotFound) {
		writeAPIError(w, http.StatusInternalServerError, "database_error", "Installation state could not be checked", nil)
		return
	}
	configKeys := make([]string, 0, len(request.Config))
	for key := range request.Config {
		configKeys = append(configKeys, key)
	}
	sort.Strings(configKeys)
	slog.Info("Installation request validated", "request_id", r.Header.Get("X-Request-ID"), "device_id", device.ID, "app_id", item.ID, "config_keys", configKeys, "mutation_id", mutationID)
	if memberMarketCredentialChangeForbidden(r, item.ID, schemaDefaults(schema), request.Config) {
		writeAPIError(w, 403, "owner_required", "Only the owner can change market credentials", nil)
		return
	}
	config, fieldErrors := validateConfigPatch(schema, schemaDefaults(schema), request.Config)
	for _, field := range schema.Fields {
		if !field.Required {
			continue
		}
		value, explicitlyProvided := request.Config[field.Key]
		if !explicitlyProvided || value == nil || value == "" {
			fieldErrors[field.Key] = "is required"
		}
	}
	if len(fieldErrors) > 0 {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_config", "Configuration validation failed", fieldErrors)
		return
	}
	iname, err := generateUniqueIname(s.DB, device.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "id_generation_failed", "Installation ID could not be generated", nil)
		return
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	app := data.App{
		DeviceID: device.ID, Iname: iname, Name: item.ID, Path: &relativePath, Config: config,
		Enabled: enabled, UInterval: request.RenderIntervalMin, DisplayTime: request.DisplayTimeSec,
	}
	now := time.Now()
	previousVersion := device.StateVersion
	device.StateVersion++
	if mutationID == "" {
		mutationID = newFrameRequestID()
	}
	device.LastMutationID = mutationID
	device.LastMutationResult = "installation_created"
	image, messages, renderErr := s.renderInstallation(r.Context(), device, &app, config)
	result, renderMessage, nextRenderAt := classifyRenderResult(now, image, messages, renderErr, app.UInterval)
	app.LastRender, app.EmptyLastRender = now, len(image) == 0
	app.LastRenderResult, app.LastRenderMessage, app.NextRenderAt = result, renderMessage, nextRenderAt
	app.RenderContextHash = renderContextHash(device, &app)
	if len(image) > 0 {
		app.LastSuccessfulRender = &now
	}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		maxOrder, err := getMaxAppOrder(tx, device.ID)
		if err != nil {
			return err
		}
		app.Order = maxOrder + 1
		if err := gorm.G[data.App](tx).Create(r.Context(), &app); err != nil {
			return err
		}
		if err := tx.Omit("Apps").Save(device).Error; err != nil {
			return err
		}
		if len(image) > 0 {
			return s.saveRenderedInstallationImage(device, &app, image)
		}
		return nil
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "installation_failed", "Installation could not be created", nil)
		return
	}
	device.Apps = append(device.Apps, &app)
	recordDeviceInvalidation(device, previousVersion, "installation_created", mutationID)
	slog.Info("Installation mutation completed",
		"request_id", r.Header.Get("X-Request-ID"), "device_id", device.ID, "app_id", item.ID,
		"config_keys", configKeys, "validation_result", "valid", "http_status", http.StatusCreated,
		"initial_render_result", result, "initial_render_message", renderMessage,
		"rollback_result", "not_required", "final_installation_state", "installed", "mutation_id", mutationID,
	)
	s.notifyDashboard(user.Username, WSEvent{Type: "apps_changed", DeviceID: device.ID})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(s.installationConfigPayload(device, &app, schema))
}

func (s *Server) reorderInstallations(ctx context.Context, deviceID string, installationIDs []string) ([]*data.App, error) {
	var ordered []*data.App
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var reorderErr error
		ordered, reorderErr = reorderInstallationsTx(ctx, tx, deviceID, installationIDs)
		return reorderErr
	})
	return ordered, err
}

func reorderInstallationsTx(ctx context.Context, tx *gorm.DB, deviceID string, installationIDs []string) ([]*data.App, error) {
	appsList, err := gorm.G[data.App](tx).Where("device_id = ?", deviceID).Find(ctx)
	if err != nil {
		return nil, err
	}
	realApps := make([]data.App, 0, len(appsList))
	pushedIDs := make(map[string]bool)
	for i := range appsList {
		if appsList[i].Pushed {
			pushedIDs[appsList[i].Iname] = true
			continue
		}
		realApps = append(realApps, appsList[i])
	}
	if len(installationIDs) != len(realApps) {
		return nil, errors.New("all installations must be included")
	}
	byID := make(map[string]*data.App, len(realApps))
	for i := range realApps {
		byID[realApps[i].Iname] = &realApps[i]
	}
	ordered := make([]*data.App, 0, len(installationIDs))
	seen := make(map[string]bool, len(installationIDs))
	for _, id := range installationIDs {
		if seen[id] {
			return nil, fmt.Errorf("duplicate installation ID %q", id)
		}
		seen[id] = true
		if pushedIDs[id] {
			return nil, fmt.Errorf("installation %q is temporary pushed content", id)
		}
		app := byID[id]
		if app == nil {
			return nil, fmt.Errorf("installation %q does not belong to this device", id)
		}
		ordered = append(ordered, app)
	}
	for order, app := range ordered {
		if _, err := gorm.G[data.App](tx).Where("device_id = ? AND id = ?", deviceID, app.ID).Update(ctx, "order", order); err != nil {
			return nil, err
		}
		app.Order = order
	}
	return ordered, nil
}

func (s *Server) handleInstallationOrderPatch(w http.ResponseWriter, r *http.Request) {
	device := GetDevice(r)
	unlock := s.lockDevicePoll(device.ID)
	defer unlock()
	fresh, err := gorm.G[data.Device](s.DB).Preload("Apps", orderedAppsPreload).Where("id = ?", device.ID).First(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "device_reload_failed", "Device state could not be refreshed", nil)
		return
	}
	device = &fresh
	var request struct {
		InstallationIDs      []string `json:"installationIDs"`
		ExpectedStateVersion *uint64  `json:"expectedStateVersion"`
		MutationID           string   `json:"mutationID"`
	}
	if !decodeAPIJSON(w, r, &request) {
		return
	}
	if request.ExpectedStateVersion != nil && *request.ExpectedStateVersion != device.StateVersion {
		writeAPIError(w, http.StatusConflict, "stale_state", "Device state changed before this order was applied", nil)
		return
	}
	mutationID := strings.TrimSpace(request.MutationID)
	if mutationID == "" {
		mutationID = newFrameRequestID()
	}
	previousVersion := device.StateVersion
	device.StateVersion++
	device.LastMutationID, device.LastMutationResult = mutationID, "installation_order_updated"
	var ordered []*data.App
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		var reorderErr error
		ordered, reorderErr = reorderInstallationsTx(r.Context(), tx, device.ID, request.InstallationIDs)
		if reorderErr != nil {
			return reorderErr
		}
		return tx.Omit("Apps").Save(device).Error
	})
	if err != nil {
		if strings.Contains(err.Error(), "installation") || strings.Contains(err.Error(), "duplicate") {
			writeAPIError(w, http.StatusUnprocessableEntity, "invalid_order", err.Error(), nil)
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "order_state_failed", "Installation order state could not be saved", nil)
		return
	}
	recordDeviceInvalidation(device, previousVersion, "rotation_order", mutationID)
	payloads := make([]AppPayload, 0, len(ordered))
	for _, app := range ordered {
		payloads = append(payloads, s.toAppPayload(device, app))
	}
	s.notifyDashboard(GetUser(r).Username, WSEvent{Type: "apps_changed", DeviceID: device.ID})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"installationIDs": request.InstallationIDs, "installations": payloads})
}
