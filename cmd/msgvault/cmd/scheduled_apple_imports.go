package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/imessage"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/whatsapp"
)

const defaultWhatsAppAppleStorePath = "Library/Group Containers/group.net.whatsapp.WhatsApp.shared/ChatStorage.sqlite"

func runScheduledWhatsAppApple(ctx context.Context, s *store.Store, src config.WhatsAppAppleSource) (retErr error) {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
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
	opts := whatsapp.DefaultOptions()
	opts.Phone = src.Phone
	opts.DisplayName = src.DisplayName
	summary, err := whatsapp.NewImporter(s, scheduledWhatsAppProgress{logger: state.logger, source: src.Name}).Import(ctx, path, opts)
	if err != nil {
		return fmt.Errorf("whatsapp apple source %q: %w", src.Name, err)
	}
	if err := s.TouchSourceLastSyncAt(summary.SourceID); err != nil {
		return fmt.Errorf("update source freshness: %w", err)
	}
	confirmDefaultIdentity(io.Discard, s, summary.SourceID, src.Phone, src.Phone, "phone-e164", state.logger)
	if err := runPostSourceCreateMigrationsForInvocation(s, state); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}
	state.logger.Info("whatsapp apple import finished", "source", src.Name, "added", summary.MessagesAdded, "updated", summary.MessagesUpdated, "skipped", summary.MessagesSkipped)
	return nil
}

func runScheduledIMessage(ctx context.Context, s *store.Store, cfg config.IMessageConfig) (retErr error) {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	defer func() {
		retErr = errors.Join(retErr, rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), api.IMessageJobName))
	}()
	src, err := resolveImessageSource(s)
	if err != nil {
		return fmt.Errorf("get or create source: %w", err)
	}
	if err := runPostSourceCreateMigrationsForInvocation(s, state); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}
	opts := []imessage.ClientOption{
		imessage.WithImessageLogger(state.logger),
		imessage.WithAttachments(state.cfg.AttachmentsDir(), "", imessageMediaPolicy(0, state.cfg)),
	}
	last, err := s.GetLastSuccessfulSyncByType(src.ID, "imessage_scheduled_import")
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return fmt.Errorf("get previous scheduled import: %w", err)
	}
	if last != nil {
		opts = append(opts, imessage.WithAfterDate(last.StartedAt.Add(-cfg.Window)))
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
	client, err := imessage.NewClient(path, opts...)
	if err != nil {
		return fmt.Errorf("open iMessage database: %w", err)
	}
	defer func() { _ = client.Close() }()
	summary, err := importImessageRecorded(ctx, s, client, src.ID, "imessage_scheduled_import")
	if err != nil {
		return fmt.Errorf("imessage import: %w", err)
	}
	state.logger.Info("imessage import finished", "imported", summary.MessagesImported, "added", summary.MessagesAdded, "dates_cleared", summary.DatesCleared)
	if count, err := s.RetitleImessageChats(); err != nil {
		state.logger.Warn("could not refresh iMessage chat titles", "error", err)
	} else {
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
