package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"tronbyt-server/internal/data"

	"gorm.io/gorm"
)

func TestBrightnessHeaderChangesWithoutFrameTransition(t *testing.T) {
	s := newTestServerAPI(t)
	ctx := context.Background()
	path := "pushed:brightness"
	app := data.App{DeviceID: "testdevice", Iname: "brightness", Name: "Static", Enabled: true, Pushed: true, PushKind: persistentPushKind, Path: &path}
	if err := s.DB.Create(&app).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.savePushedImage("testdevice", "brightness", "", []byte("same-frame")); err != nil {
		t.Fatal(err)
	}

	poll := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/testdevice/next", nil)
		req.SetPathValue("id", "testdevice")
		rr := httptest.NewRecorder()
		s.handleNextApp(rr, req)
		return rr
	}

	_, err := gorm.G[data.Device](s.DB).Where("id = ?", "testdevice").Update(ctx, "brightness", data.Brightness(20))
	if err != nil {
		t.Fatal(err)
	}
	first := poll()
	_, err = gorm.G[data.Device](s.DB).Where("id = ?", "testdevice").Update(ctx, "brightness", data.Brightness(80))
	if err != nil {
		t.Fatal(err)
	}
	second := poll()

	if first.Header().Get("Tronbyt-Brightness") != "20" || second.Header().Get("Tronbyt-Brightness") != "80" {
		t.Fatalf("brightness headers did not update immediately: first=%q second=%q", first.Header().Get("Tronbyt-Brightness"), second.Header().Get("Tronbyt-Brightness"))
	}
	if first.Body.String() != second.Body.String() {
		t.Fatal("brightness-only update unexpectedly changed the frame")
	}
}

func TestConcurrentHTTPPollsConsumeDistinctOneShotFrames(t *testing.T) {
	s := newTestServerAPI(t)
	if err := s.savePushedImage("testdevice", "", "first", []byte("frame-one")); err != nil {
		t.Fatal(err)
	}
	if err := s.savePushedImage("testdevice", "", "second", []byte("frame-two")); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan string, 2)
	for range 2 {
		go func() {
			<-start
			req := httptest.NewRequest(http.MethodGet, "/testdevice/next", nil)
			req.SetPathValue("id", "testdevice")
			rr := httptest.NewRecorder()
			s.handleNextApp(rr, req)
			if rr.Code != http.StatusOK {
				results <- "status-error"
				return
			}
			results <- rr.Body.String()
		}()
	}
	close(start)
	frames := []string{<-results, <-results}
	sort.Strings(frames)
	if frames[0] != "frame-one" || frames[1] != "frame-two" {
		t.Fatalf("concurrent polls returned %q; expected both one-shot frames exactly once", frames)
	}
}

func TestHandleNextApp(t *testing.T) {
	s := newTestServerAPI(t)
	var device data.Device
	s.DB.First(&device, "id = ?", "testdevice")

	path := "pushed:1"
	app := data.App{
		DeviceID:  "testdevice",
		Iname:     "1",
		Name:      "Test App",
		UInterval: 10,
		Enabled:   true,
		Pushed:    true,
		PushKind:  persistentPushKind,
		Path:      &path,
	}
	if err := s.DB.Create(&app).Error; err != nil {
		t.Fatalf("Failed to create app: %v", err)
	}

	if err := s.savePushedImage("testdevice", "testapp", "", []byte("dummy image")); err != nil {
		t.Fatalf("Failed to save pushed image: %v", err)
	}
	if err := s.savePushedImage("testdevice", "1", "", []byte("dummy image")); err != nil {
		t.Fatalf("Failed to save selected pushed image: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/testdevice/next", nil)
	req.SetPathValue("id", "testdevice")

	rr := httptest.NewRecorder()

	s.handleNextApp(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			rr.Code, http.StatusOK)
	}

	if rr.Header().Get("Content-Type") != "image/webp" {
		t.Errorf("Expected content type image/webp, got %s", rr.Header().Get("Content-Type"))
	}
	if rr.Header().Get("Tronbyt-App") != "Test App" {
		t.Errorf("Expected selected app response header, got %q", rr.Header().Get("Tronbyt-App"))
	}
	if rr.Header().Get("Tronbyt-Installation") != "1" {
		t.Errorf("Expected selected installation response header, got %q", rr.Header().Get("Tronbyt-Installation"))
	}
}

func TestHandleNextApp_FirmwareUpdate(t *testing.T) {
	s := newTestServerAPI(t)

	// Ensure device exists
	device := data.Device{
		ID:       "fwdevice",
		Username: "admin",
		Info: data.DeviceInfo{
			ProtocolType: data.ProtocolWS, // Start with WS to test protocol update to HTTP
		},
	}
	s.DB.Create(&device)

	req := httptest.NewRequest(http.MethodGet, "/fwdevice/next", nil)
	req.SetPathValue("id", "fwdevice")
	req.Header.Set("X-Firmware-Version", "v1.5.0")

	rr := httptest.NewRecorder()
	s.handleNextApp(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}

	// Verify DB update
	var updatedDevice data.Device
	if err := s.DB.First(&updatedDevice, "id = ?", "fwdevice").Error; err != nil {
		t.Fatalf("Failed to fetch device: %v", err)
	}

	if updatedDevice.Info.ProtocolType != data.ProtocolHTTP {
		t.Errorf("Expected protocol HTTP, got %s", updatedDevice.Info.ProtocolType)
	}
	if updatedDevice.Info.FirmwareVersion != "v1.5.0" {
		t.Errorf("Expected firmware version v1.5.0, got %s", updatedDevice.Info.FirmwareVersion)
	}
}

