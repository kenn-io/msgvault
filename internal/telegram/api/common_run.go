package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/gotd/td/tg"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
)

// RunCommonDifferences drains account-wide updates, applying effects before
// each durable checkpoint. The caller holds the source sync execution.
func RunCommonDifferences(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, writer *telegram.LiveWriter, state *telegram.SyncState, dialogs []CatalogDialog, save func(context.Context, *telegram.SyncState) error) error {
	return runCommonDifferences(ctx, st, sourceID, client, writer, state, dialogs, time.Time{}, save)
}

func runCommonDifferences(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, writer *telegram.LiveWriter, state *telegram.SyncState, dialogs []CatalogDialog, after time.Time, save func(context.Context, *telegram.SyncState) error) (retErr error) {
	return runCommonDifferencesForChat(ctx, st, sourceID, client, writer, state, dialogs, 0, after, save)
}

// runSelectedCommonDifferences applies the selected dialog's common-box
// effects without persisting the shared account-wide update position.
func runSelectedCommonDifferences(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, writer *telegram.LiveWriter, state *telegram.SyncState, dialogs []CatalogDialog, chatID int64, after time.Time, save func(context.Context, *telegram.SyncState) error) error {
	if state == nil || state.Common == nil || chatID == 0 || save == nil {
		return errors.New("invalid selected Telegram common difference run")
	}
	initialCommon := *state.Common
	initialGaps := cloneTelegramRecoveryGaps(state.Gaps)
	localState := *state
	localCommon := initialCommon
	localState.Common = &localCommon
	localState.Gaps = cloneTelegramRecoveryGaps(initialGaps)
	selectedSave := func(ctx context.Context, next *telegram.SyncState) error {
		persisted := *next
		common := initialCommon
		persisted.Common = &common
		persisted.Gaps = cloneTelegramRecoveryGaps(initialGaps)
		if err := save(ctx, &persisted); err != nil {
			return err
		}
		state.Counts = persisted.Counts
		return nil
	}
	if err := runCommonDifferencesForChat(ctx, st, sourceID, client, writer, &localState, dialogs, chatID, after, selectedSave); err != nil {
		return err
	}
	state.Counts = localState.Counts
	return nil
}

func cloneTelegramRecoveryGaps(gaps map[string]telegram.RecoveryGap) map[string]telegram.RecoveryGap {
	if gaps == nil {
		return nil
	}
	clone := make(map[string]telegram.RecoveryGap, len(gaps))
	for key, gap := range gaps {
		clone[key] = gap
	}
	return clone
}

func runCommonDifferencesForChat(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, writer *telegram.LiveWriter, state *telegram.SyncState, dialogs []CatalogDialog, selectedChatID int64, after time.Time, save func(context.Context, *telegram.SyncState) error) (retErr error) {
	if st == nil || sourceID <= 0 || client == nil || writer == nil || state == nil || state.Common == nil || save == nil {
		return errors.New("invalid Telegram common difference run")
	}
	defer func() {
		retErr = errors.Join(retErr, writer.FlushConversationStats(context.WithoutCancel(ctx)))
	}()
	byID := make(map[int64]CatalogDialog, len(dialogs))
	for _, dialog := range dialogs {
		if _, exists := byID[dialog.ChatID]; exists {
			return errors.New("duplicate Telegram dialog")
		}
		byID[dialog.ChatID] = dialog
	}
	recoveries := 0
	for {
		page, err := GetCommonDifferencePage(ctx, client, *state.Common)
		if err != nil {
			return err
		}
		if page.TooLong {
			if recoveries >= 2 {
				return errors.New("Telegram common difference repeatedly exceeded recovery window")
			}
			if err := recoverCommonGap(ctx, client, writer, state, dialogs, selectedChatID, page.RecoveryPTS, after, save); err != nil {
				return err
			}
			recoveries++
			continue
		}
		err = CommitCommonDifference(ctx, page, state.Common, func(ctx context.Context, response tg.UpdatesDifferenceClass) error {
			return applyCommonDifference(ctx, st, sourceID, client, writer, byID, response, selectedChatID, after)
		}, func(ctx context.Context, next telegram.CommonUpdateState) error {
			if err := writer.FlushConversationStats(ctx); err != nil {
				return err
			}
			updated := *state
			updated.Common = &next
			updated.Counts = writer.Counts()
			if err := save(ctx, &updated); err != nil {
				return err
			}
			state.Counts = updated.Counts
			return nil
		})
		if err != nil {
			return err
		}
		if !page.More {
			return nil
		}
	}
}

