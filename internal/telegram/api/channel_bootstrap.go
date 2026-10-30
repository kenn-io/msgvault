package api

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/gotd/td/tg"

	"go.kenn.io/msgvault/internal/telegram"
)

// BootstrapChannel persists a channel's initial observed pts before history.
// Later observations must never replace the last applied position.
func BootstrapChannel(ctx context.Context, dialog CatalogDialog, state *telegram.DialogState, save func(context.Context, telegram.DialogState) error) error {
	if state == nil || save == nil || (dialog.Kind != "channel" && dialog.Kind != "supergroup") || dialog.NativeID <= 0 {
		return errors.New("invalid Telegram channel bootstrap")
	}
	peer, err := telegram.MarkedPeerID(&tg.PeerChannel{ChannelID: dialog.NativeID})
	if err != nil || (dialog.ChatID != 0 && dialog.ChatID != peer) {
		return errors.New("invalid Telegram channel peer")
	}
	if err := state.Validate(); err != nil {
		return err
	}
	if state.ChannelPTS != nil {
		return nil
	}
	if !dialog.HasObservedPTS || dialog.ObservedPTS < 0 || int64(dialog.ObservedPTS) > math.MaxInt32 {
		return errors.New("Telegram channel pts is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	next := *state
	pts := dialog.ObservedPTS
	next.ChannelPTS = &pts
	if err := save(ctx, next); err != nil {
		return fmt.Errorf("save Telegram channel pts: %w", err)
	}
	*state = next
	return nil
}
