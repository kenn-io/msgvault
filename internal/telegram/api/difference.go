package api

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/gotd/td/tg"
	"go.kenn.io/msgvault/internal/telegram"
)

// CommonDifferencePage retains every Telegram effect until a caller has
// applied it. Next is a proposed checkpoint, not a persisted position.
type CommonDifferencePage struct {
	Response    tg.UpdatesDifferenceClass
	Previous    telegram.CommonUpdateState
	Next        *telegram.CommonUpdateState
	More        bool
	TooLong     bool
	RecoveryPTS int
}

// GetCommonDifferencePage reads one common-box difference without mutating
// durable state. A too-long response requires a recorded gap and rescan before
// its recovery position can be committed.
func GetCommonDifferencePage(ctx context.Context, client *tg.Client, current telegram.CommonUpdateState) (CommonDifferencePage, error) {
	var out CommonDifferencePage
	if client == nil || !validCommonState(current) {
		return out, errors.New("invalid Telegram common update state")
	}
	response, err := client.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: current.Pts, Qts: current.Qts, Date: current.Date})
	if err != nil {
		return out, fmt.Errorf("get Telegram common difference: %w", err)
	}
	out.Response = response
	out.Previous = current
	var next telegram.CommonUpdateState
	switch d := response.(type) {
	case *tg.UpdatesDifferenceEmpty:
		if d == nil {
			return CommonDifferencePage{}, errors.New("invalid Telegram difference")
		}
		next = current
		next.Date, next.Seq = d.Date, d.Seq
	case *tg.UpdatesDifference:
		if d == nil {
			return CommonDifferencePage{}, errors.New("invalid Telegram difference")
		}
		next = commonState(d.State)
	case *tg.UpdatesDifferenceSlice:
		if d == nil {
			return CommonDifferencePage{}, errors.New("invalid Telegram difference")
		}
		next = commonState(d.IntermediateState)
		out.More = true
	case *tg.UpdatesDifferenceTooLong:
		if d == nil || d.Pts < current.Pts || int64(d.Pts) > math.MaxInt32 {
			return CommonDifferencePage{}, errors.New("invalid Telegram recovery position")
		}
		out.TooLong, out.RecoveryPTS = true, d.Pts
		return out, nil
	default:
		return CommonDifferencePage{}, errors.New("unexpected Telegram common difference")
	}
	if !validCommonState(next) || next.Pts < current.Pts || next.Qts < current.Qts {
		return CommonDifferencePage{}, errors.New("invalid Telegram difference checkpoint")
	}
	if out.More && next == current {
		return CommonDifferencePage{}, errors.New("Telegram difference slice did not advance")
	}
	out.Next = &next
	return out, nil
}

func commonState(s tg.UpdatesState) telegram.CommonUpdateState {
	return telegram.CommonUpdateState{Pts: s.Pts, Qts: s.Qts, Date: s.Date, Seq: s.Seq}
}

func validCommonState(s telegram.CommonUpdateState) bool {
	return s.Pts >= 0 && int64(s.Pts) <= math.MaxInt32 && s.Qts >= 0 && int64(s.Qts) <= math.MaxInt32 && s.Date > 0 && int64(s.Date) <= math.MaxInt32 && s.Seq >= 0 && int64(s.Seq) <= math.MaxInt32
}

// CommitCommonDifference applies all effects before saving a proposed position.
// apply must be replay-safe and process every relevant account-wide dialog.
// The caller records gaps and rescans separately for too-long responses.
func CommitCommonDifference(ctx context.Context, page CommonDifferencePage, current *telegram.CommonUpdateState,
	apply func(context.Context, tg.UpdatesDifferenceClass) error,
	save func(context.Context, telegram.CommonUpdateState) error,
) error {
	if current == nil || apply == nil || save == nil || page.Response == nil || page.Next == nil || page.TooLong || *current != page.Previous || !validCommonState(*page.Next) || page.Next.Pts < current.Pts || page.Next.Qts < current.Qts {
		return errors.New("invalid or stale Telegram difference checkpoint")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := apply(ctx, page.Response); err != nil {
		return fmt.Errorf("apply Telegram difference: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := save(ctx, *page.Next); err != nil {
		return fmt.Errorf("save Telegram difference checkpoint: %w", err)
	}
	*current = *page.Next
	return nil
}
