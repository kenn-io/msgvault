package api

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/gotd/td/tg"
	"go.kenn.io/msgvault/internal/telegram"
)

// ChannelDifferencePage keeps response effects separate from the proposed pts.
type ChannelDifferencePage struct {
	Response    tg.UpdatesChannelDifferenceClass
	ChatID      int64
	PreviousPTS int
	NextPTS     *int
	More        bool
	TooLong     bool
	RecoveryPTS *int
}

// GetChannelDifferencePage fetches one channel update page without saving pts.
// A too-long page must be processed and its gap recorded before recovery.
func GetChannelDifferencePage(ctx context.Context, client *tg.Client, dialog CatalogDialog, pts int) (ChannelDifferencePage, error) {
	var out ChannelDifferencePage
	if client == nil || (dialog.Kind != "channel" && dialog.Kind != "supergroup") || dialog.NativeID <= 0 || pts < 0 || int64(pts) > math.MaxInt32 {
		return out, errors.New("invalid Telegram channel difference request")
	}
	expected, err := telegram.MarkedPeerID(&tg.PeerChannel{ChannelID: dialog.NativeID})
	if err != nil || (dialog.ChatID != 0 && dialog.ChatID != expected) {
		return out, errors.New("invalid Telegram channel peer")
	}
	response, err := client.UpdatesGetChannelDifference(ctx, &tg.UpdatesGetChannelDifferenceRequest{
		Channel: &tg.InputChannel{ChannelID: dialog.NativeID, AccessHash: dialog.AccessHash},
		Filter:  &tg.ChannelMessagesFilterEmpty{}, Pts: pts, Limit: 100,
	})
	if err != nil {
		return out, fmt.Errorf("get Telegram channel difference: %w", err)
	}
	out.Response = response
	out.ChatID, out.PreviousPTS = expected, pts
	var next int
	switch d := response.(type) {
	case *tg.UpdatesChannelDifferenceEmpty:
		if d == nil {
			return ChannelDifferencePage{}, errors.New("invalid Telegram channel difference")
		}
		next, out.More = d.Pts, !d.Final
	case *tg.UpdatesChannelDifference:
		if d == nil {
			return ChannelDifferencePage{}, errors.New("invalid Telegram channel difference")
		}
		next, out.More = d.Pts, !d.Final
	case *tg.UpdatesChannelDifferenceTooLong:
		if d == nil {
			return ChannelDifferencePage{}, errors.New("invalid Telegram channel difference")
		}
		peer, ok := d.Dialog.(*tg.Dialog)
		if !ok || peer == nil {
			return ChannelDifferencePage{}, errors.New("missing Telegram recovery dialog")
		}
		marked, err := telegram.MarkedPeerID(peer.Peer)
		if err != nil || marked != expected {
			return ChannelDifferencePage{}, errors.New("unexpected Telegram recovery peer")
		}
		out.TooLong, out.More = true, !d.Final
		if recovery, present := peer.GetPts(); present {
			if recovery < pts || int64(recovery) > math.MaxInt32 {
				return ChannelDifferencePage{}, errors.New("invalid Telegram recovery position")
			}
			out.RecoveryPTS = &recovery
		}
		return out, nil
	default:
		return ChannelDifferencePage{}, errors.New("unexpected Telegram channel difference")
	}
	if next < pts || int64(next) > math.MaxInt32 {
		return ChannelDifferencePage{}, errors.New("invalid Telegram channel checkpoint")
	}
	if out.More && next == pts {
		return ChannelDifferencePage{}, errors.New("Telegram channel difference did not advance")
	}
	out.NextPTS = &next
	return out, nil
}

// CommitChannelDifference applies all effects before persisting the channel pts.
// Too-long pages require the separate recovery path and cannot be committed here.
func CommitChannelDifference(ctx context.Context, page ChannelDifferencePage, chatID int64, pts *int,
	apply func(context.Context, tg.UpdatesChannelDifferenceClass) error,
	save func(context.Context, int) error,
) error {
	if pts == nil || apply == nil || save == nil || page.Response == nil || page.NextPTS == nil || page.TooLong || chatID == 0 || chatID != page.ChatID || *pts < 0 || *pts != page.PreviousPTS || *page.NextPTS < *pts || int64(*page.NextPTS) > math.MaxInt32 {
		return errors.New("invalid or stale Telegram channel checkpoint")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := apply(ctx, page.Response); err != nil {
		return fmt.Errorf("apply Telegram channel difference: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := save(ctx, *page.NextPTS); err != nil {
		return fmt.Errorf("save Telegram channel checkpoint: %w", err)
	}
	*pts = *page.NextPTS
	return nil
}
