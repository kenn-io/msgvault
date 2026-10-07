package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/imessage"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/whatsapp"
)

// defaultWhatsAppAppleStorePath is where WhatsApp for Mac keeps ChatStorage.
const defaultWhatsAppAppleStorePath = "Library/Group Containers/group.net.whatsapp.WhatsApp.shared/ChatStorage.sqlite"

// runScheduledWhatsAppApple imports one configured native macOS WhatsApp
// store. The run belongs to the daemon scheduler: ctx is the scheduler's, not
// an HTTP or CLI client's.
func runScheduledWhatsAppApple(ctx context.Context, s *store.Store, src config.WhatsAppAppleSource) error {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	if !strings.HasPrefix(src.Phone, "+") {
		return fmt.Errorf("whatsapp_apple source %q: phone must be in E.164 format (starting with +)", src.Name)
	}
	path := src.Path
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("get home directory: %w", err)
		}
		path = filepath.Join(home, defaultWhatsAppAppleStorePath)
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("whatsapp_apple source %q: %w", src.Name, err)
	}

	opts := whatsapp.DefaultOptions()
	opts.Phone = src.Phone
	opts.DisplayName = src.DisplayName
	opts.AttachmentsDir = state.cfg.AttachmentsDir()
	summary, err := whatsapp.NewImporter(s, whatsapp.NullProgress{}).Import(ctx, path, opts)
	if err != nil {
		return fmt.Errorf("whatsapp_apple source %q: %w", src.Name, err)
	}
	if summary.SourceID != 0 {
		confirmDefaultIdentity(io.Discard, s, summary.SourceID, src.Phone, src.Phone, "phone-e164", state.logger)
	}
	state.logger.Info("whatsapp apple import finished", "source", src.Name,
		"added", summary.MessagesAdded, "skipped", summary.MessagesSkipped)
	return rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), "whatsapp-apple:"+src.Name)
}

// runScheduledIMessage imports the local macOS Messages store, limited to
// cfg.Window when set.
func runScheduledIMessage(ctx context.Context, s *store.Store, cfg config.IMessageConfig) error {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	path := cfg.DBPath
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("get home directory: %w", err)
		}
		path = filepath.Join(home, "Library", "Messages", "chat.db")
	}

	opts := []imessage.ClientOption{imessage.WithImessageLogger(state.logger)}
	if cfg.Window != "" {
		window, err := time.ParseDuration(cfg.Window)
		if err != nil || window <= 0 {
			return fmt.Errorf("imessage.window %q must be a positive duration such as \"48h\"", cfg.Window)
		}
		opts = append(opts, imessage.WithAfterDate(time.Now().Add(-window)))
	}
	if cfg.Me != "" {
		opts = append(opts, imessage.WithOwnerHandle(cfg.Me))
	}
	client, err := imessage.NewClient(path, opts...)
	if err != nil {
		return fmt.Errorf("open iMessage database: %w", err)
	}
	defer func() { _ = client.Close() }()

	src, err := resolveImessageSource(s)
	if err != nil {
		return fmt.Errorf("get or create source: %w", err)
	}
	summary, err := importImessageRecorded(ctx, s, client, src.ID)
	// A failed or cancelled import may already have cleared dates, so the
	// invalidation depends on the partial summary, not on success.
	if summary != nil && summary.DatesCleared > 0 {
		// A date clear leaves message IDs unchanged, so the message-id-keyed
		// staleness check cannot see it and cached messages.parquet rows would
		// keep the old dates. The CLI import forces a full rebuild here; the
		// daemon instead drops the cache commit marker, which makes the next
		// staleness probe demand a full rebuild and keeps the interval throttle
		// from deferring it. The marker is dropped even when AutoBuildCache is
		// off, so readers stop serving the stale cache.
		if invErr := invalidateCacheForDateClear(state.cfg); invErr != nil {
			invErr = fmt.Errorf("invalidate analytics cache after clearing dates: %w", invErr)
			if err != nil {
				return errors.Join(fmt.Errorf("imessage import: %w", err), invErr)
			}
			return invErr
		}
	}
	if err != nil {
		return fmt.Errorf("imessage import: %w", err)
	}
	state.logger.Info("imessage import finished", "imported", summary.MessagesImported,
		"dates_cleared", summary.DatesCleared)
	if _, err := s.RetitleImessageChats(); err != nil {
		state.logger.Warn("could not refresh iMessage chat titles", "error", err)
	}
	return rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), "imessage")
}

// invalidateCacheForDateClear makes the committed analytics cache unusable
// under the builder lock, so a build that read the pre-clear dates cannot
// republish its marker after the invalidation.
func invalidateCacheForDateClear(cfg *config.Config) error {
	if store.IsPostgresURL(cfg.DatabaseDSN()) {
		return nil
	}
	analyticsDir := cfg.AnalyticsDir()
	buildLock, err := acquireCacheBuildLock(context.Background(), analyticsDir)
	if err != nil {
		return err
	}
	invalidateErr := invalidateSyncStateFile(query.CacheStatePath(analyticsDir))
	return errors.Join(invalidateErr, wrapError(buildLock.Unlock(), "unlock cache builder lock"))
}
