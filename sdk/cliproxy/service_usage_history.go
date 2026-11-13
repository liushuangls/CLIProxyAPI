package cliproxy

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagehistory"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

func (s *Service) startUsageHistory(cfg *config.Config) error {
	if cfg.UsageHistory.Disabled {
		return nil
	}
	if err := cfg.UsageHistory.Validate(); err != nil {
		return err
	}
	path := strings.TrimSpace(cfg.UsageHistory.Path)
	if path == "" {
		path = filepath.Join(cfg.AuthDir, "usage-history.db")
	}
	store, err := usagehistory.Open(path, cfg.UsageHistory.RetentionDays)
	if err != nil {
		return fmt.Errorf("start credential usage history: %w", err)
	}
	s.usageHistory = store
	usage.RegisterNamedPlugin("credential-usage-history", store)
	s.serverOptions = append(s.serverOptions, api.WithUsageHistory(store))
	return nil
}

func (s *Service) stopUsageHistory(ctx context.Context) error {
	usage.StopDefault()
	if s.usageHistory == nil {
		return nil
	}
	store := s.usageHistory
	if err := usage.DefaultManager().Shutdown(ctx); err != nil {
		// Keep the file open until queued callbacks finish, even if the caller's
		// shutdown deadline expires. Never close it underneath pending writes.
		go func() {
			if errDrain := usage.DefaultManager().Shutdown(context.Background()); errDrain != nil {
				log.WithError(errDrain).Error("failed to drain usage history")
				return
			}
			if errClose := store.Close(); errClose != nil {
				log.WithError(errClose).Error("close usage history")
			}
		}()
		return fmt.Errorf("drain usage history: %w", err)
	}
	return store.Close()
}