func recoverCommonGap(ctx context.Context, client *tg.Client, writer *telegram.LiveWriter, state *telegram.SyncState, dialogs []CatalogDialog, selectedChatID int64, recoveryPTS int, after time.Time, save func(context.Context, *telegram.SyncState) error) error {
	remote, err := client.UpdatesGetState(ctx)
	if err != nil {
		return fmt.Errorf("get Telegram recovery state: %w", err)
	}
	if remote == nil {
		return errors.New("missing Telegram recovery state")
	}
	position := commonState(*remote)
	if !validCommonState(position) || position.Pts < recoveryPTS || position.Qts < state.Common.Qts {
		return errors.New("invalid Telegram recovery state")
	}
	now := time.Now().UTC()
	since := now.Add(-7 * 24 * time.Hour)
	if after.After(since) {
		since = after
	}
	for _, dialog := range dialogs {
		if selectedChatID != 0 && dialog.ChatID != selectedChatID {
			continue
		}
		if dialog.EnabledByDefault && (dialog.Kind == "user" || dialog.Kind == "group") {
			// A gap may include messages newer than the catalog snapshot used
			// to start this run. Use an unbounded ceiling to rescan from the
			// dialog's current top.
			dialog.TopMessage = math.MaxInt32
			if err := RescanDialogSince(ctx, client, writer, dialog, since); err != nil {
				return fmt.Errorf("rescan Telegram dialog %d: %w", dialog.ChatID, err)
			}
		}
	}
	gapStart := since
	if gapStart.After(now) {
		gapStart = now
	}
	if err := writer.FlushConversationStats(ctx); err != nil {
		return err
	}
	next, err := state.WithRecoveryGap("common", now, gapStart)
	if err != nil {
		return err
	}
	next.Common = &position
	next.Counts = writer.Counts()
	if err := save(ctx, next); err != nil {
		return fmt.Errorf("save Telegram recovery state: %w", err)
	}
	state.Common, state.Gaps, state.Counts = next.Common, next.Gaps, next.Counts
	slog.Warn("Telegram update gap: recent messages were rescanned, but older edits and deletions may be missing")
	return nil
}

