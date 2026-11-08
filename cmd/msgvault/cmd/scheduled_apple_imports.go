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
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/imessage"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/whatsapp"
)

const defaultWhatsAppAppleStorePath = "Library/Group Containers/group.net.whatsapp.WhatsApp.shared/ChatStorage.sqlite"

func runScheduledWhatsAppApple(ctx context.Context, s *store.Store, src config.WhatsAppAppleSource) (retErr error) {
	state := invocationFromContext(ctx)
	defer func() {
		retErr = errors.Join(retErr, rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), api.WhatsAppAppleJobName(src.Phone)))
	}()
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
	writerAlive := whatsAppWriterAlive(ctx)

	opts := whatsapp.DefaultOptions()
	opts.WriterAlive = writerAlive
	opts.Phone = src.Phone
	opts.DisplayName = src.DisplayName
	summary, err := whatsapp.NewImporter(s, scheduledWhatsAppProgress{logger: state.logger, source: src.Name}).Import(ctx, path, opts)
	if err != nil {
		if summary != nil && summary.SyncID != 0 {
			markRunIfSourceUnreadable(ctx, s, summary.SyncID, path, readStartedAt)
		} else {
			recordUnreadableSource(ctx, s, "whatsapp", src.Phone, syncType, path, readStartedAt, err)
		}
		return fmt.Errorf("whatsapp apple source %q: %w", src.Name, err)
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
	}
	// The import committed; record its owner and finish the migration even
	// when a stop request arrives now.
	post := context.WithoutCancel(ctx)
	confirmDefaultIdentity(post, io.Discard, s, summary.SourceID, src.Phone, src.Phone, "phone-e164", state.logger)
	if err := runPostSourceCreateMigrationsForInvocation(post, s, state); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}
	state.logger.Info("whatsapp apple import finished", "source", src.Name, "added", summary.MessagesAdded, "updated", summary.MessagesUpdated, "skipped", summary.MessagesSkipped)
	return nil
}

func runScheduledIMessage(ctx context.Context, s *store.Store, cfg config.IMessageConfig) (retErr error) {
	state := invocationFromContext(ctx)
	defer func() {
		retErr = errors.Join(retErr, rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), api.IMessageJobName))
	}()
	opts, err := buildImessageOpts(state.logger, state.cfg.AttachmentsDir(), "", false, imessageMediaPolicy(0, state.cfg))
	if err != nil {
		return err
	}
	if cfg.Me != "" {
		opts = append(opts, imessage.WithOwnerHandle(cfg.Me))
	}
	path := cfg.DBPath
	if path == "" {
		path, err = defaultChatDBPath()
		if err != nil {
			return err
		}
	}
	src, err := resolveImessageSource(s)
	if err != nil {
		return fmt.Errorf("get or create source: %w", err)
	}
	client, err := openImessageClientRecorded(ctx, s, src, path, opts, time.Now())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if err := runPostSourceCreateMigrationsForInvocation(ctx, s, state); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}
	summary, err := importImessageRecorded(ctx, s, client, src.ID, path)
	if attachmentImportOnly(err) {
		state.logger.Warn("imessage attachment import warning", "error", err)
	} else if err != nil {
		return fmt.Errorf("imessage import: %w", err)
	}
	state.logger.Info("imessage import finished", "imported", summary.MessagesImported, "added", summary.MessagesAdded, "updated", summary.MessagesUpdated)
	// The import committed; refresh titles even when a stop request arrives now.
	if count, err := s.RetitleImessageChats(context.WithoutCancel(ctx)); err != nil {
		state.logger.Warn("could not refresh iMessage chat titles", "error", err)
	} else if count > 0 {
		state.logger.Info("imessage chat titles refreshed", "count", count)
	}
	return nil
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
		client, err = imessage.NewClient(ctx, path, opts...)
	}
	if err != nil {
		if src != nil {
			recordUnreadableSource(ctx, s, src.SourceType, src.Identifier, "imessage_import", path, readStartedAt, err)
		}
		return nil, fmt.Errorf("open iMessage database: %w", err)
	}
	return client, nil
}

// whatsAppWriterAlive reports whether WhatsApp for Mac is running on this
// machine. While it is closed, ChatStorage stays readable but nothing new
// reaches it, so a run that changes nothing proves nothing. It returns nil when
// that cannot be known: off macOS, or when pgrep itself fails. Tests replace it.
var whatsAppWriterAlive = func(ctx context.Context) *bool {
	if runtime.GOOS != "darwin" {
		return nil
	}
	alive := true
	if err := exec.CommandContext(ctx, "pgrep", "-qx", "WhatsApp").Run(); err != nil {
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
