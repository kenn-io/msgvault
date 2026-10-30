package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	const syncType = "whatsapp_apple_import"
	readStartedAt := time.Now()
	if err := checkAppleSourceReadable(path); err != nil {
		recordUnreadableSource(ctx, s, "whatsapp", src.Phone, syncType, path, readStartedAt, err)
		return fmt.Errorf("whatsapp_apple source %q: %w", src.Name, err)
	}
	writerAlive := whatsAppWriterAlive()

	opts := whatsapp.DefaultOptions()
	opts.Phone = src.Phone
	opts.DisplayName = src.DisplayName
	opts.AttachmentsDir = state.cfg.AttachmentsDir()
	summary, err := whatsapp.NewImporter(s, scheduledWhatsAppProgress{logger: state.logger, source: src.Name}).Import(ctx, path, opts)
	if err != nil {
		if summary != nil && summary.SyncID != 0 {
			markRunIfSourceUnreadable(ctx, s, summary.SyncID, path, readStartedAt)
		} else {
			// The importer failed before starting its run.
			recordUnreadableSource(ctx, s, "whatsapp", src.Phone, syncType, path, readStartedAt, err)
		}
		return fmt.Errorf("whatsapp_apple source %q: %w", src.Name, err)
	}
	if summary.SourceID != 0 {
		m := store.SyncMeasurement{
			ReadStartedAt: &readStartedAt,
			SourceMtime:   appleSourceMtime(path),
			WriterAlive:   writerAlive,
		}.ClassifyOutcome(summary.MessagesAdded+summary.MessagesUpdated > 0)
		if err := s.SetSyncMeasurement(context.WithoutCancel(ctx), summary.SyncID, m); err != nil {
			state.logger.Warn("could not record sync measurement", "error", err)
		}
		// A run that could not see new data must not make the source look fresh.
		if m.Outcome != store.SyncOutcomeUnmeasured {
			if err := s.TouchSourceLastSyncAt(summary.SourceID); err != nil {
				return fmt.Errorf("update source freshness: %w", err)
			}
		}
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
	// The configured source is created even when chat.db cannot be read, so
	// status reports the gap.
	src, err := resolveImessageSource(s)
	if err != nil {
		return fmt.Errorf("get or create source: %w", err)
	}
	client, err := openImessageClientRecorded(ctx, s, src, path, opts, time.Now())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	summary, err := importImessageRecorded(ctx, s, client, src.ID, path)
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

// openImessageClientRecorded opens chat.db. When src is set and the source
// cannot be read (missing, or denied by Full Disk Access), it also records an
// unmeasured run on src, so every caller reports the gap the same way.
func openImessageClientRecorded(
	ctx context.Context, s *store.Store, src *store.Source, path string,
	opts []imessage.ClientOption, readStartedAt time.Time,
) (*imessage.Client, error) {
	err := checkAppleSourceReadable(path)
	var client *imessage.Client
	if err == nil {
		client, err = imessage.NewClient(path, opts...)
	}
	if err != nil {
		if src != nil {
			recordUnreadableSource(ctx, s, src.SourceType, src.Identifier, "imessage_import", path, readStartedAt, err)
		}
		return nil, fmt.Errorf("open iMessage database: %w", err)
	}
	return client, nil
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

// scheduledWhatsAppProgress keeps importer warnings visible in daemon logs.
type scheduledWhatsAppProgress struct {
	whatsapp.NullProgress

	logger *slog.Logger
	source string
}

func (p scheduledWhatsAppProgress) OnError(err error) {
	p.logger.Warn("whatsapp apple import warning", "source", p.source, "error", err)
}

// whatsAppWriterAlive reports whether WhatsApp for Mac is running on this
// machine. While it is closed, ChatStorage stays readable but nothing new
// reaches it, so a run that changes nothing proves nothing. It returns nil when
// that cannot be known: off macOS, or when pgrep itself fails. Tests replace it.
var whatsAppWriterAlive = func() *bool {
	if runtime.GOOS != "darwin" {
		return nil
	}
	alive := true
	if err := exec.Command("pgrep", "-qx", "WhatsApp").Run(); err != nil {
		// pgrep exits 1 when no process matches; anything else is a failed probe.
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return nil
		}
		alive = false
	}
	return &alive
}

// checkAppleSourceReadable opens path read-only to separate a missing source
// from a denied one. Tests replace it to inject a denial without relying on
// file modes, which not every OS enforces.
var checkAppleSourceReadable = func(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return f.Close()
}

// sourceUnreadableReason probes path and names why it cannot be read, or
// returns "" when it can. Failures are classified by probing the source rather
// than by their text, so an archive-side error such as a PostgreSQL
// "permission denied" is never reported as a source the daemon cannot read.
func sourceUnreadableReason(path string) string {
	err := checkAppleSourceReadable(path)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, fs.ErrNotExist):
		return store.SyncReasonSourceMissing
	case errors.Is(err, fs.ErrPermission):
		return store.SyncReasonFDADenied
	default:
		return ""
	}
}

// recordUnreadableSource records an unmeasured run after cause when the source
// at path cannot be read and no run was started, so status shows the gap
// rather than the previous run's result.
func recordUnreadableSource(
	ctx context.Context, s *store.Store, sourceType, identifier, syncType, path string,
	readStartedAt time.Time, cause error,
) {
	reason := sourceUnreadableReason(path)
	if reason == "" {
		return
	}
	src, err := s.GetOrCreateSource(sourceType, identifier)
	if err != nil {
		return
	}
	_ = s.RecordUnmeasuredSync(context.WithoutCancel(ctx), src.ID, syncType, reason, cause.Error(),
		store.SyncMeasurement{ReadStartedAt: &readStartedAt})
}

// markRunIfSourceUnreadable marks a failed run unmeasured when its source
// became unreadable, and returns the reason ("" when the source is readable).
func markRunIfSourceUnreadable(
	ctx context.Context, s *store.Store, syncID int64, path string, readStartedAt time.Time,
) string {
	reason := sourceUnreadableReason(path)
	if reason == "" {
		return ""
	}
	_ = s.SetSyncMeasurement(context.WithoutCancel(ctx), syncID, store.SyncMeasurement{
		Outcome: store.SyncOutcomeUnmeasured, Reason: reason, ReadStartedAt: &readStartedAt,
	})
	return reason
}

// appleSourceMtime returns the newest mtime of the SQLite file and its WAL:
// writers append to the WAL long before the main file changes.
func appleSourceMtime(path string) *time.Time {
	var newest time.Time
	for _, p := range []string{path, path + "-wal"} {
		if info, err := os.Stat(p); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if newest.IsZero() {
		return nil
	}
	return &newest
}
