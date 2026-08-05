package server

import (
	"context"
	"os"
	"sort"

	"tronbyt-server/internal/data"

	"gorm.io/gorm"
)

func displayRestoreTarget(device *data.Device) *data.App {
	if device.DisplayingApp != nil {
		if current := device.GetApp(*device.DisplayingApp); current != nil && current.Enabled && !current.Pushed {
			return current
		}
	}
	apps := make([]*data.App, 0, len(device.Apps))
	for _, app := range device.Apps {
		if app.Enabled && !app.Pushed {
			apps = append(apps, app)
		}
	}
	sort.SliceStable(apps, func(i, j int) bool { return apps[i].Order < apps[j].Order })
	if len(apps) > 0 {
		start := device.LastAppIndex
		if start < 0 || start >= len(apps) {
			start = 0
		}
		return apps[start]
	}
	return nil
}

func (s *Server) applySleepState(ctx context.Context, device *data.Device, sleeping bool) error {
	_, err := s.clearTemporaryPushFiles(device.ID)
	if err != nil {
		return err
	}
	var target *data.App
	if device.DisplayRestoreApp != nil {
		candidate := device.GetApp(*device.DisplayRestoreApp)
		if candidate != nil && candidate.Enabled && !candidate.Pushed {
			target = candidate
		}
	}
	if target == nil {
		target = displayRestoreTarget(device)
	}
	var restoreID *string
	if target != nil {
		value := target.Iname
		restoreID = &value
	}
	device.Sleeping = sleeping
	device.DisplayingApp = nil
	device.DisplayRestoreApp = restoreID
	device.ActiveShowNowApp = nil
	device.ShowNowRestoreApp = nil
	if sleeping {
		s.diagnosticsEvents.add(device.ID, "display_sleep", "Display entered sleep without changing rotation", "")
	} else {
		s.diagnosticsEvents.add(device.ID, "display_wake", "Display wake scheduled normal rotation restore", optionalString(restoreID))
	}
	return nil
}

func (s *Server) prepareDisplayRestore(ctx context.Context, device *data.Device) error {
	target := displayRestoreTarget(device)
	var restoreID *string
	if target != nil {
		value := target.Iname
		restoreID = &value
	}
	_, err := gorm.G[data.Device](s.DB).Where("id = ?", device.ID).
		Select("DisplayRestoreApp", "DisplayingApp").
		Updates(ctx, data.Device{
			DisplayRestoreApp: restoreID,
			DisplayingApp:     nil,
		})
	if err == nil {
		device.DisplayRestoreApp = restoreID
		device.DisplayingApp = nil
	}
	return err
}

func (s *Server) restoreDisplayAfterPowerOn(ctx context.Context, device *data.Device) error {
	var target *data.App
	if device.DisplayRestoreApp != nil {
		target = device.GetApp(*device.DisplayRestoreApp)
	}
	if target == nil || target.Pushed || !target.Enabled {
		target = displayRestoreTarget(device)
	}
	if target == nil {
		_, err := gorm.G[data.Device](s.DB).Where("id = ?", device.ID).
			Select("DisplayRestoreApp", "DisplayingApp").
			Updates(ctx, data.Device{
				DisplayRestoreApp: nil,
				DisplayingApp:     nil,
			})
		return err
	}

	restoreID := target.Iname
	index := expandedIndexForInstallation(device, restoreID)
	_, err := gorm.G[data.Device](s.DB).Where("id = ?", device.ID).
		Select("DisplayRestoreApp", "DisplayingApp", "LastAppIndex").
		Updates(ctx, data.Device{
			DisplayRestoreApp: &restoreID,
			DisplayingApp:     &restoreID,
			LastAppIndex:      index,
		})
	if err != nil {
		return err
	}
	device.DisplayRestoreApp = &restoreID
	device.DisplayingApp = &restoreID
	device.LastAppIndex = index

	webpDir, err := s.ensureDeviceImageDir(device.ID)
	if err == nil {
		if image, readErr := os.ReadFile(s.getAppWebpPath(webpDir, target)); readErr == nil && len(image) > 0 {
			s.Broadcaster.Notify(device.ID, image)
		}
	}
	return nil
}
