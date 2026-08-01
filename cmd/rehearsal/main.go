// Command rehearsal provides local-only seed, inspection, backup, polling and
// load tools for the Docker Compose deployment rehearsal.
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"tronbyt-server/internal/data"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultDeviceID  = "rehearsal-display"
	defaultDeviceKey = "rehearsal-device-key"
	defaultUserKey   = "rehearsal-user-key"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: tronbyt-rehearsal {seed|inspect|backup|restore|poll|load}")
	}
	var err error
	switch os.Args[1] {
	case "seed":
		err = seed(os.Args[2:])
	case "inspect":
		err = inspect()
	case "backup":
		err = backup(os.Args[2:])
	case "restore":
		err = restore(os.Args[2:])
	case "poll":
		err = poll(os.Args[2:])
	case "load":
		err = load(os.Args[2:])
	default:
		err = fmt.Errorf("unknown rehearsal command %q", os.Args[1])
	}
	if err != nil {
		fatalf("%v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "rehearsal: "+format+"\n", args...)
	os.Exit(1)
}

func environment(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func dataDirectory() string { return environment("DATA_DIR", "data") }

func openDatabase() (*gorm.DB, error) {
	dir := dataDirectory()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db, err := data.Open(filepath.Join(dir, "tronbyt.db"), "WARN")
	if err != nil {
		return nil, err
	}
	err = db.AutoMigrate(&data.User{}, &data.Device{}, &data.App{}, &data.WebAuthnCredential{}, &data.Setting{}, &data.OIDCIdentity{})
	return db, err
}

func seed(args []string) error {
	flags := flag.NewFlagSet("seed", flag.ContinueOnError)
	summaries := flags.Int("summaries", 1_000, "representative user catalogue summaries")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *summaries < 1 || *summaries > 10_000 {
		return errors.New("summaries must be between 1 and 10000")
	}
	db, err := openDatabase()
	if err != nil {
		return err
	}
	user := data.User{
		Username:      "rehearsal-user",
		Password:      "login-disabled-use-api-keys",
		APIKey:        environment("REHEARSAL_USER_KEY", defaultUserKey),
		SystemRepoURL: "https://github.com/cartergillam/apps.git",
	}
	if err := db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&user).Error; err != nil {
		return err
	}
	tz := "America/Toronto"
	device := data.Device{
		ID:                  environment("REHEARSAL_DEVICE_ID", defaultDeviceID),
		Username:            user.Username,
		Name:                "Local Rehearsal Display",
		Type:                data.DeviceMatrixPortal,
		APIKey:              environment("REHEARSAL_DEVICE_KEY", defaultDeviceKey),
		Brightness:          65,
		DefaultInterval:     15,
		NightModeEnabled:    true,
		NightModeApp:        "clock",
		NightStart:          "22:00",
		NightEnd:            "07:00",
		NightBrightness:     8,
		Timezone:            &tz,
		Location:            data.DeviceLocation{Description: "Fixture City, Ontario, Canada", Locality: "Fixture City", Region: "Ontario", Country: "Canada", Lat: 43.1, Lng: -79.9, Timezone: tz},
		RequireAPIKey:       true,
		LastAppIndex:        0,
		PendingImageURL:     "",
		PendingUpdateURL:    "",
		InterstitialEnabled: false,
	}
	if err := db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&device).Error; err != nil {
		return err
	}
	clockPath := "system-apps/apps/ogclock/og_clock.star"
	mlbPath := "system-apps/apps/mlb_game/mlb_game.star"
	apps := []data.App{
		{DeviceID: device.ID, Iname: "clock", Name: "og-clock", Path: &clockPath, Enabled: true, Order: 0, UInterval: 1, DisplayTime: 15, Config: data.JSONMap{"timezone": "__device__", "time_format": "12"}},
		{DeviceID: device.ID, Iname: "mlb-game", Name: "mlb-game", Path: &mlbPath, Enabled: true, Order: 1, UInterval: 5, DisplayTime: 15, Config: data.JSONMap{"team": "TOR", "gameday_only": "true", "include_exhibition_opponents": "false"}},
	}
	for i := range apps {
		if err := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "device_id"}, {Name: "iname"}}, DoUpdates: clause.AssignmentColumns([]string{"name", "path", "enabled", "order", "uinterval", "display_time", "config"})}).Create(&apps[i]).Error; err != nil {
			return err
		}
	}
	settings := []data.Setting{
		{Key: "secret_key", Value: "local-rehearsal-cookie-secret-not-for-production"},
		{Key: "system_apps_repo", Value: "https://github.com/cartergillam/apps.git"},
	}
	for i := range settings {
		if err := db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&settings[i]).Error; err != nil {
			return err
		}
	}
	if err := seedRepresentativeCatalogue(user.Username, *summaries); err != nil {
		return err
	}
	fmt.Printf("seeded device=%s apps=%d representative_catalogue=%d location=%s\n", device.ID, len(apps), *summaries, tz)
	return nil
}

