package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"tronbyt-server/internal/data"

	"gorm.io/gorm"
)

const (
	persistentPushKind      = "persistent"
	temporaryPushMaximumAge = 24 * time.Hour
)

type pushCleanupReport struct {
	DevicesInspected        int `json:"devicesInspected"`
	LegacyRowsMigrated      int `json:"legacyRowsMigrated"`
	StaleRowsRemoved        int `json:"staleRowsRemoved"`
	ReferencesCleared       int `json:"referencesCleared"`
	OrdersRepaired          int `json:"ordersRepaired"`
	OrphanFilesRemoved      int `json:"orphanFilesRemoved"`
	StaleTemporaryFiles     int `json:"staleTemporaryFiles"`
	TemporaryFilesPreserved int `json:"temporaryFilesPreserved"`
}

// cleanupTemporaryPushLifecycle is idempotent. Database classification and
// reference repair commit before any files are removed, so an interrupted file
// phase can safely be retried on the next startup.
func (s *Server) cleanupTemporaryPushLifecycle(ctx context.Context) error {
	dryRun := strings.EqualFold(strings.TrimSpace(os.Getenv("TRONBYT_PUSH_CLEANUP_DRY_RUN")), "true")
	report, err := s.inspectOrCleanupPushLifecycle(ctx, dryRun)
	if err == nil {
		slog.Info("Push lifecycle cleanup complete",
			"dry_run", dryRun,
			"devices", report.DevicesInspected,
			"legacy_migrated", report.LegacyRowsMigrated,
			"stale_rows", report.StaleRowsRemoved,
			"references_cleared", report.ReferencesCleared,
			"orders_repaired", report.OrdersRepaired,
			"orphan_files", report.OrphanFilesRemoved,
			"stale_temporary_files", report.StaleTemporaryFiles,
			"temporary_preserved", report.TemporaryFilesPreserved,
		)
	}
	return err
}

