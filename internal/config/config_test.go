package config

import (
	"testing"
)

func TestLoadSettings(t *testing.T) {
	t.Setenv("DATA_DIR", "testdata")
	t.Setenv("SYSTEM_APPS_REPO", "https://github.com/cartergillam/apps.git")
	t.Setenv("SYSTEM_APPS_REF", "feature/mlb-clock-reliability")
	t.Setenv("SYSTEM_APPS_EXPECTED_COMMIT", "bbfcff4e0")

	cfg, err := LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings failed: %v", err)
	}

	if cfg.DataDir != "testdata" {
		t.Errorf("Expected DATA_DIR 'testdata', got '%s'", cfg.DataDir)
	}
	if !cfg.SystemAppsRepoExplicit() {
		t.Fatal("expected deployment repository to take precedence over database settings")
	}
	if cfg.SystemAppsRef != "feature/mlb-clock-reliability" || cfg.SystemAppsExpectedSHA != "bbfcff4e0" {
		t.Fatalf("unexpected apps source: ref=%q expected=%q", cfg.SystemAppsRef, cfg.SystemAppsExpectedSHA)
	}
}