func seedRepresentativeCatalogue(username string, count int) error {
	root := filepath.Join(dataDirectory(), "users", username, "repo", "apps")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	preview, err := os.ReadFile(filepath.Join(dataDirectory(), "system-apps", "apps", "ogclock", "og_clock.webp"))
	if err != nil {
		return fmt.Errorf("read local apps preview: %w", err)
	}
	star := []byte("load(\"render.star\", \"render\")\n\ndef main():\n    return render.Root(child = render.Text(content = \"Load\"))\n")
	for i := range count {
		id := fmt.Sprintf("rehearsal-%04d", i)
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, id+".star"), star, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, id+".webp"), preview, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func inspect() error {
	db, err := openDatabase()
	if err != nil {
		return err
	}
	counts := map[string]int64{}
	for name, model := range map[string]any{"users": &data.User{}, "devices": &data.Device{}, "apps": &data.App{}} {
		var count int64
		if err := db.Model(model).Count(&count).Error; err != nil {
			return err
		}
		counts[name] = count
	}
	result := map[string]any{
		"database": filepath.Join(dataDirectory(), "tronbyt.db"),
		"counts":   counts,
		"columns": map[string]bool{
			"push_kind":          db.Migrator().HasColumn(&data.App{}, "push_kind"),
			"last_render_result": db.Migrator().HasColumn(&data.App{}, "last_render_result"),
			"next_render_at":     db.Migrator().HasColumn(&data.App{}, "next_render_at"),
			"location":           db.Migrator().HasColumn(&data.Device{}, "location"),
		},
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func safeDataDirectory() (string, error) {
	dir, err := filepath.Abs(dataDirectory())
	if err != nil {
		return "", err
	}
	if dir == "/" || dir == "." || len(filepath.Clean(dir)) < 5 {
		return "", fmt.Errorf("refusing broad data directory %q", dir)
	}
	return dir, nil
}

func backup(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: backup <archive.tar.gz>")
	}
	dir, err := safeDataDirectory()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(args[0], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	gzipWriter := gzip.NewWriter(out)
	tarWriter := tar.NewWriter(gzipWriter)
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "system-apps" || strings.HasPrefix(rel, "system-apps"+string(filepath.Separator)) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == "." {
			return nil
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tarWriter, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if walkErr != nil {
		return walkErr
	}
	if err := tarWriter.Close(); err != nil {
		return err
	}
	if err := gzipWriter.Close(); err != nil {
		return err
	}
	fmt.Printf("backup=%s\n", args[0])
	return nil
}

func restore(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: restore <archive.tar.gz>")
	}
	dir, err := safeDataDirectory()
	if err != nil {
		return err
	}
	archive, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer archive.Close()
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "system-apps" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(filepath.FromSlash(header.Name))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == "system-apps" || strings.HasPrefix(clean, "system-apps"+string(filepath.Separator)) {
			return fmt.Errorf("unsafe archive path %q", header.Name)
		}
		target := filepath.Join(dir, clean)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, reader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("unsupported archive entry %q", header.Name)
		}
	}
	fmt.Printf("restored=%s\n", args[0])
	return nil
}

type requestResult struct {
	Kind         string        `json:"kind"`
	Status       int           `json:"status"`
	App          string        `json:"app,omitempty"`
	Installation string        `json:"installation,omitempty"`
	Bytes        int           `json:"bytes"`
	Hash         string        `json:"sha256Prefix,omitempty"`
	Duration     time.Duration `json:"-"`
	DurationMS   int64         `json:"durationMS"`
	Error        string        `json:"error,omitempty"`
}

