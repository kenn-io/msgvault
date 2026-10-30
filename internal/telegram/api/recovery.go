package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
	"go.kenn.io/msgvault/internal/telegram"
)

// RescanDialogSince re-reads a bounded trailing window without inferring
// deletions from absent messages. The caller holds the source execution.
func RescanDialogSince(ctx context.Context, client *tg.Client, writer *telegram.LiveWriter, dialog CatalogDialog, since time.Time) error {
	if client == nil || writer == nil || since.IsZero() || dialog.TopMessage < 0 {
		return errors.New("invalid Telegram trailing rescan")
	}
	if dialog.TopMessage == 0 {
		return nil
	}
	offset := 0
	for {
		page, err := GetHistoryPage(ctx, client, dialog, offset, 0, dialog.TopMessage)
		if err != nil {
			return err
		}
		if page.Empty {
			return writer.ResolveReplies(ctx)
		}
		for _, item := range page.Messages {
			message, ok := item.(*tg.Message)
			if !ok {
				switch item.(type) {
				case *tg.MessageService, *tg.MessageEmpty:
					continue
				default:
					return errors.New("unsupported Telegram rescan message")
				}
			}
			if message == nil || message.Date <= 0 {
				return errors.New("invalid Telegram rescan message")
			}
			if !time.Unix(int64(message.Date), 0).After(since) {
				// messages.getHistory orders results by descending date.
				return writer.ResolveReplies(ctx)
			}
			if _, edited := message.GetEditDate(); edited {
				err = ApplyAPIEditAuthoritative(ctx, writer, dialog, message, page.Users, page.Chats)
			} else {
				err = ArchiveAPIMessageWithWriter(ctx, writer, dialog, message, page.Users, page.Chats)
			}
			if err != nil {
				return fmt.Errorf("archive Telegram rescan message: %w", err)
			}
		}
		offset = page.NextOffset
	}
}
