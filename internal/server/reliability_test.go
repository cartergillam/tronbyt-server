package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tronbyt-server/internal/data"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestApplyDeviceRenderContextLocationInheritance(t *testing.T) {
	tz := "America/Toronto"
	device := &data.Device{
		Timezone: &tz,
		Location: data.DeviceLocation{
			Description: "Caledonia, ON, Canada",
			Locality:    "Caledonia",
			Lat:         43.0738,
			Lng:         -79.9519,
			Timezone:    tz,
		},
	}

	inherited := map[string]any{"location": "__device__"}
	applyDeviceRenderContext(inherited, device)
	assert.Equal(t, tz, inherited["$tz"])
	assert.Equal(t, inherited["$location"], inherited["location"])
	assert.Contains(t, inherited["location"], "Caledonia")

	overridden := map[string]any{"location": "New York, NY"}
	applyDeviceRenderContext(overridden, device)
	assert.Equal(t, "New York, NY", overridden["location"], "explicit app location must override the device default")
	assert.Contains(t, overridden["$location"], "Caledonia")
}

func TestCleanupTemporaryPushLifecycle(t *testing.T) {
	s := newTestServerAPI(t)
	ctx := context.Background()
	legacyPath := "pushed:legacy"
	persistentPath := "pushed:persistent"
	stalePath := "pushed:missing"
	temporaryPath := "pushed:__123_show-now"
	legacy := data.App{DeviceID: "testdevice", Iname: "101", Name: "pushed", Pushed: true, Enabled: true, Order: 0, Path: &legacyPath}
	persistent := data.App{DeviceID: "testdevice", Iname: "102", Name: "pushed", Pushed: true, PushKind: persistentPushKind, Enabled: true, Order: 1, Path: &persistentPath}
	stale := data.App{DeviceID: "testdevice", Iname: "103", Name: "pushed", Pushed: true, Enabled: true, Order: 2, Path: &stalePath}
	temporary := data.App{DeviceID: "testdevice", Iname: "104", Name: "pushed", Pushed: true, Enabled: true, Order: 3, Path: &temporaryPath}
	require.NoError(t, gorm.G[data.App](s.DB).Create(ctx, &legacy))
	require.NoError(t, gorm.G[data.App](s.DB).Create(ctx, &persistent))
	require.NoError(t, gorm.G[data.App](s.DB).Create(ctx, &stale))
	require.NoError(t, gorm.G[data.App](s.DB).Create(ctx, &temporary))

	device, err := gorm.G[data.Device](s.DB).Where("id = ?", "testdevice").First(ctx)
	require.NoError(t, err)
	device.DisplayingApp = &stale.Iname
	device.DisplayRestoreApp = &stale.Iname
	device.PinnedApp = &temporary.Iname
	device.InterstitialApp = &temporary.Iname
	device.InterstitialEnabled = true
	device.LastAppIndex = 99
	require.NoError(t, s.DB.Omit("Apps").Save(&device).Error)

	dir := filepath.Join(s.DataDir, "webp", device.ID, "pushed")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for _, name := range []string{"legacy.webp", "persistent.webp", "orphan.webp", "__123_show-now.webp", "__stale.webp"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644))
	}
	staleTime := time.Now().Add(-temporaryPushMaximumAge - time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "__stale.webp"), staleTime, staleTime))

	dryRun, err := s.inspectOrCleanupPushLifecycle(ctx, true)
	require.NoError(t, err)
	assert.Equal(t, 1, dryRun.LegacyRowsMigrated)
	assert.Equal(t, 2, dryRun.StaleRowsRemoved)
	assert.Equal(t, 1, dryRun.OrphanFilesRemoved)
	assert.Equal(t, 1, dryRun.StaleTemporaryFiles)
	unchanged, err := gorm.G[data.App](s.DB).Where("id = ?", legacy.ID).First(ctx)
	require.NoError(t, err)
	assert.Empty(t, unchanged.PushKind, "dry-run must not classify rows")

	require.NoError(t, s.cleanupTemporaryPushLifecycle(ctx))
	legacy, err = gorm.G[data.App](s.DB).Where("id = ?", legacy.ID).First(ctx)
	require.NoError(t, err)
	assert.Equal(t, persistentPushKind, legacy.PushKind, "legacy stable pushes are migrated, not deleted")
	_, err = gorm.G[data.App](s.DB).Where("id = ?", persistent.ID).First(ctx)
	require.NoError(t, err)
	_, err = gorm.G[data.App](s.DB).Where("id = ?", stale.ID).First(ctx)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	_, err = gorm.G[data.App](s.DB).Where("id = ?", temporary.ID).First(ctx)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)

	device, err = gorm.G[data.Device](s.DB).Where("id = ?", device.ID).First(ctx)
	require.NoError(t, err)
	assert.Nil(t, device.DisplayingApp)
	assert.Nil(t, device.DisplayRestoreApp)
	assert.Nil(t, device.PinnedApp)
	assert.Nil(t, device.InterstitialApp)
	assert.False(t, device.InterstitialEnabled)
	assert.Zero(t, device.LastAppIndex)

	_, err = os.Stat(filepath.Join(dir, "legacy.webp"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "orphan.webp"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(dir, "persistent.webp"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "__123_show-now.webp"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "__stale.webp"))
	assert.True(t, os.IsNotExist(err))

	second, err := s.inspectOrCleanupPushLifecycle(ctx, false)
	require.NoError(t, err)
	assert.Zero(t, second.LegacyRowsMigrated)
	assert.Zero(t, second.StaleRowsRemoved)
	assert.Zero(t, second.OrphanFilesRemoved)
	assert.Zero(t, second.StaleTemporaryFiles)
}

