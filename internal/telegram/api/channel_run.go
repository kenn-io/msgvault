package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
)

// RunChannelDifferences applies each channel's effects before its applied pts
// advances. The caller holds the source sync execution.
func RunChannelDifferences(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, writer *telegram.LiveWriter, state *telegram.SyncState, dialog CatalogDialog, save func(context.Context, *telegram.SyncState) error) error {
	return runChannelDifferences(ctx, st, sourceID, client, writer, state, dialog, time.Time{}, save)
}

func runChannelDifferences(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, writer *telegram.LiveWriter, state *telegram.SyncState, dialog CatalogDialog, after time.Time, save func(context.Context, *telegram.SyncState) error) (retErr error) {
	if st == nil || sourceID <= 0 || client == nil || writer == nil || state == nil || save == nil || dialog.ChatID == 0 {
		return errors.New("invalid Telegram channel difference run")
	}
	defer func() {
		retErr = errors.Join(retErr, writer.FlushConversationStats(context.WithoutCancel(ctx)))
	}()
	key := strconv.FormatInt(dialog.ChatID, 10)
	recoveries := 0
	for {
		progress, ok := state.Dialogs[key]
		if !ok || progress.ChannelPTS == nil {
			return errors.New("Telegram channel applied pts is unavailable")
		}
		pts := *progress.ChannelPTS
		page, err := GetChannelDifferencePage(ctx, client, dialog, pts)
		if err != nil {
			if tgerr.Is(err, "CHANNEL_PRIVATE") {
				return recordChannelInaccessible(ctx, state, key, save)
			}
			return err
		}
		if page.TooLong {
			if recoveries >= 2 {
				return errors.New("Telegram channel difference repeatedly exceeded recovery window")
			}
			if err := recoverChannelGap(ctx, st, sourceID, client, writer, state, dialog, page, after, save); err != nil {
				if tgerr.Is(err, "CHANNEL_PRIVATE") {
					return recordChannelInaccessible(ctx, state, key, save)
				}
				return err
			}
			recoveries++
			continue
		}
		err = CommitChannelDifference(ctx, page, dialog.ChatID, &pts, func(ctx context.Context, response tg.UpdatesChannelDifferenceClass) error {
			return applyChannelDifference(ctx, st, sourceID, client, writer, dialog, response, after)
		}, func(ctx context.Context, next int) error {
			if err := writer.FlushConversationStats(ctx); err != nil {
				return err
			}
			updated := state.Dialogs[key]
			updated.ChannelPTS = &next
			state.Counts = writer.Counts()
			return saveDialogState(ctx, state, key, updated, save)
		})
		if err != nil {
			return err
		}
		if !page.More {
			return nil
		}
	}
}

func recordChannelInaccessible(ctx context.Context, state *telegram.SyncState, key string, save func(context.Context, *telegram.SyncState) error) error {
	progress, ok := state.Dialogs[key]
	if !ok || progress.Peer == nil {
		return errors.New("missing Telegram channel catalog state")
	}
	peer := *progress.Peer
	if peer.InaccessibleSince == nil {
		now := time.Now().UTC()
		peer.InaccessibleSince = &now
	}
	progress.Peer = &peer
	if err := saveDialogState(ctx, state, key, progress, save); err != nil {
		return err
	}
	slog.Warn("Telegram channel is inaccessible; its update position was retained")
	return nil
}

func validateChannelMessagePeer(dialog CatalogDialog, message *tg.Message) error {
	peerID, err := telegram.MarkedPeerID(message.PeerID)
	if err != nil || peerID != dialog.ChatID {
		return errors.New("Telegram message does not match dialog")
	}
	return nil
}

