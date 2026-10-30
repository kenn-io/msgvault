package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
	"go.kenn.io/msgvault/internal/store"
)

// allowTelegramEditWithAfter keeps an --after run from importing an older
// edit unless its source message is already archived.
func allowTelegramEditWithAfter(ctx context.Context, st *store.Store, sourceID, peerID int64, message *tg.Message, after time.Time) (bool, error) {
	if after.IsZero() {
		return true, nil
	}
	if st == nil || sourceID <= 0 || peerID == 0 || message == nil {
		return false, errors.New("invalid Telegram edit after-bound check")
	}
	if message.Date <= 0 {
		return false, errors.New("Telegram edit has no message timestamp")
	}
	if time.Unix(int64(message.Date), 0).After(after) {
		return true, nil
	}
	historyLink, err := st.TelegramHistoryMigrationContext(ctx, sourceID, peerID)
	if err != nil {
		return false, fmt.Errorf("load Telegram edit history linkage: %w", err)
	}
	key, err := store.TelegramHistoryMessageKey(peerID, int64(message.ID), historyLink)
	if err != nil {
		return false, err
	}
	archived, err := st.MessageMetadataBatchContext(ctx, sourceID, []string{key})
	if err != nil {
		return false, fmt.Errorf("find Telegram edit target: %w", err)
	}
	_, exists := archived[key]
	return exists, nil
}
