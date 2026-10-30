package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
)

// MediaBackfillSummary counts outcomes without exposing message identifiers.
type MediaBackfillSummary struct{ Stored, Skipped, Failed int }

// backfillMedia retries only attachment occurrences already present in the archive.
// The caller holds the source execution and supplies a freshly discovered catalog.
func backfillMedia(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, dialogs []CatalogDialog, options MediaOptions) (MediaBackfillSummary, error) {
	var summary MediaBackfillSummary
	if st == nil || sourceID <= 0 || client == nil || options.AttachmentsDir == "" || options.TempDir == "" || options.Download == nil {
		return summary, errors.New("invalid Telegram media backfill")
	}
	if err := options.Policy.Validate(); err != nil {
		return summary, err
	}
	byID := make(map[int64]CatalogDialog, len(dialogs))
	for _, dialog := range dialogs {
		byID[dialog.ChatID] = dialog
	}
	process := newMediaProcessor(options.Policy, dialogs, options.AttachmentsDir, options.TempDir, options.Download)
	var after int64
	for {
		page, err := st.ListTelegramPendingMediaAfterContext(ctx, sourceID, after, 100)
		if err != nil {
			return summary, err
		}
		if len(page) == 0 {
			return summary, ctx.Err()
		}
		for _, item := range page {
			after = item.MessageID
			parts := strings.Split(item.SourceMessageID, ":")
			if len(parts) != 2 || parts[0] != item.ChatID {
				summary.Failed++
				continue
			}
			chatID, chatErr := strconv.ParseInt(parts[0], 10, 64)
			messageID, msgErr := strconv.ParseInt(parts[1], 10, 32)
			dialog, ok := byID[chatID]
			if chatErr != nil || msgErr != nil || messageID <= 0 || !ok {
				summary.Failed++
				continue
			}
			kind := "private_group"
			switch dialog.Kind {
			case "user":
				kind = "personal_chat"
			case "channel":
				kind = "public_channel"
			case "supergroup":
				kind = "private_supergroup"
			}
			refs, err := st.MessageProviderAttachmentsContext(ctx, item.MessageID, "api:")
			if err != nil {
				return summary, err
			}
			for _, old := range refs {
				if old.StoragePath != "" {
					continue
				}
				source := strings.TrimPrefix(old.SourceAttachmentID, "api:")
				saved := &telegram.LiveMedia{SourceID: source, Metadata: old.Metadata, Kind: old.MediaType, Filename: old.Filename, MIMEType: old.MimeType}
				if _, err := mediaReferenceFromStored(saved); err != nil {
					summary.Failed++
					continue
				}
				ref, err := RefreshMediaReference(ctx, client, dialog, int(messageID), source)
				if err != nil {
					if ctx.Err() != nil {
						return summary, ctx.Err()
					}
					if errors.Is(err, ErrFloodWaitLimit) {
						return summary, err
					}
					summary.Failed++
					slog.Warn("Telegram attachment reference could not be refreshed; it remains retryable")
					continue
				}
				media, err := ref.liveMedia()
				if err != nil {
					return summary, err
				}
				if err := process(ctx, telegram.Chat{ID: chatID, Type: kind}, media); err != nil {
					return summary, err
				}
				state, reason := attachmentpolicy.StatePending, attachmentpolicy.SkipReason("")
				switch {
				case media.StoragePath != "":
					state = attachmentpolicy.StateStored
					summary.Stored++
				case media.SkipReason != "":
					state, reason = attachmentpolicy.StateSkipped, media.SkipReason
					summary.Skipped++
				case media.Failed:
					state, reason = attachmentpolicy.StateFailed, attachmentpolicy.SkipFetchFailure
					summary.Failed++
				}
				write := store.AttachmentWrite{Filename: old.Filename, MIMEType: old.MimeType, StoragePath: media.StoragePath, ContentHash: media.ContentHash, Size: media.Size, SourceAttachmentID: old.SourceAttachmentID, MediaType: old.MediaType, Width: old.Width, Height: old.Height, DurationMS: old.DurationMS, Metadata: old.Metadata, Role: old.Role, RoleSource: old.RoleSource, SourcePartKey: old.SourcePartKey, ContentID: old.ContentID, State: state, SkipReason: reason}
				if err := st.EnsureAttachmentRecordAppendOnly(ctx, item.MessageID, write); err != nil {
					return summary, fmt.Errorf("fill Telegram attachment: %w", err)
				}
			}
			if err := st.RecomputeMessageAttachmentStatsContext(ctx, item.MessageID); err != nil {
				return summary, err
			}
		}
	}
}

// RunAuthenticatedMediaBackfill verifies the account and refreshes its catalog
// before retrying existing attachment occurrences. The caller owns the source run.
func RunAuthenticatedMediaBackfill(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, options MediaOptions) (MediaBackfillSummary, error) {
	var summary MediaBackfillSummary
	if st == nil || client == nil || sourceID <= 0 {
		return summary, errors.New("invalid authenticated Telegram media backfill")
	}
	owner, err := Owner(ctx, client)
	if err != nil {
		return summary, err
	}
	source, err := st.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return summary, err
	}
	if source == nil || source.SourceType != telegram.SourceType || source.Identifier != telegram.OwnerSourceIdentifier(owner.ID) {
		return summary, errors.New("Telegram account does not match archive source")
	}
	dialogs, err := ListDialogs(ctx, client)
	if err != nil {
		return summary, err
	}
	formerGroups, err := repairAndDiscoverFormerGroups(ctx, st, sourceID, client, dialogs, 0)
	if err != nil {
		return summary, err
	}
	dialogs = attachFormerGroups(dialogs, formerGroups, nil)
	return backfillMedia(ctx, st, sourceID, client, dialogs, options)
}