func TestClassifyRenderResultBoundaries(t *testing.T) {
	now := time.Date(2026, time.July, 31, 23, 30, 0, 0, time.UTC)

	t.Run("visible clock boundary", func(t *testing.T) {
		boundary := now.Add(time.Minute)
		result, message, next := classifyRenderResult(now, []byte("frame"), []string{nextRenderMarker + boundary.Format(time.RFC3339)}, nil, 15)
		assert.Equal(t, "visible", result)
		assert.Empty(t, message)
		require.NotNil(t, next)
		assert.Equal(t, boundary, *next)
	})

	t.Run("final second keeps exact minute boundary", func(t *testing.T) {
		at125959 := time.Date(2026, time.July, 31, 16, 34, 59, 250_000_000, time.UTC)
		at123500 := time.Date(2026, time.July, 31, 16, 35, 0, 0, time.UTC)
		result, _, next := classifyRenderResult(
			at125959,
			[]byte("12:34"),
			[]string{nextRenderMarker + at123500.Format(time.RFC3339)},
			nil,
			1,
		)
		assert.Equal(t, "visible", result)
		require.NotNil(t, next)
		assert.Equal(t, at123500, *next)
	})

	t.Run("no game retries quickly", func(t *testing.T) {
		result, _, next := classifyRenderResult(now, nil, []string{"--- APPLET HIDDEN FROM ROTATION (NO GAME TODAY) ---"}, nil, 360)
		assert.Equal(t, "hidden", result)
		require.NotNil(t, next)
		assert.Equal(t, now.Add(30*time.Minute), *next)
	})

	t.Run("upstream failure beats hidden marker", func(t *testing.T) {
		result, message, next := classifyRenderResult(now, nil, []string{
			renderFailureMarker + " MLB schedule temporarily unavailable",
			"--- APPLET HIDDEN FROM ROTATION (NO GAME TODAY) ---",
		}, nil, 360)
		assert.Equal(t, "upstream_failure", result)
		assert.Equal(t, "MLB schedule temporarily unavailable", message)
		require.NotNil(t, next)
		assert.Equal(t, now.Add(2*time.Minute), *next)
	})

	t.Run("renderer error wins", func(t *testing.T) {
		result, message, next := classifyRenderResult(now, nil, nil, errors.New("private.example/path?token=secret"), 15)
		assert.Equal(t, "failure", result)
		assert.NotContains(t, message, "token=secret")
		require.NotNil(t, next)
		assert.Equal(t, now.Add(2*time.Minute), *next)
	})
}

