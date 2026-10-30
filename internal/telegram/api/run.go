package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
)

// HistoryOptions control a manual history scan. Scheduled runs use defaults.
type HistoryOptions struct {
	EnabledChannels []int64
	Full            bool
	After           time.Time
	Takeout         bool
	MediaProcessor  func(context.Context, telegram.Chat, *telegram.LiveMedia) error
	Media           *MediaOptions
}

// MediaOptions configure attachment policy, content storage and downloader.
type MediaOptions struct {
	AttachmentsDir string
	TempDir        string
	Policy         attachmentpolicy.Policy
	Download       func(context.Context, *MediaReference, string, int64) (*os.File, error)
}

// RunAuthenticatedBackfill verifies the account before repairing legacy keys
// and writing history. The caller owns the source execution for the run.
func RunAuthenticatedBackfill(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, state *telegram.SyncState, observedAt time.Time, includeChannels bool, chatID int64, save func(context.Context, *telegram.SyncState) error) error {
	return RunAuthenticatedBackfillWithOptions(ctx, st, sourceID, client, state, observedAt, includeChannels, chatID, HistoryOptions{}, save)
}

// RunAuthenticatedBackfillWithOptions applies manual history options after
// verifying the account and resuming any previously checkpointed scan.
func RunAuthenticatedBackfillWithOptions(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, state *telegram.SyncState, observedAt time.Time, includeChannels bool, chatID int64, options HistoryOptions, save func(context.Context, *telegram.SyncState) error) error {
	if st == nil || client == nil || state == nil || save == nil || sourceID <= 0 {
		return errors.New("invalid authenticated Telegram backfill")
	}
	owner, err := Owner(ctx, client)
	if err != nil {
		return err
	}
	source, err := st.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return err
	}
	if source == nil || source.SourceType != telegram.SourceType || source.Identifier != telegram.OwnerSourceIdentifier(owner.ID) {
		return errors.New("Telegram account does not match archive source")
	}
	overlap, overlapErr := st.HasBeeperTelegramOwnerContext(ctx, owner.ID)
	if overlapErr != nil {
		return fmt.Errorf("check Telegram account overlap: %w", overlapErr)
	}
	if overlap {
		slog.Warn("This Telegram account is also archived through Beeper; messages may be duplicated")
	}
	if options.Takeout {
		_, err := st.GetLastSuccessfulSyncByTypeContext(ctx, sourceID, "sync-telegram")
		if err == nil && !hasPendingTakeoutHistory(state) {
			return errors.New("Telegram takeout is available only for the first backfill")
		}
		if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
			return err
		}
	}
	return runAuthenticatedBackfill(ctx, st, sourceID, client, owner, state, observedAt, includeChannels, chatID, options, save)
}

func hasPendingTakeoutHistory(state *telegram.SyncState) bool {
	if state == nil {
		return false
	}
	for _, dialog := range state.Dialogs {
		if dialog.History != nil && len(dialog.History.Ranges) > 0 {
			return true
		}
	}
	return false
}