func TestHandleNextApp_PendingHTTPHeaders(t *testing.T) {
	s := newTestServerAPI(t)

	device := data.Device{
		ID:               "httpdevice",
		Username:         "admin",
		PendingUpdateURL: "http://example.com/firmware.bin",
		PendingImageURL:  "http://example.com/new-next",
		PendingReboot:    true,
	}
	if err := s.DB.Create(&device).Error; err != nil {
		t.Fatalf("Failed to create device: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/httpdevice/next", nil)
	req.SetPathValue("id", "httpdevice")

	rr := httptest.NewRecorder()
	s.handleNextApp(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}

	if got := rr.Header().Get("Tronbyt-OTA-URL"); got != "http://example.com/firmware.bin" {
		t.Errorf("Expected Tronbyt-OTA-URL header, got %q", got)
	}
	if got := rr.Header().Get("Tronbyt-Image-URL"); got != "http://example.com/new-next" {
		t.Errorf("Expected Tronbyt-Image-URL header, got %q", got)
	}
	if got := rr.Header().Get("Tronbyt-Reboot"); got != "true" {
		t.Errorf("Expected Tronbyt-Reboot header, got %q", got)
	}

	var updatedDevice data.Device
	if err := s.DB.First(&updatedDevice, "id = ?", "httpdevice").Error; err != nil {
		t.Fatalf("Failed to fetch device: %v", err)
	}
	if updatedDevice.PendingUpdateURL != "" {
		t.Errorf("Expected pending update URL to be cleared, got %q", updatedDevice.PendingUpdateURL)
	}
	if updatedDevice.PendingImageURL != "" {
		t.Errorf("Expected pending image URL to be cleared, got %q", updatedDevice.PendingImageURL)
	}
	if updatedDevice.PendingReboot {
		t.Error("Expected pending reboot to be cleared")
	}
}

func TestHandleNextApp_APIKey(t *testing.T) {
	s := newTestServerAPI(t)

	// Update device to require API key
	var device data.Device
	s.DB.First(&device, "id = ?", "testdevice")
	s.DB.Model(&device).Update("require_api_key", true)

	// Add an app so /next returns something
	path := "pushed:1"
	app := data.App{
		DeviceID:  "testdevice",
		Iname:     "1",
		Name:      "Test App",
		UInterval: 10,
		Enabled:   true,
		Pushed:    true,
		Path:      &path,
	}
	s.DB.Create(&app)
	if err := s.savePushedImage("testdevice", "testapp", "", []byte("dummy image")); err != nil {
		t.Fatalf("Failed to save pushed image: %v", err)
	}

	t.Run("without API key returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/testdevice/next", nil)
		req.SetPathValue("id", "testdevice")

		rr := httptest.NewRecorder()
		s.handleNextApp(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("Expected status 401 Unauthorized, got %v", rr.Code)
		}
	})

	t.Run("with incorrect API key returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/testdevice/next?key=wrongkey", nil)
		req.SetPathValue("id", "testdevice")

		rr := httptest.NewRecorder()
		s.handleNextApp(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("Expected status 401 Unauthorized, got %v", rr.Code)
		}
	})

	t.Run("with correct API key via query param returns 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/testdevice/next?key=device_api_key", nil)
		req.SetPathValue("id", "testdevice")

		rr := httptest.NewRecorder()
		s.handleNextApp(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("Expected status 200 OK, got %v", rr.Code)
		}
	})

	t.Run("with correct API key via Bearer header returns 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/testdevice/next", nil)
		req.SetPathValue("id", "testdevice")
		req.Header.Set("Authorization", "Bearer device_api_key")

		rr := httptest.NewRecorder()
		s.handleNextApp(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("Expected status 200 OK, got %v", rr.Code)
		}
	})
}

func TestHandleWS_APIKey(t *testing.T) {
	s := newTestServerAPI(t)

	// Update device to require API key
	var device data.Device
	s.DB.First(&device, "id = ?", "testdevice")
	s.DB.Model(&device).Update("require_api_key", true)

	t.Run("without API key returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v0/devices/testdevice/ws", nil)
		req.SetPathValue("id", "testdevice")

		rr := httptest.NewRecorder()
		s.handleWS(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("Expected status 401 Unauthorized, got %v", rr.Code)
		}
	})

	t.Run("with incorrect API key returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v0/devices/testdevice/ws?key=wrongkey", nil)
		req.SetPathValue("id", "testdevice")

		rr := httptest.NewRecorder()
		s.handleWS(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("Expected status 401 Unauthorized, got %v", rr.Code)
		}
	})

	t.Run("with correct API key via query param does not return 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v0/devices/testdevice/ws?key=device_api_key", nil)
		req.SetPathValue("id", "testdevice")

		rr := httptest.NewRecorder()
		s.handleWS(rr, req)

		if rr.Code == http.StatusUnauthorized {
			t.Errorf("Expected status not 401 Unauthorized, got %v", rr.Code)
		}
	})

	t.Run("with correct API key via Bearer header does not return 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v0/devices/testdevice/ws", nil)
		req.SetPathValue("id", "testdevice")
		req.Header.Set("Authorization", "Bearer device_api_key")

		rr := httptest.NewRecorder()
		s.handleWS(rr, req)

		if rr.Code == http.StatusUnauthorized {
			t.Errorf("Expected status not 401 Unauthorized, got %v", rr.Code)
		}
	})
}
