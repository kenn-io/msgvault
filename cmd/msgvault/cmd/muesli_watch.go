package cmd

import (
	"context"
	"errors"
	"time"

	"github.com/robfig/cron/v3"
	"go.kenn.io/msgvault/internal/config"
)

type muesliWatchSource struct {
	source   config.MuesliSource
	schedule cron.Schedule
	next     time.Time
}

func muesliWatchSources(sources []config.MuesliSource) ([]muesliWatchSource, error) {
	var selected []muesliWatchSource
	for _, source := range sources {
		if !source.Enabled || source.Schedule == "" {
			continue
		}
		schedule, err := cron.ParseStandard(source.Schedule)
		if err != nil {
			return nil, errors.New("invalid Muesli rescan schedule")
		}
		if schedule.Next(time.Now()).IsZero() {
			return nil, errors.New("muesli rescan schedule has no future transition")
		}
		selected = append(selected, muesliWatchSource{source: source, schedule: schedule})
	}
	if len(selected) == 0 {
		return nil, errors.New("--watch requires an enabled Muesli source with a schedule")
	}
	return selected, nil
}

// muesliWatchStopError marks a scan failure that rescanning cannot fix, such
// as an incompatible daemon or rejected credentials, so the watcher exits.
type muesliWatchStopError struct{ err error }

func (e muesliWatchStopError) Error() string { return e.err.Error() }
func (e muesliWatchStopError) Unwrap() error { return e.err }

// runMuesliWatch performs one initial pass and serial cron rescans. Transport
// failures wait for the next scan; a muesliWatchStopError ends the watch; a
// cancelled process releases its source lock.
func runMuesliWatch(ctx context.Context, sources []muesliWatchSource, scan func(context.Context, config.MuesliSource) error, wait func(context.Context, time.Time) error, report func(error)) error {
	for _, source := range sources {
		if source.schedule.Next(time.Now()).IsZero() {
			return errors.New("muesli rescan schedule has no future transition")
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
		if stop, ok := errors.AsType[muesliWatchStopError](err); ok {
			return stop.err
		}
		if err != nil {
			report(err)
		}
		sources[i].next = sources[i].schedule.Next(time.Now())
		if sources[i].next.IsZero() {
			return errors.New("muesli rescan schedule has no future transition")
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
			if stop, ok := errors.AsType[muesliWatchStopError](err); ok {
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
				return errors.New("muesli rescan schedule has no future transition")
			}
		}
	}
}
func waitMuesliSchedule(ctx context.Context, next time.Time) error {
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