func TestClockRenderDueAndBoundaryDwell(t *testing.T) {
	boundary := time.Date(2026, time.November, 1, 6, 0, 0, 0, time.UTC)
	lastRender := boundary.Add(-30 * time.Second)
	app := &data.App{LastRender: lastRender, UInterval: 15, NextRenderAt: &boundary}

	assert.False(t, renderDue(boundary.Add(-time.Nanosecond), app), "cached frame remains valid before its exact boundary")
	assert.True(t, renderDue(boundary, app), "delayed or exact-boundary polling must invalidate the cached minute")
	assert.True(t, renderDue(boundary.Add(8*time.Second), app), "a delayed poll must render the current minute")
	assert.Equal(t, 1, effectiveFrameDwell(boundary.Add(-750*time.Millisecond), 15, app))
	assert.Equal(t, 10, effectiveFrameDwell(boundary.Add(-10*time.Second), 15, app))
	assert.Equal(t, 15, effectiveFrameDwell(boundary.Add(-30*time.Second), 15, app))
}

func TestDeviceEventTimelineIsBoundedAndRecordsHealthTransitions(t *testing.T) {
	timeline := newDeviceEventTimeline(3)
	for i := range 5 {
		timeline.add("device", "render", fmt.Sprintf("event %d", i), "")
	}
	require.Len(t, timeline.recent("device", 10), 3)

	timeline.setHealth("device", "stale")
	timeline.setHealth("device", "stale")
	timeline.setHealth("device", "connected")
	events := timeline.recent("device", 10)
	require.Len(t, events, 3)
	assert.Equal(t, "device_resumed", events[0].Type)
	assert.Equal(t, "device_stale", events[1].Type)
}

// The production-lineage fixtures intentionally describe only the columns
// written by an older server. Values are synthetic and contain no exported
// production identifiers, credentials, or location precision.
type productionLineageUser struct {
	Username string `gorm:"primaryKey"`
	Password string
	APIKey   string `gorm:"uniqueIndex"`
}

func (productionLineageUser) TableName() string { return "users" }

type productionLineageDevice struct {
	ID                  string `gorm:"primaryKey"`
	Username            string `gorm:"index"`
	Name                string
	Type                data.DeviceType `gorm:"type:text"`
	APIKey              string          `gorm:"uniqueIndex"`
	Brightness          data.Brightness
	NightModeEnabled    bool
	NightModeApp        string
	NightStart          string
	NightEnd            string
	NightBrightness     data.Brightness
	DefaultInterval     int
	Timezone            *string
	Location            data.DeviceLocation `gorm:"type:text"`
	LastAppIndex        int
	DisplayingApp       *string
	DisplayRestoreApp   *string
	PinnedApp           *string
	InterstitialEnabled bool
	InterstitialApp     *string
	RequireAPIKey       bool
}

func (productionLineageDevice) TableName() string { return "devices" }

type productionLineageApp struct {
	ID              uint   `gorm:"primaryKey"`
	DeviceID        string `gorm:"index:idx_device_order,priority:1;uniqueIndex:idx_device_iname,priority:1;type:string"`
	Iname           string `gorm:"uniqueIndex:idx_device_iname,priority:2"`
	Name            string
	UInterval       int
	DisplayTime     int
	Enabled         bool
	Pushed          bool
	Order           int `gorm:"index:idx_device_order,priority:2"`
	LastRender      time.Time
	Path            *string
	Config          data.JSONMap `gorm:"type:text"`
	EmptyLastRender bool
}

func (productionLineageApp) TableName() string { return "apps" }

