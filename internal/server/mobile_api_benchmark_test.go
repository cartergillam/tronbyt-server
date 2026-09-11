package server

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"tronbyt-server/internal/apps"
	"tronbyt-server/internal/data"

	"github.com/stretchr/testify/require"
)

// BenchmarkCatalogueForUser1000 approximates the production catalogue size
// while the system-app manifest cache is warm. It guards against accidentally
// reintroducing manifest parsing or superlinear catalogue summary work.
func BenchmarkCatalogueForUser1000(b *testing.B) {
	server := &Server{systemAppsCache: make([]apps.AppMetadata, 1_000)}
	for i := range server.systemAppsCache {
		id := fmt.Sprintf("app-%04d", i)
		server.systemAppsCache[i] = apps.AppMetadata{
			Manifest: apps.Manifest{ID: id, Name: "Catalogue " + id, Category: "utility", Tags: []string{"clock"}},
			Path:     "system-apps/apps/" + id,
			Preview:  id + ".webp",
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		items := server.catalogueForUser(nil)
		if len(items) != 1_000 {
			b.Fatalf("got %d catalogue items", len(items))
		}
	}
}

func TestPhysicalPollingRemainsResponsiveUnderCatalogueLoad(t *testing.T) {
	s := newTestServerAPI(t)
	s.systemAppsCache = make([]apps.AppMetadata, 1_000)
	for i := range s.systemAppsCache {
		id := fmt.Sprintf("load-app-%04d", i)
		s.systemAppsCache[i] = apps.AppMetadata{
			Manifest: apps.Manifest{ID: id, Name: "Load App " + id, Category: "utility", Tags: []string{"load"}},
			Path:     "system-apps/apps/" + id,
		}
		if i < 6 {
			dir := filepath.Join(s.DataDir, "system-apps", "apps", id)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			file, err := os.Create(filepath.Join(dir, id+".png"))
			require.NoError(t, err)
			pixel := image.NewRGBA(image.Rect(0, 0, 1, 1))
			pixel.Set(0, 0, color.RGBA{R: uint8(i * 30), G: 80, B: 160, A: 255})
			require.NoError(t, png.Encode(file, pixel))
			require.NoError(t, file.Close())
			s.systemAppsCache[i].Preview = filepath.Join(id, id+".png")
		}
	}

	path := "pushed:load-frame"
	app := data.App{DeviceID: "testdevice", Iname: "load-frame", Name: "Load Frame", Pushed: true, PushKind: persistentPushKind, Enabled: true, Path: &path}
	require.NoError(t, s.DB.Create(&app).Error)
	require.NoError(t, s.savePushedImage("testdevice", "load-frame", "", []byte("rehearsal-frame")))
	require.NoError(t, s.DB.Model(&data.Device{}).Where("id = ?", "testdevice").Update("require_api_key", true).Error)

	get := func(path, key string) (time.Duration, int) {
		started := time.Now()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		response := httptest.NewRecorder()
		s.ServeHTTP(response, req)
		return time.Since(started), response.Code
	}

	var workers sync.WaitGroup
	var loadErrors int
	var errorLock sync.Mutex
	recordError := func(status int) {
		if status >= 200 && status < 400 {
			return
		}
		errorLock.Lock()
		loadErrors++
		errorLock.Unlock()
	}
	for worker := range 6 {
		worker := worker
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 20 {
				_, status := get(fmt.Sprintf("/v0/catalogue/load-app-%04d/icon", worker), "test_api_key")
				recordError(status)
			}
		}()
	}
	for worker := range 4 {
		worker := worker
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range 20 {
				_, status := get(fmt.Sprintf("/v0/catalogue?limit=30&search=load-app-%d%d", worker, index%10), "test_api_key")
				recordError(status)
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		for range 20 {
			_, status := get("/v0/devices/testdevice/diagnostics", "test_api_key")
			recordError(status)
		}
	}()

	pollLatencies := make([]time.Duration, 0, 20)
	for range 20 {
		latency, status := get("/testdevice/next", "device_api_key")
		require.Equal(t, http.StatusOK, status)
		pollLatencies = append(pollLatencies, latency)
	}
	workers.Wait()
	require.Zero(t, loadErrors)
	sort.Slice(pollLatencies, func(i, j int) bool { return pollLatencies[i] < pollLatencies[j] })
	p95 := pollLatencies[int(float64(len(pollLatencies)-1)*0.95)]
	maximum := pollLatencies[len(pollLatencies)-1]
	t.Logf("1000-summary concurrent load: poll_requests=%d p95=%s max=%s icon_workers=6 search_workers=4", len(pollLatencies), p95, maximum)
	require.Less(t, p95, 2*time.Second)
	require.Less(t, maximum, 5*time.Second)
}
