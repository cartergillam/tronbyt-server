package server

import (
	"testing"

	"tronbyt-server/internal/data"

	"github.com/stretchr/testify/assert"
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