func request(ctx context.Context, client *http.Client, endpoint, key, kind string) requestResult {
	started := time.Now()
	result := requestResult{Kind: kind}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+key)
		var response *http.Response
		response, err = client.Do(req)
		if err == nil {
			defer response.Body.Close()
			result.Status = response.StatusCode
			result.App = response.Header.Get("Tronbyt-App")
			result.Installation = response.Header.Get("Tronbyt-Installation")
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 32<<20))
			err = readErr
			result.Bytes = len(body)
			sum := sha256.Sum256(body)
			result.Hash = hex.EncodeToString(sum[:6])
		}
	}
	result.Duration = time.Since(started)
	result.DurationMS = result.Duration.Milliseconds()
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

func poll(args []string) error {
	flags := flag.NewFlagSet("poll", flag.ContinueOnError)
	count := flags.Int("count", 5, "number of polls; zero runs until interrupted")
	interval := flags.Duration("interval", 15*time.Second, "poll interval")
	staleAfter := flags.Duration("stale-after", 5*time.Minute, "unchanged-frame warning threshold")
	if err := flags.Parse(args); err != nil {
		return err
	}
	base := strings.TrimRight(environment("REHEARSAL_BASE_URL", "http://127.0.0.1:18000"), "/")
	deviceID := environment("REHEARSAL_DEVICE_ID", defaultDeviceID)
	key := environment("REHEARSAL_DEVICE_KEY", defaultDeviceKey)
	endpoint := base + "/" + url.PathEscape(deviceID) + "/next"
	client := &http.Client{Timeout: 30 * time.Second}
	var lastHash string
	var unchangedSince time.Time
	for index := 0; *count == 0 || index < *count; index++ {
		result := request(context.Background(), client, endpoint, key, "poll")
		if result.Status == http.StatusUnauthorized {
			return errors.New("poll authentication rejected")
		}
		if result.Error != "" || result.Status != http.StatusOK {
			return fmt.Errorf("poll failed: status=%d error=%s", result.Status, result.Error)
		}
		if result.Hash != lastHash {
			lastHash = result.Hash
			unchangedSince = time.Now()
		} else if !unchangedSince.IsZero() && time.Since(unchangedSince) > *staleAfter {
			result.Error = "frame unchanged beyond stale threshold"
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return err
		}
		if *count == 0 || index+1 < *count {
			time.Sleep(*interval)
		}
	}
	return nil
}

type catalogueResponse struct {
	Apps []struct {
		ID      string  `json:"id"`
		IconURL *string `json:"iconURL"`
	} `json:"apps"`
	Total int `json:"total"`
}

type measurements struct {
	mu     sync.Mutex
	values map[string][]time.Duration
	errors map[string]int
}

func (m *measurements) add(result requestResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[result.Kind] = append(m.values[result.Kind], result.Duration)
	if result.Error != "" || result.Status < 200 || result.Status >= 400 {
		m.errors[result.Kind]++
	}
}

func percentile(values []time.Duration, fraction float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	copyValues := append([]time.Duration(nil), values...)
	sort.Slice(copyValues, func(i, j int) bool { return copyValues[i] < copyValues[j] })
	index := int(float64(len(copyValues)-1) * fraction)
	return copyValues[index]
}

