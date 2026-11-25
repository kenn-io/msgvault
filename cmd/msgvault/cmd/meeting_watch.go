package cmd

import (
	"context"
	"errors"
	"go.kenn.io/msgvault/internal/cronexpr"
	"time"

	"github.com/netresearch/go-cron"
)

type meetingWatchSource[S any] struct {
	source   S
	schedule cron.Schedule
	next     time.Time
}

func meetingWatchSources[S any](sources []S, enabled func(S) bool, expression func(S) string) ([]meetingWatchSource[S], error) {
	var selected []meetingWatchSource[S]
	for _, source := range sources {
		if !enabled(source) || expression(source) == "" {
			continue
		}
		schedule, err := cronexpr.Parser.Parse(expression(source))
		if err != nil {
			return nil, errors.New("invalid meeting rescan schedule")
		}
		if schedule.Next(time.Now()).IsZero() {
			return nil, errors.New("meeting rescan schedule has no future transition")
		}
		selected = append(selected, meetingWatchSource[S]{source: source, schedule: schedule})
	}
	if len(selected) == 0 {
		return nil, errors.New("--watch requires an enabled meeting source with a schedule")
	}
	return selected, nil
}

// meetingWatchStopError marks a scan failure that rescanning cannot fix, such
// as an incompatible daemon or rejected credentials, so the watcher exits.
type meetingWatchStopError struct{ err error }

func (e meetingWatchStopError) Error() string { return e.err.Error() }
func (e meetingWatchStopError) Unwrap() error { return e.err }

// runMeetingWatch[S any] performs one initial pass and serial cron rescans. Transport
// failures wait for the next scan; a meetingWatchStopError ends the watch; a
// cancelled process releases its source lock.
func runMeetingWatch[S any](ctx context.Context, sources []meetingWatchSource[S], scan func(context.Context, S) error, wait func(context.Context, time.Time) error, report func(error)) error {
	for _, source := range sources {
		if source.schedule.Next(time.Now()).IsZero() {
			return errors.New("meeting rescan schedule has no future transition")
		}
	}
	for i := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := scan(ctx, sources[i].source)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if stop, ok := errors.AsType[meetingWatchStopError](err); ok {
			return stop.err
		}
		if err != nil {
			report(err)
		}
		sources[i].next = sources[i].schedule.Next(time.Now())
		if sources[i].next.IsZero() {
			return errors.New("meeting rescan schedule has no future transition")
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		next := sources[0].next
		for _, source := range sources {
			if source.next.Before(next) {
				next = source.next
			}
		}
		if err := wait(ctx, next); err != nil {
			return err
		}
		for i := range sources {
			if !sources[i].next.Equal(next) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			err := scan(ctx, sources[i].source)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if stop, ok := errors.AsType[meetingWatchStopError](err); ok {
				return stop.err
			}
			if err != nil {
				report(err)
			}
			from := next
			if now := time.Now(); now.After(from) {
				from = now
			}
			sources[i].next = sources[i].schedule.Next(from)
			if sources[i].next.IsZero() {
				return errors.New("meeting rescan schedule has no future transition")
			}
		}
	}
}

func waitMeetingSchedule(ctx context.Context, next time.Time) error {
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
