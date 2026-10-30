package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

type takeoutInvoker struct {
	next tg.Invoker
	id   int64
}

func (i takeoutInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	switch request := input.(type) {
	case *tg.MessagesGetDialogsRequest, *tg.MessagesGetHistoryRequest, *tg.MessagesGetSplitRangesRequest,
		*tg.AccountFinishTakeoutSessionRequest, *tg.InvokeWithMessagesRangeRequest:
		return i.next.Invoke(ctx, &tg.InvokeWithTakeoutRequest{TakeoutID: i.id, Query: request.(bin.Object)}, output)
	}
	return i.next.Invoke(ctx, input, output)
}

type messagesRangeInvoker struct {
	next   tg.Invoker
	range_ tg.MessageRange
}

func (i messagesRangeInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	switch request := input.(type) {
	case *tg.MessagesGetDialogsRequest, *tg.MessagesGetHistoryRequest:
		return i.next.Invoke(ctx, &tg.InvokeWithMessagesRangeRequest{Range: i.range_, Query: request.(bin.Object)}, output)
	}
	return i.next.Invoke(ctx, input, output)
}

// startTakeout wraps the takeout API requests in the explicitly requested session.
func startTakeout(ctx context.Context, client *tg.Client) (*tg.Client, error) {
	if client == nil {
		return nil, errors.New("invalid Telegram takeout client")
	}
	session, err := client.AccountInitTakeoutSession(ctx, &tg.AccountInitTakeoutSessionRequest{MessageUsers: true, MessageChats: true, MessageMegagroups: true, MessageChannels: true})
	if err != nil {
		if delay, ok := tgerr.AsType(err, "TAKEOUT_INIT_DELAY"); ok && delay.Argument >= 0 && int64(delay.Argument) <= math.MaxInt64/int64(time.Second) {
			return nil, fmt.Errorf("Telegram takeout requires approval and a wait of %s", time.Duration(delay.Argument)*time.Second)
		}
		return nil, fmt.Errorf("start Telegram takeout: %w", err)
	}
	if session == nil || session.ID == 0 {
		return nil, errors.New("Telegram returned an invalid takeout session")
	}
	return tg.NewClient(takeoutInvoker{next: client.Invoker(), id: session.ID}), nil
}

// WithTakeout always attempts to finish an initialized session, including
// after cancellation. Telegram may require approval on another device.
func WithTakeout(ctx context.Context, client *tg.Client, work func(context.Context, *tg.Client) error) (retErr error) {
	if work == nil {
		return errors.New("invalid Telegram takeout work")
	}
	wrapped, err := startTakeout(ctx, client)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		finished, err := wrapped.AccountFinishTakeoutSession(cleanup, &tg.AccountFinishTakeoutSessionRequest{Success: retErr == nil})
		if err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("finish Telegram takeout: %w", err))
		} else if !finished {
			retErr = errors.Join(retErr, errors.New("finish Telegram takeout was not confirmed"))
		}
	}()
	return work(ctx, wrapped)
}
