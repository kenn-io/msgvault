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
	existing, _ := s.GetSourceByTypeAndIdentifier(ctx, "whatsapp", src.Phone)
	previousRunID := latestSyncRunID(ctx, s, existing)
	summary, err := whatsapp.NewImporter(s, scheduledWhatsAppProgress{logger: state.logger, source: src.Name}).Import(ctx, path, opts)
	if err != nil {
		err = fmt.Errorf("whatsapp apple source %q: %w", src.Name, err)
		recordNotStartedAttempt(ctx, s, existing, previousRunID, err, state.logger)
		return err
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
	var existing *store.Source
	if sources, err := s.ListSourcesContext(ctx, sourceTypeAppleMessages); err == nil {
		existing = store.SelectImessageSource(sources)
	}
	client, err := imessage.NewClient(ctx, path, opts...)
	if err != nil {
		err = fmt.Errorf("open iMessage database: %w", err)
		recordNotStartedAttempt(ctx, s, existing, latestSyncRunID(ctx, s, existing), err, state.logger)
		return err
	}
	defer func() { _ = client.Close() }()
	src, err := resolveImessageSource(s)
	if err != nil {
		return fmt.Errorf("get or create source: %w", err)
	}
	if err := runPostSourceCreateMigrationsForInvocation(ctx, s, state); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}
	summary, err := importImessageRecorded(ctx, s, client, src.ID)
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

// latestSyncRunID returns the newest run recorded for src, or 0 when src is
// nil or has none.
func latestSyncRunID(ctx context.Context, s *store.Store, src *store.Source) int64 {
	if src == nil {
		return 0
	}
	run, err := s.GetLatestSyncContext(ctx, src.ID, 0)
	if err != nil || run == nil {
		return 0
	}
	return run.ID
}

// recordNotStartedAttempt gives an archived source a failed run when its
// scheduled import failed before recording one, for example because macOS
// denied access to the database. Without it, Sources keeps showing the
// previous run's result. A source that was never imported is left alone.
func recordNotStartedAttempt(
	ctx context.Context, s *store.Store, src *store.Source, previousRunID int64, cause error, logger *slog.Logger,
) {
	// A stop request is not a source failure.
	if src == nil || ctx.Err() != nil || latestSyncRunID(ctx, s, src) != previousRunID {
		return
	}
	if err := s.RecordNotStartedSync(ctx, src.ID, cause.Error()); err != nil {
		logger.Warn("could not record failed scheduled import", "source_id", src.ID, "error", err)
	}
}