func applyCommonDifference(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, writer *telegram.LiveWriter, dialogs map[int64]CatalogDialog, response tg.UpdatesDifferenceClass, selectedChatID int64, after time.Time) error {
	var messages []tg.MessageClass
	var updates []tg.UpdateClass
	var users []tg.UserClass
	var chats []tg.ChatClass
	switch difference := response.(type) {
	case *tg.UpdatesDifferenceEmpty:
		return nil
	case *tg.UpdatesDifference:
		messages, updates, users, chats = difference.NewMessages, difference.OtherUpdates, difference.Users, difference.Chats
	case *tg.UpdatesDifferenceSlice:
		messages, updates, users, chats = difference.NewMessages, difference.OtherUpdates, difference.Users, difference.Chats
	default:
		return errors.New("unsupported Telegram common difference")
	}
	applyMessage := func(item tg.MessageClass, edit bool) error {
		message, ok := item.(*tg.Message)
		if !ok {
			switch item.(type) {
			case *tg.MessageService, *tg.MessageEmpty:
				return nil
			default:
				return errors.New("unsupported Telegram difference message")
			}
		}
		if message == nil {
			return errors.New("invalid Telegram difference message")
		}
		_, hasEditDate := message.GetEditDate()
		if !after.IsZero() && !edit && !hasEditDate {
			if message.Date <= 0 {
				return errors.New("Telegram difference message has no timestamp")
			}
			if !time.Unix(int64(message.Date), 0).After(after) {
				return nil
			}
		}
		peer, err := telegram.MarkedPeerID(message.PeerID)
		if err != nil {
			return err
		}
		if selectedChatID != 0 && peer != selectedChatID {
			return nil
		}
		dialog, ok := dialogs[peer]
		if !ok {
			return errors.New("Telegram difference message has no catalog dialog")
		}
		if dialog.Kind != "user" && dialog.Kind != "group" {
			return errors.New("Telegram common difference has a channel message")
		}
		if !dialog.EnabledByDefault {
			return nil
		}
		if edit || hasEditDate {
			allowed, err := allowTelegramEditWithAfter(ctx, st, sourceID, peer, message, after)
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
			return fmt.Errorf("archive Telegram difference message: %w", err)
		}
	}
	for _, update := range updates {
		switch item := update.(type) {
		case *tg.UpdateNewMessage:
			if item == nil {
				return errors.New("invalid Telegram new-message update")
			}
			if err := applyMessage(item.Message, false); err != nil {
				return err
			}
		case *tg.UpdateEditMessage:
			if item == nil {
				return errors.New("invalid Telegram edit update")
			}
			if err := applyMessage(item.Message, true); err != nil {
				return err
			}
		case *tg.UpdateDeleteMessages:
			if selectedChatID == 0 {
				if err := ApplyCommonDeletions(ctx, st, sourceID, []tg.UpdateClass{item}); err != nil {
					return err
				}
			} else if dialog, ok := dialogs[selectedChatID]; ok && (dialog.Kind == "user" || dialog.Kind == "group") {
				if err := ApplyCommonDeletionsForDialog(ctx, st, sourceID, dialog, []tg.UpdateClass{item}); err != nil {
					return err
				}
			}
		case *tg.UpdateNewChannelMessage, *tg.UpdateEditChannelMessage, *tg.UpdateDeleteChannelMessages:
			var peer tg.PeerClass
			switch channelUpdate := item.(type) {
			case *tg.UpdateNewChannelMessage:
				if channelUpdate == nil {
					return errors.New("invalid Telegram channel update")
				}
				switch message := channelUpdate.Message.(type) {
				case *tg.Message:
					if message == nil {
						return errors.New("invalid Telegram channel update message")
					}
					peer = message.PeerID
				case *tg.MessageService, *tg.MessageEmpty:
					continue
				default:
					return errors.New("invalid Telegram channel update message")
				}
			case *tg.UpdateEditChannelMessage:
				if channelUpdate == nil {
					return errors.New("invalid Telegram channel update")
				}
				switch message := channelUpdate.Message.(type) {
				case *tg.Message:
					if message == nil {
						return errors.New("invalid Telegram channel update message")
					}
					peer = message.PeerID
				case *tg.MessageService, *tg.MessageEmpty:
					continue
				default:
					return errors.New("invalid Telegram channel update message")
				}
			case *tg.UpdateDeleteChannelMessages:
				if channelUpdate == nil {
					return errors.New("invalid Telegram channel update")
				}
				peer = &tg.PeerChannel{ChannelID: channelUpdate.ChannelID}
			}
			if _, ok := peer.(*tg.PeerChannel); !ok {
				return errors.New("invalid Telegram channel update peer")
			}
			id, err := telegram.MarkedPeerID(peer)
			if err != nil {
				return err
			}
			if selectedChatID != 0 && id != selectedChatID {
				continue
			}
			dialog, ok := dialogs[id]
			if !ok {
				return errors.New("Telegram channel update has no catalog dialog")
			}
			if dialog.EnabledByDefault {
				if err := applyChannelDifference(ctx, st, sourceID, client, writer, dialog, &tg.UpdatesChannelDifference{OtherUpdates: []tg.UpdateClass{update}, Users: users, Chats: chats}, after); err != nil {
					return err
				}
			}
		}
	}
	return writer.ResolveReplies(ctx)
}
