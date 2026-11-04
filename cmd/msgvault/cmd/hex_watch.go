package cmd

import (
	"context"
	"go.kenn.io/msgvault/internal/config"
	"time"
)

type hexWatchSource = meetingWatchSource[config.HexSource]
type hexWatchStopError = meetingWatchStopError

func hexWatchSources(s []config.HexSource) ([]hexWatchSource, error) {
	return meetingWatchSources(s, func(s config.HexSource) bool { return s.Enabled }, func(s config.HexSource) string { return s.Schedule })
}
func runHexWatch(ctx context.Context, s []hexWatchSource, scan func(context.Context, config.HexSource) error, wait func(context.Context, time.Time) error, report func(error)) error {
	return runMeetingWatch(ctx, s, scan, wait, report)
}
func waitHexSchedule(ctx context.Context, next time.Time) error { return waitMuesliSchedule(ctx, next) }
