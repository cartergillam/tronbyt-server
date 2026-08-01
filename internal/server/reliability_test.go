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