func runAuthenticatedBackfill(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, owner *tg.User, state *telegram.SyncState, observedAt time.Time, includeChannels bool, chatID int64, options HistoryOptions, save func(context.Context, *telegram.SyncState) error) error {
	if err := BootstrapUpdates(ctx, client, state, save); err != nil {
		return err
	}
	dialogs, err := ListDialogs(ctx, client)
	if err != nil {
		return err
	}
	dialogs, err = enableConfiguredChannels(dialogs, options.EnabledChannels)
	if err != nil {
		return err
	}
	if err := RepairAmbiguousPeers(ctx, st, sourceID, client, dialogs); err != nil {
		return err
	}
	formerGroups, err := repairAndDiscoverFormerGroups(ctx, st, sourceID, client, dialogs, chatID)
	if err != nil {
		return err
	}
	dialogs = attachFormerGroups(dialogs, formerGroups, nil)
	writer, err := telegram.NewLiveWriter(ctx, st, sourceID, owner.ID, strings.TrimSpace(owner.FirstName+" "+owner.LastName))
	if err != nil {
		return err
	}
	if options.Media != nil {
		if options.MediaProcessor != nil || options.Media.AttachmentsDir == "" || options.Media.TempDir == "" || options.Media.Download == nil {
			return errors.New("invalid Telegram media options")
		}
		if err := options.Media.Policy.Validate(); err != nil {
			return err
		}
		writer.SetMediaProcessor(newMediaProcessor(options.Media.Policy, dialogs, options.Media.AttachmentsDir, options.Media.TempDir, options.Media.Download))
	} else {
		writer.SetMediaProcessor(options.MediaProcessor)
	}
	var backfillErr error
	if options.Takeout {
		backfillErr = WithTakeout(ctx, client, func(ctx context.Context, takeout *tg.Client) error {
			nativeRanges, err := takeout.MessagesGetSplitRanges(ctx)
			if err != nil {
				return fmt.Errorf("list Telegram takeout message ranges: %w", err)
			}
			if len(nativeRanges) == 0 {
				return errors.New("Telegram takeout returned no message ranges")
			}
			ranges := make([]telegram.HistoryRange, len(nativeRanges))
			for index, messageRange := range nativeRanges {
				if messageRange.MinID < 0 || messageRange.MaxID <= messageRange.MinID || int64(messageRange.MaxID) > math.MaxInt32 {
					return errors.New("Telegram returned an invalid takeout message range")
				}
				ranges[index] = telegram.HistoryRange{MinID: messageRange.MinID, MaxID: messageRange.MaxID}
			}
			rangedDialogs, err := ListDialogsForRanges(ctx, takeout, ranges)
			if err != nil {
				return err
			}
			rangedDialogs, err = enableConfiguredChannels(rangedDialogs, options.EnabledChannels)
			if err != nil {
				return err
			}
			rangedDialogs = attachFormerGroups(rangedDialogs, formerGroups, ranges)
			return backfillDialogsWithOptions(ctx, takeout, writer, state, rangedDialogs, observedAt, includeChannels, chatID, options, save)
		})
	} else {
		backfillErr = backfillDialogsWithOptions(ctx, client, writer, state, dialogs, observedAt, includeChannels, chatID, options, save)
	}
	if backfillErr != nil {
		return backfillErr
	}
	// Common updates share one account-wide cursor. A chat-filtered pass applies
	// only the selected dialog against a private cursor copy, leaving other
	// dialogs for the next unrestricted run.
	if chatID == 0 {
		if err := runCommonDifferences(ctx, st, sourceID, client, writer, state, dialogs, options.After, save); err != nil {
			return err
		}
	} else {
		if err := runSelectedCommonDifferences(ctx, st, sourceID, client, writer, state, dialogs, chatID, options.After, save); err != nil {
			return err
		}
	}
	for _, dialog := range dialogs {
		if selectedDialog(dialog, includeChannels, chatID) && (dialog.Kind == "channel" || dialog.Kind == "supergroup") {
			progress := state.Dialogs[strconv.FormatInt(dialog.ChatID, 10)]
			if progress.Peer != nil && progress.Peer.InaccessibleSince != nil {
				continue
			}
			if err := runChannelDifferences(ctx, st, sourceID, client, writer, state, dialog, options.After, save); err != nil {
				return err
			}
		}
	}
	return nil
}

// BackfillDiscoveredDialogs captures the common baseline, refreshes the full
// catalog, and walks enabled dialogs through durable history snapshots. The
// caller owns the source execution and must provide a durable state saver.
func BackfillDiscoveredDialogs(ctx context.Context, client *tg.Client, writer *telegram.LiveWriter, state *telegram.SyncState, observedAt time.Time, includeChannels bool, chatID int64, save func(context.Context, *telegram.SyncState) error) error {
	if client == nil || writer == nil || state == nil || save == nil {
		return errors.New("invalid Telegram backfill run")
	}
	if err := BootstrapUpdates(ctx, client, state, save); err != nil {
		return err
	}
	dialogs, err := ListDialogs(ctx, client)
	if err != nil {
		return err
	}
	return backfillDialogs(ctx, client, writer, state, dialogs, observedAt, includeChannels, chatID, save)
}

func backfillDialogs(ctx context.Context, client *tg.Client, writer *telegram.LiveWriter, state *telegram.SyncState, dialogs []CatalogDialog, observedAt time.Time, includeChannels bool, chatID int64, save func(context.Context, *telegram.SyncState) error) error {
	return backfillDialogsWithOptions(ctx, client, writer, state, dialogs, observedAt, includeChannels, chatID, HistoryOptions{}, save)
}

