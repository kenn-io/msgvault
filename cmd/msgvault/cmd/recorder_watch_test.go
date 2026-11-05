package cmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestRecorderWatchSchedulesEnabledSourcesAndRescans(t *testing.T) {
	t.Run("Hex", func(t *testing.T) {
		testRecorderWatch(t, func(id string, enabled bool, schedule string) config.HexSource {
			return config.HexSource{Identifier: id, Enabled: enabled, Schedule: schedule}
		}, func(s config.HexSource) string { return s.Identifier }, func(s config.HexSource) bool { return s.Enabled }, func(s config.HexSource) string { return s.Schedule })
	})
	t.Run("Muesli", func(t *testing.T) {
		testRecorderWatch(t, func(id string, enabled bool, schedule string) config.MuesliSource {
			return config.MuesliSource{Identifier: id, Enabled: enabled, Schedule: schedule}
		}, func(s config.MuesliSource) string { return s.Identifier }, func(s config.MuesliSource) bool { return s.Enabled }, func(s config.MuesliSource) string { return s.Schedule })
	})
}
func testRecorderWatch[S any](t *testing.T, source func(string, bool, string) S, identifier func(S) string, enabled func(S) bool, schedule func(S) string) {
	t.Helper()
	selected, err := meetingWatchSources([]S{source("enabled", true, "* * * * *"), source("disabled", false, "* * * * *")}, enabled, schedule)
	require.NoError(t, err)
	require.Len(t, selected, 1)
	assert.Equal(t, "enabled", identifier(selected[0].source))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	err = runMeetingWatch(ctx, selected, func(context.Context, S) error {
		calls++
		if calls == 2 {
			cancel()
		}
		return errors.New("synthetic temporary transport failure")
	}, func(context.Context, time.Time) error { return nil }, func(error) {})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 2, calls)
	for _, sources := range [][]S{nil, {source("no-schedule", true, "")}, {source("invalid", true, "invalid")}} {
		_, err := meetingWatchSources(sources, enabled, schedule)
		require.Error(t, err)
	}
	_, err = meetingWatchSources([]S{source("impossible", true, "0 0 31 2 *")}, enabled, schedule)
	require.ErrorContains(t, err, "future transition")
}
