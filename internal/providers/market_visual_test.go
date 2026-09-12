package providers

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"image"
	"os"
	"path/filepath"
	"testing"
)

func TestRealLogoShapes(t *testing.T) {
	files, err := filepath.Glob("testdata/logos/*")
	require.NoError(t, err)
	require.Len(t, files, 7)
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			body, err := os.ReadFile(file)
			require.NoError(t, err)
			normalized, err := normalizeSportsLogo(body, "")
			require.NoError(t, err)
			logo, _, err := image.Decode(bytes.NewReader(normalized))
			require.NoError(t, err)
			require.Equal(t, image.Rect(0, 0, 20, 20), logo.Bounds())
			useful := usefulLogoBounds(logo)
			require.GreaterOrEqual(t, max(useful.Dx(), useful.Dy()), 17)
			if output := os.Getenv("TRONBYT_VISUAL_OUTPUT_DIR"); output != "" {
				require.NoError(t, os.WriteFile(filepath.Join(output, filepath.Base(file)+".png"), normalized, 0600))
			}
		})
	}
}
