package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/gotd/td/tg"
	"go.kenn.io/msgvault/internal/telegram"
)

// BootstrapUpdates records Telegram's common-box state before history starts.
// The caller serializes runs and save durably persists the whole checkpoint.
func BootstrapUpdates(ctx context.Context, client *tg.Client, state *telegram.SyncState, save func(context.Context, *telegram.SyncState) error) error {
	if state == nil || client == nil || save == nil {
		return errors.New("invalid Telegram update bootstrap arguments")
	}
	if _, err := state.Marshal(); err != nil {
		return err
	}
	if state.Common != nil {
		return nil
	}
	remote, err := client.UpdatesGetState(ctx)
	if err != nil {
		return fmt.Errorf("get Telegram update state: %w", err)
	}
	if remote == nil {
		return errors.New("missing Telegram update state")
	}
	next := *state
	next.Common = &telegram.CommonUpdateState{Pts: remote.Pts, Qts: remote.Qts, Date: remote.Date, Seq: remote.Seq}
	if _, err := next.Marshal(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := save(ctx, &next); err != nil {
		return fmt.Errorf("save Telegram update state: %w", err)
	}
	state.Common = next.Common
	return nil
}
