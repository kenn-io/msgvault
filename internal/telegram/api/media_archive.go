package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/telegram"
)

type mediaDownloadFunc func(context.Context, *MediaReference, string, int64) (*os.File, error)

func newMediaProcessor(policy attachmentpolicy.Policy, dialogs []CatalogDialog, attachmentsDir, tempDir string, download mediaDownloadFunc) func(context.Context, telegram.Chat, *telegram.LiveMedia) error {
	byID := make(map[int64]CatalogDialog, len(dialogs))
	for _, dialog := range dialogs {
		byID[dialog.ChatID] = dialog
	}
	return func(ctx context.Context, chat telegram.Chat, media *telegram.LiveMedia) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		dialog, ok := byID[chat.ID]
		if !ok {
			return errors.New("Telegram media has no catalog dialog")
		}
		ref, err := mediaReferenceFromStored(media)
		if err != nil {
			return err
		}
		conversation := attachmentpolicy.Conversation{Type: chat.ConvType(), ParticipantCount: dialog.ParticipantCount}
		if policy.MaxParticipants > 0 && !dialog.ParticipantCountKnown && policy.Evaluate(conversation, ref.Size) == "" {
			media.Failed = true
			slog.Warn("Telegram attachment download deferred because the conversation membership count is unavailable")
			return nil
		}
		processed, err := archiveMedia(ctx, ref, policy, conversation, attachmentsDir, tempDir, download)
		if err != nil {
			return err
		}
		*media = *processed
		return nil
	}
}

// archiveMedia prepares a durable reference and, when policy allows, stores its
// bytes. A temporary download failure remains retryable with the reference.
func archiveMedia(ctx context.Context, ref *MediaReference, policy attachmentpolicy.Policy, conversation attachmentpolicy.Conversation, attachmentsDir, tempDir string, download mediaDownloadFunc) (*telegram.LiveMedia, error) {
	if ref == nil || download == nil || attachmentsDir == "" || tempDir == "" {
		return nil, errors.New("invalid Telegram media archive options")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if policy.MaxBytes <= 0 {
		policy.MaxBytes = attachmentpolicy.DefaultChatMaxBytes
	}
	media, err := ref.liveMedia()
	if err != nil {
		return nil, err
	}
	if reason := policy.Evaluate(conversation, ref.Size); reason != "" {
		media.SkipReason = reason
		return media, nil
	}
	limit := policy.MaxBytes
	file, err := download(ctx, ref, tempDir, limit)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrMediaTooLarge) {
			media.SkipReason = attachmentpolicy.SkipSizeCap
			return media, nil
		}
		media.Failed = true
		slog.Warn("Telegram attachment download failed; it can be retried")
		return media, nil
	}
	if file == nil {
		return nil, errors.New("Telegram media download returned no file")
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	media.StoragePath, media.ContentHash, media.Size, err = export.StoreAttachmentStream(ctx, attachmentsDir, file, limit)
	if err != nil {
		return nil, fmt.Errorf("store Telegram media: %w", err)
	}
	return media, nil
}
