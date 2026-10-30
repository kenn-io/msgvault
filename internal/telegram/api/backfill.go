package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
	"go.kenn.io/msgvault/internal/telegram"
)

// ArchiveHistoryPage writes a history page and resolves its replies before
// saving the new cursor. The caller holds the source sync execution and uses
// the same writer for the whole run.
func ArchiveHistoryPage(ctx context.Context, client *tg.Client, writer *telegram.LiveWriter, dialog CatalogDialog, state *telegram.DialogState, save func(context.Context, telegram.DialogState) error) (retErr error) {
	if writer == nil || save == nil {
		return errors.New("invalid Telegram history writer")
	}
	defer func() {
		retErr = errors.Join(retErr, writer.FlushConversationStats(context.WithoutCancel(ctx)))
	}()
	var messageIDs []int64
	return BackfillPage(ctx, client, dialog, state,
		func(ctx context.Context, message tg.MessageClass, users []tg.UserClass, chats []tg.ChatClass) error {
			switch m := message.(type) {
			case *tg.Message:
				if err := ArchiveAPIMessageWithWriter(ctx, writer, dialog, m, users, chats); err != nil {
					return err
				}
				messageIDs = append(messageIDs, int64(m.ID))
				return nil
			case *tg.MessageService, *tg.MessageEmpty:
				return nil
			default:
				return errors.New("unsupported Telegram history message")
			}
		},
		func(ctx context.Context, next telegram.DialogState) error {
			if err := writer.ResolveRepliesForMessages(ctx, dialog.ChatID, messageIDs); err != nil {
				return err
			}
			if err := writer.FlushConversationStats(ctx); err != nil {
				return err
			}
			return save(ctx, next)
		})
}

// BackfillPage fetches and persists one page of an already checkpointed
// snapshot. The caller must persist update baselines and the initial History
// state before calling it. write must be replay-safe and complete all required
// side effects before returning. save must durably persist the new state.
func BackfillPage(ctx context.Context, client *tg.Client, dialog CatalogDialog, state *telegram.DialogState,
	write func(context.Context, tg.MessageClass, []tg.UserClass, []tg.ChatClass) error,
	save func(context.Context, telegram.DialogState) error,
) error {
	if state == nil || state.History == nil || client == nil || write == nil || save == nil {
		return errors.New("invalid Telegram backfill page arguments")
	}
	if err := state.Validate(); err != nil {
		return err
	}
	progress := *state.History
	pageClient := client
	if len(progress.Ranges) > 0 {
		messageRange := progress.Ranges[progress.RangeIndex]
		pageClient = tg.NewClient(messagesRangeInvoker{
			next: client.Invoker(), range_: tg.MessageRange{MinID: messageRange.MinID, MaxID: messageRange.MaxID},
		})
	}
	page, err := GetHistoryPage(ctx, pageClient, dialog, progress.Offset, progress.MinID, progress.Ceiling)
	if err != nil {
		return err
	}
	reachedAfter := false
	for _, message := range page.Messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !progress.After.IsZero() {
			var date int
			var dated bool
			switch item := message.(type) {
			case *tg.Message:
				date, dated = item.Date, true
			case *tg.MessageService:
				date, dated = item.Date, true
			}
			if dated && date <= 0 {
				return errors.New("Telegram history message has no timestamp")
			}
			if dated && !time.Unix(int64(date), 0).After(progress.After) {
				// messages.getHistory orders results by descending date.
				reachedAfter = true
				break
			}
		}
		if err := write(ctx, message, page.Users, page.Chats); err != nil {
			return fmt.Errorf("archive Telegram history page: %w", err)
		}
	}
	next := *state
	if page.Empty || reachedAfter {
		if progress.RangeIndex+1 < len(progress.Ranges) {
			progress.RangeIndex++
			progress.Offset = 0
			next.History = &progress
		} else {
			if progress.Ceiling > next.Frontier {
				next.Frontier = progress.Ceiling
			}
			next.History = nil
		}
	} else {
		progress.Offset = page.NextOffset
		next.History = &progress
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := save(ctx, next); err != nil {
		return fmt.Errorf("save Telegram history checkpoint: %w", err)
	}
	*state = next
	return nil
}
