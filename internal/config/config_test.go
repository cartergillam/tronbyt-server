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

func TestYahooFinanceExperimentRequiresExplicitLocalFlags(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  Settings
		want bool
	}{
		{"defaults stay off", Settings{Production: true}, false},
		{"production cannot enable it", Settings{Production: true, LocalDevelopment: true, ExperimentalYahooFinance: true}, false},
		{"missing local marker", Settings{Production: false, ExperimentalYahooFinance: true}, false},
		{"missing explicit opt in", Settings{Production: false, LocalDevelopment: true}, false},
		{"explicit local opt in", Settings{Production: false, LocalDevelopment: true, ExperimentalYahooFinance: true}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.cfg.YahooFinanceExperimentEnabled(); got != test.want {
				t.Fatalf("YahooFinanceExperimentEnabled() = %t, want %t", got, test.want)
			}
		})
	}
}
