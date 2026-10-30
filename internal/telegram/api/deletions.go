package api

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/gotd/td/tg"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
)

// ApplyCommonDeletions applies explicit common-box tombstones. The caller
// processes the remaining effects before advancing the difference checkpoint.
func ApplyCommonDeletions(ctx context.Context, st *store.Store, sourceID int64, updates []tg.UpdateClass) error {
	if st == nil || sourceID <= 0 {
		return errors.New("invalid Telegram common deletion target")
	}
	for _, update := range updates {
		deleted, ok := update.(*tg.UpdateDeleteMessages)
		if !ok {
			continue
		}
		if deleted == nil {
			return errors.New("invalid Telegram common deletion")
		}
		for _, id := range deleted.Messages {
			if err := st.MarkTelegramCommonDeletedContext(ctx, sourceID, id); err != nil {
				return fmt.Errorf("mark Telegram common deletion: %w", err)
			}
		}
	}
	return nil
}

// ApplyCommonDeletionsForDialog applies a common-box tombstone only to the
// selected user or basic-group key. A chat-filtered sync leaves the shared
// common cursor untouched, so other dialogs will receive the deletion later.
func ApplyCommonDeletionsForDialog(ctx context.Context, st *store.Store, sourceID int64, dialog CatalogDialog, updates []tg.UpdateClass) error {
	if st == nil || sourceID <= 0 || dialog.ChatID == 0 ||
		(dialog.Kind != "user" && dialog.Kind != "group") ||
		(dialog.Kind == "user" && dialog.ChatID < 0) || (dialog.Kind == "group" && dialog.ChatID > 0) {
		return errors.New("invalid Telegram common deletion dialog")
	}
	for _, update := range updates {
		deleted, ok := update.(*tg.UpdateDeleteMessages)
		if !ok {
			continue
		}
		if deleted == nil {
			return errors.New("invalid Telegram common deletion")
		}
		for _, id := range deleted.Messages {
			if id <= 0 || int64(id) > math.MaxInt32 {
				return errors.New("invalid Telegram common message ID")
			}
			if err := st.MarkMessageDeletedContext(ctx, sourceID, fmt.Sprintf("%d:%d", dialog.ChatID, id)); err != nil {
				return fmt.Errorf("mark Telegram common deletion: %w", err)
			}
		}
	}
	return nil
}

// ApplyChannelDeletions applies only tombstones belonging to the requested
// channel. A different peer must never share that channel's checkpoint.
func ApplyChannelDeletions(ctx context.Context, st *store.Store, sourceID int64, dialog CatalogDialog, updates []tg.UpdateClass) error {
	if st == nil || sourceID <= 0 || (dialog.Kind != "channel" && dialog.Kind != "supergroup") {
		return errors.New("invalid Telegram channel deletion target")
	}
	source, err := st.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return err
	}
	if source == nil || source.SourceType != telegram.SourceType {
		return errors.New("Telegram channel deletion requires a Telegram source")
	}
	marked, err := telegram.MarkedPeerID(&tg.PeerChannel{ChannelID: dialog.NativeID})
	if err != nil || marked != dialog.ChatID {
		return errors.New("invalid Telegram channel deletion peer")
	}
	for _, update := range updates {
		deleted, ok := update.(*tg.UpdateDeleteChannelMessages)
		if !ok {
			continue
		}
		if deleted == nil || deleted.ChannelID != dialog.NativeID {
			return errors.New("unexpected Telegram channel deletion peer")
		}
		for _, id := range deleted.Messages {
			if id <= 0 || int64(id) > math.MaxInt32 {
				return errors.New("invalid Telegram channel message ID")
			}
			if err := st.MarkMessageDeletedContext(ctx, sourceID, fmt.Sprintf("%d:%d", marked, id)); err != nil {
				return fmt.Errorf("mark Telegram channel deletion: %w", err)
			}
		}
	}
	return nil
}