func recoverChannelGap(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, writer *telegram.LiveWriter, state *telegram.SyncState, dialog CatalogDialog, page ChannelDifferencePage, after time.Time, save func(context.Context, *telegram.SyncState) error) error {
	difference, ok := page.Response.(*tg.UpdatesChannelDifferenceTooLong)
	if !ok || difference == nil || page.RecoveryPTS == nil {
		return errors.New("missing Telegram channel recovery position")
	}
	remote := difference.Dialog.(*tg.Dialog) // validated by GetChannelDifferencePage
	if remote.TopMessage > dialog.TopMessage {
		dialog.TopMessage = remote.TopMessage
	}
	for _, item := range difference.Messages {
		message, ok := item.(*tg.Message)
		if !ok {
			switch item.(type) {
			case *tg.MessageService, *tg.MessageEmpty:
				continue
			default:
				return errors.New("unsupported Telegram channel recovery message")
			}
		}
		if message == nil {
			return errors.New("invalid Telegram channel recovery message")
		}
		if err := validateChannelMessagePeer(dialog, message); err != nil {
			return err
		}
		_, hasEditDate := message.GetEditDate()
		if hasEditDate {
			allowed, err := allowTelegramEditWithAfter(ctx, st, sourceID, dialog.ChatID, message, after)
			if err != nil {
				return err
			}
			if !allowed {
				continue
			}
		}
		if !after.IsZero() && !hasEditDate {
			if message.Date <= 0 {
				return errors.New("Telegram recovery message has no timestamp")
			}
			if !time.Unix(int64(message.Date), 0).After(after) {
				continue
			}
		}
		var err error
		if hasEditDate {
			err = ApplyAPIEditAuthoritative(ctx, writer, dialog, message, difference.Users, difference.Chats)
		} else {
			err = ArchiveAPIMessageWithWriter(ctx, writer, dialog, message, difference.Users, difference.Chats)
		}
		if err != nil {
			return fmt.Errorf("archive Telegram channel recovery message: %w", err)
		}
	}
	now := time.Now().UTC()
	since := now.Add(-7 * 24 * time.Hour)
	if after.After(since) {
		since = after
	}
	if err := RescanDialogSince(ctx, client, writer, dialog, since); err != nil {
		return fmt.Errorf("rescan Telegram channel: %w", err)
	}
	if err := writer.FlushConversationStats(ctx); err != nil {
		return err
	}
	key := strconv.FormatInt(dialog.ChatID, 10)
	gapStart := since
	if gapStart.After(now) {
		gapStart = now
	}
	next, err := state.WithRecoveryGap(key, now, gapStart)
	if err != nil {
		return err
	}
	next.Dialogs = make(map[string]telegram.DialogState, len(state.Dialogs))
	for k, value := range state.Dialogs {
		next.Dialogs[k] = value
	}
	progress := next.Dialogs[key]
	pts := *page.RecoveryPTS
	progress.ChannelPTS = &pts
	next.Dialogs[key] = progress
	next.Counts = writer.Counts()
	if err := save(ctx, next); err != nil {
		return fmt.Errorf("save Telegram channel recovery state: %w", err)
	}
	state.Dialogs, state.Gaps, state.Counts = next.Dialogs, next.Gaps, next.Counts
	slog.Warn("Telegram channel update gap: recent messages were rescanned, but older edits and deletions may be missing")
	return nil
}

func applyChannelDifference(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, writer *telegram.LiveWriter, dialog CatalogDialog, response tg.UpdatesChannelDifferenceClass, after time.Time) error {
	var messages []tg.MessageClass
	var updates []tg.UpdateClass
	var users []tg.UserClass
	var chats []tg.ChatClass
	switch difference := response.(type) {
	case *tg.UpdatesChannelDifferenceEmpty:
		return nil
	case *tg.UpdatesChannelDifference:
		messages, updates, users, chats = difference.NewMessages, difference.OtherUpdates, difference.Users, difference.Chats
	default:
		return errors.New("unsupported Telegram channel difference")
	}
	applyMessage := func(item tg.MessageClass, edit bool) error {
		message, ok := item.(*tg.Message)
		if !ok {
			switch item.(type) {
			case *tg.MessageService, *tg.MessageEmpty:
				return nil
			default:
				return errors.New("unsupported Telegram channel difference message")
			}
		}
		if message == nil {
			return errors.New("invalid Telegram channel difference message")
		}
		if err := validateChannelMessagePeer(dialog, message); err != nil {
			return err
		}
		_, hasEditDate := message.GetEditDate()
		if !after.IsZero() && !edit && !hasEditDate {
			if message.Date <= 0 {
				return errors.New("Telegram channel difference message has no timestamp")
			}
			if !time.Unix(int64(message.Date), 0).After(after) {
				return nil
			}
		}
		if edit || hasEditDate {
			allowed, err := allowTelegramEditWithAfter(ctx, st, sourceID, dialog.ChatID, message, after)
			if err != nil {
				return err
			}
			if !allowed {
				return nil
			}
		}
		if edit {
			return ApplyAPIEditWithClient(ctx, client, writer, dialog, message, users, chats)
		}
		return ArchiveAPIMessageWithWriter(ctx, writer, dialog, message, users, chats)
	}
	for _, message := range messages {
		if err := applyMessage(message, false); err != nil {
			return fmt.Errorf("archive Telegram channel difference message: %w", err)
		}
	}
	for _, update := range updates {
		switch item := update.(type) {
		case *tg.UpdateNewChannelMessage:
			if item == nil {
				return errors.New("invalid Telegram new-channel-message update")
			}
			if err := applyMessage(item.Message, false); err != nil {
				return err
			}
		case *tg.UpdateEditChannelMessage:
			if item == nil {
				return errors.New("invalid Telegram channel edit update")
			}
			if err := applyMessage(item.Message, true); err != nil {
				return err
			}
		case *tg.UpdateDeleteChannelMessages:
			if err := ApplyChannelDeletions(ctx, st, sourceID, dialog, []tg.UpdateClass{item}); err != nil {
				return err
			}
		case *tg.UpdateNewMessage, *tg.UpdateEditMessage, *tg.UpdateDeleteMessages:
			return errors.New("Telegram common update requires common difference handling")
		}
	}
	return writer.ResolveReplies(ctx)
}