// inspectOrCleanupPushLifecycle provides the same classification in dry-run
// mode without mutating the database or filesystem.
func (s *Server) inspectOrCleanupPushLifecycle(ctx context.Context, dryRun bool) (pushCleanupReport, error) {
	var report pushCleanupReport
	devices, err := gorm.G[data.Device](s.DB).Preload("Apps", nil).Find(ctx)
	if err != nil {
		return report, err
	}
	for i := range devices {
		device := &devices[i]
		report.DevicesInspected++
		pushedDir := filepath.Join(s.DataDir, "webp", device.ID, "pushed")
		entries, readErr := os.ReadDir(pushedDir)
		if readErr != nil && !os.IsNotExist(readErr) {
			return report, readErr
		}
		files := make(map[string]bool, len(entries))
		staleTemporaryFiles := make(map[string]bool)
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".webp") {
				continue
			}
			files[entry.Name()] = true
			if strings.HasPrefix(entry.Name(), "__") {
				info, infoErr := entry.Info()
				if infoErr != nil {
					return report, fmt.Errorf("inspect temporary push for device %s: %w", device.ID, infoErr)
				}
				if time.Since(info.ModTime()) > temporaryPushMaximumAge {
					staleTemporaryFiles[entry.Name()] = true
					report.StaleTemporaryFiles++
				} else {
					report.TemporaryFilesPreserved++
				}
			}
		}

		validFiles := map[string]bool{}
		migrateIDs := []uint{}
		removeIDs := []uint{}
		removedInames := map[string]bool{}
		for _, app := range device.Apps {
			if !app.Pushed {
				continue
			}
			pushID := ""
			if app.Path != nil && strings.HasPrefix(*app.Path, "pushed:") {
				pushID = strings.TrimPrefix(*app.Path, "pushed:")
			}
			filename := pushID + ".webp"
			hasStableFile := pushID != "" && files[filename]
			switch {
			case strings.HasPrefix(pushID, "__"):
				// One-shot content is file-backed. A legacy database row for the
				// same temporary frame is stale bookkeeping, never a persistent push.
				removeIDs = append(removeIDs, app.ID)
				removedInames[app.Iname] = true
				report.StaleRowsRemoved++
			case app.PushKind == persistentPushKind && hasStableFile:
				validFiles[filename] = true
			case app.PushKind == "" && hasStableFile:
				// Pre-classification rows with a stable file were the old persistent
				// representation. Preserve and classify them instead of guessing that
				// legitimate content is temporary.
				migrateIDs = append(migrateIDs, app.ID)
				validFiles[filename] = true
				report.LegacyRowsMigrated++
			default:
				removeIDs = append(removeIDs, app.ID)
				removedInames[app.Iname] = true
				report.StaleRowsRemoved++
			}
		}

		updates := map[string]any{}
		clearReference := func(field string, value *string) {
			if value != nil && removedInames[*value] {
				updates[field] = nil
				report.ReferencesCleared++
			}
		}
		clearReference("displaying_app", device.DisplayingApp)
		clearReference("display_restore_app", device.DisplayRestoreApp)
		clearReference("pinned_app", device.PinnedApp)
		if device.InterstitialApp != nil && removedInames[*device.InterstitialApp] {
			updates["interstitial_app"] = nil
			updates["interstitial_enabled"] = false
			report.ReferencesCleared++
		}

		remaining := make([]*data.App, 0, len(device.Apps)-len(removeIDs))
		for _, app := range device.Apps {
			if !containsUint(removeIDs, app.ID) {
				remaining = append(remaining, app)
			}
		}
		sort.SliceStable(remaining, func(i, j int) bool { return remaining[i].Order < remaining[j].Order })
		orderRepairs := map[uint]int{}
		for order, app := range remaining {
			if app.Order != order {
				orderRepairs[app.ID] = order
				report.OrdersRepaired++
			}
		}
		if len(removeIDs) > 0 {
			updates["last_app_index"] = 0
		}

		if !dryRun {
			if err := s.DB.Transaction(func(tx *gorm.DB) error {
				if len(migrateIDs) > 0 {
					if err := tx.Model(&data.App{}).Where("id IN ?", migrateIDs).Update("push_kind", persistentPushKind).Error; err != nil {
						return err
					}
				}
				if len(removeIDs) > 0 {
					if err := tx.Where("id IN ?", removeIDs).Delete(&data.App{}).Error; err != nil {
						return err
					}
				}
				if len(updates) > 0 {
					if err := tx.Model(&data.Device{ID: device.ID}).Updates(updates).Error; err != nil {
						return err
					}
				}
				for id, order := range orderRepairs {
					if err := tx.Model(&data.App{ID: id}).Update("order", order).Error; err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return report, fmt.Errorf("cleanup device %s database phase: %w", device.ID, err)
			}
		}

		// Stable orphan files are safe to delete only after the database phase.
		for filename := range files {
			if strings.HasPrefix(filename, "__") {
				if staleTemporaryFiles[filename] && !dryRun {
					if err := os.Remove(filepath.Join(pushedDir, filename)); err != nil && !os.IsNotExist(err) {
						return report, fmt.Errorf("remove stale temporary push for device %s: %w", device.ID, err)
					}
				}
				continue
			}
			if validFiles[filename] {
				continue
			}
			report.OrphanFilesRemoved++
			if !dryRun {
				if err := os.Remove(filepath.Join(pushedDir, filename)); err != nil && !os.IsNotExist(err) {
					return report, fmt.Errorf("remove orphan for device %s: %w", device.ID, err)
				}
			}
		}
	}
	return report, nil
}

func containsUint(values []uint, target uint) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (s *Server) removeMissingPersistentPush(ctx context.Context, device *data.Device, app *data.App) {
	app.Enabled = false
	updates := map[string]any{"last_app_index": 0}
	if device.DisplayingApp != nil && *device.DisplayingApp == app.Iname {
		updates["displaying_app"] = nil
		device.DisplayingApp = nil
	}
	if device.DisplayRestoreApp != nil && *device.DisplayRestoreApp == app.Iname {
		updates["display_restore_app"] = nil
		device.DisplayRestoreApp = nil
	}
	if device.PinnedApp != nil && *device.PinnedApp == app.Iname {
		updates["pinned_app"] = nil
		device.PinnedApp = nil
	}
	if device.InterstitialApp != nil && *device.InterstitialApp == app.Iname {
		updates["interstitial_app"] = nil
		updates["interstitial_enabled"] = false
		device.InterstitialApp = nil
		device.InterstitialEnabled = false
	}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&data.App{}, app.ID).Error; err != nil {
			return err
		}
		return tx.Model(&data.Device{ID: device.ID}).Updates(updates).Error
	}); err != nil {
		slog.Warn("Failed to remove missing persistent push", "device_id", device.ID, "installation_id", app.Iname, "error", err)
		return
	}
	device.LastAppIndex = 0
	slog.Warn("Removed persistent push with missing WebP", "device_id", device.ID, "installation_id", app.Iname)
}