func TestProductionLineageMigrationAndCleanupRehearsal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "production-lineage.db")
	openDB := func() *gorm.DB {
		db, err := gorm.Open(sqlite.Open(dbPath+"?_busy_timeout=5000"), &gorm.Config{})
		require.NoError(t, err)
		return db
	}

	legacyDB := openDB()
	require.NoError(t, legacyDB.AutoMigrate(
		&productionLineageUser{},
		&productionLineageDevice{},
		&productionLineageApp{},
	))
	require.False(t, legacyDB.Migrator().HasColumn(&productionLineageApp{}, "push_kind"))
	require.False(t, legacyDB.Migrator().HasColumn(&productionLineageApp{}, "last_render_result"))

	timezone := "America/Toronto"
	missingIname := "missing-push"
	legacyStablePath := "pushed:legacy-stable"
	missingPath := "pushed:missing-file"
	temporaryPath := "pushed:__current-show-now"
	user := productionLineageUser{
		Username: "rehearsal-user",
		Password: "synthetic-password-hash",
		APIKey:   "synthetic-user-key",
	}
	device := productionLineageDevice{
		ID:                  "fixture-device",
		Username:            user.Username,
		Name:                "Migration Rehearsal",
		Type:                data.DeviceMatrixPortal,
		APIKey:              "synthetic-device-key",
		Brightness:          62,
		NightModeEnabled:    true,
		NightModeApp:        "clock",
		NightStart:          "22:00",
		NightEnd:            "07:00",
		NightBrightness:     8,
		DefaultInterval:     15,
		Timezone:            &timezone,
		Location:            data.DeviceLocation{Description: "Fixture City, Canada", Locality: "Fixture City", Country: "Canada", Lat: 43.1, Lng: -79.9, Timezone: timezone},
		LastAppIndex:        99,
		DisplayingApp:       &missingIname,
		DisplayRestoreApp:   &missingIname,
		InterstitialEnabled: true,
		InterstitialApp:     &missingIname,
		RequireAPIKey:       true,
	}
	apps := []productionLineageApp{
		{DeviceID: device.ID, Iname: "clock", Name: "Clock", Enabled: true, Order: 0, Config: data.JSONMap{"timezone": "__device__"}},
		{DeviceID: device.ID, Iname: "mlb-game", Name: "MLB Game", Enabled: true, Order: 1, Config: data.JSONMap{"team": "TOR", "gameday_only": "true"}},
		{DeviceID: device.ID, Iname: "legacy-push", Name: "pushed", Enabled: true, Pushed: true, Order: 2, Path: &legacyStablePath},
		{DeviceID: device.ID, Iname: missingIname, Name: "pushed", Enabled: true, Pushed: true, Order: 3, Path: &missingPath},
		{DeviceID: device.ID, Iname: "temporary-row", Name: "pushed", Enabled: true, Pushed: true, Order: 4, Path: &temporaryPath},
	}
	require.NoError(t, legacyDB.Create(&user).Error)
	require.NoError(t, legacyDB.Create(&device).Error)
	require.NoError(t, legacyDB.Create(&apps).Error)

	pushedDir := filepath.Join(root, "webp", device.ID, "pushed")
	require.NoError(t, os.MkdirAll(pushedDir, 0o755))
	for _, name := range []string{"legacy-stable.webp", "orphan-stable.webp", "__current-show-now.webp", "__expired-show-now.webp"} {
		require.NoError(t, os.WriteFile(filepath.Join(pushedDir, name), []byte("synthetic "+name), 0o644))
	}
	expired := time.Now().Add(-temporaryPushMaximumAge - time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(pushedDir, "__expired-show-now.webp"), expired, expired))

	legacySQL, err := legacyDB.DB()
	require.NoError(t, err)
	require.NoError(t, legacySQL.Close())

	db := openDB()
	require.NoError(t, db.AutoMigrate(
		&data.User{},
		&data.Device{},
		&data.App{},
		&data.WebAuthnCredential{},
		&data.Setting{},
		&data.OIDCIdentity{},
	))
	require.True(t, db.Migrator().HasColumn(&data.App{}, "push_kind"))
	require.True(t, db.Migrator().HasColumn(&data.App{}, "last_render_result"))
	require.True(t, db.Migrator().HasColumn(&data.App{}, "next_render_at"))

	s := &Server{DB: db, DataDir: root, diagnosticsEvents: newDeviceEventTimeline(20)}
	var beforeApps int64
	require.NoError(t, db.Model(&data.App{}).Count(&beforeApps).Error)
	beforeDevice, err := gorm.G[data.Device](db).Where("id = ?", device.ID).First(ctx)
	require.NoError(t, err)

	dryRun, err := s.inspectOrCleanupPushLifecycle(ctx, true)
	require.NoError(t, err)
	assert.Equal(t, 1, dryRun.LegacyRowsMigrated)
	assert.Equal(t, 2, dryRun.StaleRowsRemoved)
	assert.Equal(t, 1, dryRun.OrphanFilesRemoved)
	assert.Equal(t, 1, dryRun.StaleTemporaryFiles)
	var afterDryRunApps int64
	require.NoError(t, db.Model(&data.App{}).Count(&afterDryRunApps).Error)
	assert.Equal(t, beforeApps, afterDryRunApps)
	afterDryRunDevice, err := gorm.G[data.Device](db).Where("id = ?", device.ID).First(ctx)
	require.NoError(t, err)
	assert.Equal(t, beforeDevice.DisplayingApp, afterDryRunDevice.DisplayingApp)
	for _, name := range []string{"legacy-stable.webp", "orphan-stable.webp", "__current-show-now.webp", "__expired-show-now.webp"} {
		_, statErr := os.Stat(filepath.Join(pushedDir, name))
		require.NoError(t, statErr, "dry-run must preserve %s", name)
	}

	realRun, err := s.inspectOrCleanupPushLifecycle(ctx, false)
	require.NoError(t, err)
	assert.Equal(t, dryRun, realRun)
	var migrated []data.App
	require.NoError(t, db.Where("device_id = ?", device.ID).Order("`order`").Find(&migrated).Error)
	assert.Equal(t, []string{"clock", "mlb-game", "legacy-push"}, []string{migrated[0].Iname, migrated[1].Iname, migrated[2].Iname})
	assert.Equal(t, persistentPushKind, migrated[2].PushKind)

	migratedDevice, err := gorm.G[data.Device](db).Where("id = ?", device.ID).First(ctx)
	require.NoError(t, err)
	assert.Nil(t, migratedDevice.DisplayingApp)
	assert.Nil(t, migratedDevice.DisplayRestoreApp)
	assert.Nil(t, migratedDevice.InterstitialApp)
	assert.False(t, migratedDevice.InterstitialEnabled)
	assert.Equal(t, "clock", migratedDevice.NightModeApp)
	assert.Equal(t, timezone, migratedDevice.Location.Timezone)
	assert.Equal(t, "Fixture City", migratedDevice.Location.Locality)
	assert.Zero(t, migratedDevice.LastAppIndex)

	_, err = os.Stat(filepath.Join(pushedDir, "legacy-stable.webp"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(pushedDir, "__current-show-now.webp"))
	require.NoError(t, err)
	for _, name := range []string{"orphan-stable.webp", "__expired-show-now.webp"} {
		_, statErr := os.Stat(filepath.Join(pushedDir, name))
		assert.True(t, os.IsNotExist(statErr), "%s should be removed", name)
	}

	// An older binary selecting only its known columns can still read ordinary
	// installations after the additive migration.
	var legacyReadable []productionLineageApp
	require.NoError(t, db.Where("device_id = ? AND pushed = ?", device.ID, false).Order("`order`").Find(&legacyReadable).Error)
	assert.Equal(t, []string{"clock", "mlb-game"}, []string{legacyReadable[0].Iname, legacyReadable[1].Iname})

	second, err := s.inspectOrCleanupPushLifecycle(ctx, false)
	require.NoError(t, err)
	assert.Zero(t, second.LegacyRowsMigrated)
	assert.Zero(t, second.StaleRowsRemoved)
	assert.Zero(t, second.OrphanFilesRemoved)
	assert.Zero(t, second.StaleTemporaryFiles)
}