func backfillDialogsWithOptions(ctx context.Context, client *tg.Client, writer *telegram.LiveWriter, state *telegram.SyncState, dialogs []CatalogDialog, observedAt time.Time, includeChannels bool, chatID int64, options HistoryOptions, save func(context.Context, *telegram.SyncState) error) (retErr error) {
	if writer == nil {
		return errors.New("missing Telegram live writer")
	}
	defer func() {
		if err := writer.ResolveReplies(ctx); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("reconcile Telegram replies: %w", err))
		}
		retErr = errors.Join(retErr, writer.FlushConversationStats(context.WithoutCancel(ctx)))
	}()
	if err := RefreshCatalog(ctx, state, dialogs, observedAt, save); err != nil {
		return err
	}
	if chatID != 0 && writer.RequiresShiftedHistoryRepair(chatID) {
		return errors.New("requested Telegram dialog still has shifted history awaiting verified migration linkage")
	}
	// Capture each selected channel's update position before any history walk.
	for _, dialog := range dialogs {
		if chatID == 0 && (dialog.Inaccessible || dialog.UnsupportedReason != "") {
			continue
		}
		if !selectedDialog(dialog, includeChannels, chatID) || (dialog.Kind != "channel" && dialog.Kind != "supergroup") {
			continue
		}
		if writer.RequiresShiftedHistoryRepair(dialog.ChatID) {
			continue
		}
		if dialog.Inaccessible || dialog.UnsupportedReason != "" {
			return unsupportedDialogError(dialog)
		}
		key := strconv.FormatInt(dialog.ChatID, 10)
		progress := state.Dialogs[key]
		if progress.ChannelPTS != nil {
			continue
		}
		if !dialog.HasObservedPTS {
			return fmt.Errorf("Telegram channel %s has no safe initial update position", key)
		}
		pts := dialog.ObservedPTS
		progress.ChannelPTS = &pts
		if progress.Frontier != 0 || progress.History != nil {
			since := observedAt.Add(-7 * 24 * time.Hour)
			if options.After.After(since) {
				since = options.After
			}
			if err := RescanDialogSince(ctx, client, writer, dialog, since); err != nil {
				if tgerr.Is(err, "CHANNEL_PRIVATE") {
					if err := recordChannelInaccessible(ctx, state, key, save); err != nil {
						return err
					}
					continue
				}
				return fmt.Errorf("rescan Telegram channel %s: %w", key, err)
			}
			if err := writer.FlushConversationStats(ctx); err != nil {
				return err
			}
			gapStart := since
			if gapStart.After(observedAt) {
				gapStart = observedAt
			}
			next, err := state.WithRecoveryGap(key, observedAt, gapStart)
			if err != nil {
				return err
			}
			next.Dialogs = make(map[string]telegram.DialogState, len(state.Dialogs))
			for k, value := range state.Dialogs {
				next.Dialogs[k] = value
			}
			next.Dialogs[key] = progress
			next.Counts = writer.Counts()
			if err := save(ctx, next); err != nil {
				return fmt.Errorf("save Telegram channel recovery state: %w", err)
			}
			*state = *next
			slog.Warn("Telegram channel update position was missing: recent messages were rescanned, but older edits and deletions may be missing")
			continue
		}
		if err := saveDialogState(ctx, state, key, progress, save); err != nil {
			return err
		}
	}
	matched := chatID == 0
	for _, dialog := range dialogs {
		if chatID != 0 && dialog.ChatID != chatID && dialog.MigrationParentChatID != chatID {
			continue
		}
		if chatID != 0 {
			matched = true
			if dialog.Inaccessible || dialog.UnsupportedReason != "" {
				return unsupportedDialogError(dialog)
			}
		}
		if chatID == 0 && (dialog.Inaccessible || dialog.UnsupportedReason != "") {
			continue
		}
		if writer.RequiresShiftedHistoryRepair(dialog.ChatID) {
			continue
		}
		if chatID == 0 && !dialog.EnabledByDefault && !(includeChannels && dialog.Kind == "channel") {
			continue
		}
		key := strconv.FormatInt(dialog.ChatID, 10)
		if peer := state.Dialogs[key].Peer; peer != nil && peer.InaccessibleSince != nil {
			continue
		}
		fullCompleted := false
		for {
			progress := state.Dialogs[key]
			if progress.History != nil && len(progress.History.Ranges) > 0 && !options.Takeout {
				return errors.New("resume Telegram takeout history with --takeout")
			}
			if options.Takeout && progress.History != nil && len(progress.History.Ranges) == 0 {
				if len(dialog.MessageRanges) == 0 {
					return errors.New("Telegram takeout dialog has no split ranges")
				}
				resumed := *progress.History
				resumed.Offset = 0
				resumed.Ranges = append([]telegram.HistoryRange(nil), dialog.MessageRanges...)
				progress.History = &resumed
				if err := saveDialogState(ctx, state, key, progress, save); err != nil {
					return err
				}
			}
			if progress.History == nil {
				if options.Takeout && len(dialog.MessageRanges) == 0 {
					return errors.New("Telegram takeout dialog has no split ranges")
				}
				full := options.Full && !fullCompleted
				if dialog.TopMessage <= 0 || (!full && dialog.TopMessage <= progress.Frontier) {
					break
				}
				minID := progress.Frontier
				if full {
					minID = 0
				}
				progress.History = &telegram.HistoryProgress{Ceiling: dialog.TopMessage, MinID: minID, Ranges: append([]telegram.HistoryRange(nil), dialog.MessageRanges...), After: options.After, Full: full}
				if err := saveDialogState(ctx, state, key, progress, save); err != nil {
					return err
				}
			}
			wasFull := progress.History.Full
			completedAfter := progress.History.After
			if err := ArchiveHistoryPage(ctx, client, writer, dialog, &progress, func(ctx context.Context, next telegram.DialogState) error {
				state.Counts = writer.Counts()
				return saveDialogState(ctx, state, key, next, save)
			}); err != nil {
				if (dialog.Kind == "channel" || dialog.Kind == "supergroup") && tgerr.Is(err, "CHANNEL_PRIVATE") {
					if err := recordChannelInaccessible(ctx, state, key, save); err != nil {
						return err
					}
					break
				}
				return fmt.Errorf("backfill Telegram dialog %s: %w", key, err)
			}
			if wasFull && progress.History == nil && completedAfter.Equal(options.After) {
				fullCompleted = true
			}
		}
		if options.Full && dialog.TopMessage > 0 {
			if peer := state.Dialogs[key].Peer; peer != nil && peer.InaccessibleSince != nil {
				continue
			}
			since := observedAt.Add(-7 * 24 * time.Hour)
			if options.After.After(since) {
				since = options.After
			}
			if err := RescanDialogSince(ctx, client, writer, dialog, since); err != nil {
				if (dialog.Kind == "channel" || dialog.Kind == "supergroup") && tgerr.Is(err, "CHANNEL_PRIVATE") {
					if err := recordChannelInaccessible(ctx, state, key, save); err != nil {
						return err
					}
					continue
				}
				return fmt.Errorf("rescan Telegram dialog %s: %w", key, err)
			}
			state.Counts = writer.Counts()
			if err := save(ctx, state); err != nil {
				return fmt.Errorf("save Telegram rescan state: %w", err)
			}
		}
	}
	if !matched {
		return errors.New("requested Telegram dialog is unavailable")
	}
	return nil
}

func selectedDialog(dialog CatalogDialog, includeChannels bool, chatID int64) bool {
	if chatID != 0 {
		return dialog.ChatID == chatID
	}
	return dialog.EnabledByDefault || (includeChannels && dialog.Kind == "channel")
}

func unsupportedDialogError(dialog CatalogDialog) error {
	reason := dialog.UnsupportedReason
	if reason == "" && dialog.Inaccessible {
		reason = "Telegram dialog is inaccessible"
	}
	return fmt.Errorf("Telegram dialog %s is unsupported: %s", strconv.FormatInt(dialog.ChatID, 10), reason)
}

func saveDialogState(ctx context.Context, state *telegram.SyncState, key string, dialog telegram.DialogState, save func(context.Context, *telegram.SyncState) error) error {
	next := *state
	next.Dialogs = make(map[string]telegram.DialogState, len(state.Dialogs))
	for key, value := range state.Dialogs {
		next.Dialogs[key] = value
	}
	next.Dialogs[key] = dialog
	if err := save(ctx, &next); err != nil {
		return err
	}
	state.Dialogs = next.Dialogs
	return nil
}