func load(args []string) error {
	flags := flag.NewFlagSet("load", flag.ContinueOnError)
	duration := flags.Duration("duration", 60*time.Second, "test duration")
	pollInterval := flags.Duration("poll-interval", 15*time.Second, "physical poll cadence")
	if err := flags.Parse(args); err != nil {
		return err
	}
	base := strings.TrimRight(environment("REHEARSAL_BASE_URL", "http://127.0.0.1:18000"), "/")
	deviceID := environment("REHEARSAL_DEVICE_ID", defaultDeviceID)
	deviceKey := environment("REHEARSAL_DEVICE_KEY", defaultDeviceKey)
	userKey := environment("REHEARSAL_USER_KEY", defaultUserKey)
	client := &http.Client{Timeout: 30 * time.Second}

	initial := request(context.Background(), client, base+"/v0/catalogue?limit=200&offset=0", userKey, "catalogue")
	if initial.Status != http.StatusOK || initial.Error != "" {
		return fmt.Errorf("initial catalogue failed: status=%d error=%s", initial.Status, initial.Error)
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/v0/catalogue?limit=200&offset=0", nil)
	req.Header.Set("Authorization", "Bearer "+userKey)
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	var catalogue catalogueResponse
	err = json.NewDecoder(response.Body).Decode(&catalogue)
	response.Body.Close()
	if err != nil {
		return err
	}
	if catalogue.Total < 1_000 {
		return fmt.Errorf("catalogue has %d summaries; run rehearsal seed first", catalogue.Total)
	}
	iconURLs := make([]string, 0, 6)
	for _, app := range catalogue.Apps {
		if app.IconURL != nil && len(iconURLs) < 6 {
			iconURLs = append(iconURLs, base+*app.IconURL)
		}
	}
	if len(iconURLs) < 6 {
		return fmt.Errorf("catalogue exposed only %d icon URLs", len(iconURLs))
	}

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	m := &measurements{values: map[string][]time.Duration{}, errors: map[string]int{}}
	var workers sync.WaitGroup
	run := func(kind string, delay time.Duration, endpoint func(int) string, key string) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			index := 0
			for {
				select {
				case <-ctx.Done():
					return
				default:
					m.add(request(ctx, client, endpoint(index), key, kind))
					index++
					if delay > 0 {
						timer := time.NewTimer(delay)
						select {
						case <-ctx.Done():
							timer.Stop()
							return
						case <-timer.C:
						}
					}
				}
			}
		}()
	}
	run("poll", *pollInterval, func(int) string { return base + "/" + url.PathEscape(deviceID) + "/next" }, deviceKey)
	for worker := range 6 {
		workerIndex := worker
		run("icon", 20*time.Millisecond, func(int) string { return iconURLs[workerIndex] }, userKey)
	}
	searches := []string{"clock", "weather", "sports", "rehearsal-09"}
	for worker := range 4 {
		workerIndex := worker
		run("catalogue", 25*time.Millisecond, func(index int) string {
			return base + "/v0/catalogue?limit=30&search=" + url.QueryEscape(searches[(index+workerIndex)%len(searches)])
		}, userKey)
	}
	run("diagnostics", 100*time.Millisecond, func(int) string {
		return base + "/v0/devices/" + url.PathEscape(deviceID) + "/diagnostics"
	}, deviceKey)
	workers.Wait()

	metricsResult := request(context.Background(), client, base+"/metrics", "", "metrics")
	pprofResult := request(context.Background(), client, base+"/debug/pprof/goroutine?debug=1", "", "pprof")
	guardrailFailed := false
	result := map[string]any{
		"catalogueSummaries": catalogue.Total,
		"durationSeconds":    duration.Seconds(),
		"metricsStatus":      metricsResult.Status,
		"pprofStatus":        pprofResult.Status,
		"guardrails": map[string]any{
			"pollP95MS": 2_000,
			"pollMaxMS": 5_000,
			"apiP95MS":  2_000,
			"errorRate": 0.01,
		},
	}
	byKind := map[string]any{}
	for kind, values := range m.values {
		errors := m.errors[kind]
		maxValue := time.Duration(0)
		for _, value := range values {
			if value > maxValue {
				maxValue = value
			}
		}
		errorRate := float64(errors) / float64(max(1, len(values)))
		p95 := percentile(values, 0.95)
		byKind[kind] = map[string]any{
			"requests":  len(values),
			"errors":    errors,
			"errorRate": errorRate,
			"p50MS":     percentile(values, 0.50).Milliseconds(),
			"p95MS":     p95.Milliseconds(),
			"maxMS":     maxValue.Milliseconds(),
		}
		if errorRate > 0.01 || (kind == "poll" && (p95 > 2*time.Second || maxValue > 5*time.Second)) || (kind != "poll" && p95 > 2*time.Second) {
			guardrailFailed = true
		}
	}
	result["measurements"] = byKind
	result["guardrailsPassed"] = !guardrailFailed
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return err
	}
	if guardrailFailed {
		return errors.New("one or more load guardrails failed")
	}
	return nil
}
