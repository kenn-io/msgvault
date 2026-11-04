package cmd

import (
	"context"
	"go.kenn.io/msgvault/internal/config"
	"time"
)

type muesliWatchSource = meetingWatchSource[config.MuesliSource]
type muesliWatchStopError = meetingWatchStopError

func muesliWatchSources(s []config.MuesliSource) ([]muesliWatchSource, error) {
	return meetingWatchSources(s, func(s config.MuesliSource) bool { return s.Enabled }, func(s config.MuesliSource) string { return s.Schedule })
}
func runMuesliWatch(ctx context.Context, s []muesliWatchSource, scan func(context.Context, config.MuesliSource) error, wait func(context.Context, time.Time) error, report func(error)) error {
	return runMeetingWatch(ctx, s, scan, wait, report)
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
