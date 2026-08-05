package serve

import (
	"fmt"
	"log/slog"
	"os"
	"time"
	"tronbyt-server/internal/config"
	"tronbyt-server/internal/data"
	"tronbyt-server/internal/gitutils"
	"tronbyt-server/internal/server"

	"github.com/spf13/cobra"
	"gorm.io/gorm"
)

const Name = "serve"

func New() *cobra.Command {
	return &cobra.Command{
		Use:   Name,
		Short: "Run Tronbyt server",
		RunE:  run,

		SilenceUsage: true,
	}
}

func run(cmd *cobra.Command, args []string) error {
	cfg, err := config.FromContext(cmd.Context())
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	if err := migrateLegacyDB(cmd.Context(), cfg.DBDSN, cfg.DataDir); err != nil {
		return fmt.Errorf("failed to migrate legacy database: %w", err)
	}

	// Open DB
	db, err := data.Open(cfg.DBDSN, cfg.LogLevel)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	// AutoMigrate before reading deployment settings so a fresh database has
	// the settings table available.
	if err := db.AutoMigrate(&data.User{}, &data.Device{}, &data.App{}, &data.WebAuthnCredential{}, &data.Setting{}, &data.OIDCIdentity{}); err != nil {
		return fmt.Errorf("failed to migrate schema: %w", err)
	}
	if !cfg.SystemAppsRepoExplicit() {
		setting, err := gorm.G[data.Setting](db).Where("key = ?", "system_apps_repo").First(cmd.Context())
		if err == nil && setting.Value != "" {
			cfg.SystemAppsRepo = setting.Value
		}
	}

	// Clone/update and verify the exact system-app source before the server can
	// render. Production must never continue with an unknown or stale checkout.
	info, err := gitutils.EnsureRepoAtRef(
		cfg.SystemAppsDir(), cfg.SystemAppsRepo, cfg.SystemAppsRef,
		cfg.SystemAppsExpectedSHA, cfg.GitHubToken, cfg.Production,
	)
	if err != nil {
		slog.Error("System apps checkout failed", "repository", cfg.SystemAppsRepo, "ref", cfg.SystemAppsRef, "update_succeeded", false, "error", err)
		if cfg.Production {
			return fmt.Errorf("system apps checkout failed: %w", err)
		}
	} else {
		slog.Info("System apps checkout ready", "repository", info.URL, "ref", info.Branch, "commit", info.CommitHash, "update_succeeded", true)
		invalidated, invalidateErr := gorm.G[data.App](db).
			Where("path LIKE ?", "%system-apps/%").
			Select("LastRender", "NextRenderAt").
			Updates(cmd.Context(), data.App{LastRender: time.Time{}, NextRenderAt: nil})
		if invalidateErr != nil {
			return fmt.Errorf("invalidate system-app renders after checkout verification: %w", invalidateErr)
		}
		slog.Info("System app renders invalidated", "installations", invalidated, "commit", info.CommitHash)
	}

	// Sanitize data
	sanitizeDB(cmd.Context(), db)

	cache, err := initCache(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("failed to initialize cache: %w", err)
	}
	defer cache.Close()

	srv := server.NewServer(db, cfg)
	srv.SchemaCache = cache

	// Firmware Update (production only)
	if cfg.Production {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("Panic during background firmware update", "panic", r)
				}
			}()
			if err := srv.UpdateFirmwareBinaries(); err != nil {
				slog.Error("Failed to update firmware binaries in background", "error", err)
			}
		}()
	} else {
		slog.Info("Skipping firmware update (dev mode)")
	}

	singleUserWarning(cmd.Context(), db, cfg.SingleUserAutoLogin)

	return serve(cfg, srv)
}
