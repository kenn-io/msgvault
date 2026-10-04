package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calcontrol"
	"go.kenn.io/msgvault/internal/gcal"
)

func TestCalendarControlCLIParsesSharedTargetAndDryRun(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	var got calcontrol.Request
	cmd := newCalendarControlCmd(func(_ context.Context, r calcontrol.Request) (*calcontrol.Result, error) {
		got = r
		return &calcontrol.Result{DryRun: true}, nil
	})
	cmd.SetArgs([]string{"create", "team@example.com", "--account", "person@example.com", "--summary", "Planning", "--from", "2026-10-02T09:00:00", "--to", "2026-10-02T10:00:00", "--tz", "Europe/London", "--attendees", "guest@example.com", "--rrule", "RRULE:FREQ=WEEKLY;COUNT=4", "--reminder", "popup:15", "--dry-run", "--json"})
	requirements.NoError(cmd.Execute())
	assertions.Equal("person@example.com", got.Account)
	assertions.Equal("team@example.com", got.CalendarID)
	assertions.Equal("none", got.SendUpdates)
	assertions.True(got.DryRun)
	requirements.NotNil(got.Event.Start)
	assertions.Equal("Europe/London", got.Event.Start.TimeZone)
	requirements.NotNil(got.Event.Attendees)
	assertions.Equal("guest@example.com", (*got.Event.Attendees)[0].Email)
	requirements.NotNil(got.Event.Reminders)
	assertions.Equal(15, got.Event.Reminders.Overrides[0].Minutes)
}
func TestCalendarControlCLIAllDayAndPartialPatch(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	var got calcontrol.Request
	run := func(_ context.Context, r calcontrol.Request) (*calcontrol.Result, error) {
		got = r
		return &calcontrol.Result{}, nil
	}
	cmd := newCalendarControlCmd(run)
	cmd.SetArgs([]string{"create", "primary", "--account", "person@example.com", "--summary", "Day off", "--from", "2026-10-02", "--to", "2026-10-03", "--all-day"})
	requirements.NoError(cmd.Execute())
	assertions.Equal("2026-10-02", got.Event.Start.Date)
	cmd = newCalendarControlCmd(run)
	cmd.SetArgs([]string{"update", "team", "event", "--account", "person@example.com", "--summary", "", "--add-attendee", "new@example.com"})
	requirements.NoError(cmd.Execute())
	requirements.NotNil(got.Event.Summary)
	assertions.Empty(*got.Event.Summary)
	assertions.Nil(got.Event.Start)
	assertions.Nil(got.Event.End)
	assertions.Equal([]string{"new@example.com"}, got.AddAttendees)
}
func TestCalendarControlCLIRejectsInvalidInputBeforeBackend(t *testing.T) {
	for _, flags := range [][]string{{"--from", "invalid", "--to", "2026-10-02T10:00:00Z"}, {"--from", "2026-10-02T09:00:00Z", "--to", "2026-10-02T08:00:00Z"}, {"--from", "2026-10-02T09:00:00Z", "--to", "2026-10-02T10:00:00Z", "--send-updates", "invalid"}} {
		calls := 0
		cmd := newCalendarControlCmd(func(context.Context, calcontrol.Request) (*calcontrol.Result, error) {
			calls++
			return &calcontrol.Result{}, nil
		})
		cmd.SetArgs(append([]string{"create", "team", "--account", "person@example.com", "--summary", "Planning"}, flags...))
		require.Error(t, cmd.Execute())
		assert.Zero(t, calls)
	}
}

func TestCalendarControlCLIMovePositionalDestination(t *testing.T) {
	var got calcontrol.Request
	cmd := newCalendarControlCmd(func(_ context.Context, r calcontrol.Request) (*calcontrol.Result, error) {
		got = r
		return &calcontrol.Result{}, nil
	})
	cmd.SetArgs([]string{"move", "team", "event", "other", "--account", "person@example.com"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, "other", got.Destination)
}

func TestCalendarControlCLIScopeHelpMatchesSupportedActions(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	root := newCalendarControlCmd(nil)
	respond, _, err := root.Find([]string{"respond"})
	requirements.NoError(err)
	respondScope := respond.Flags().Lookup("scope")
	requirements.NotNil(respondScope)
	assertions.NotContains(respondScope.Usage, "future")

	update, _, err := root.Find([]string{"update"})
	requirements.NoError(err)
	updateScope := update.Flags().Lookup("scope")
	requirements.NotNil(updateScope)
	assertions.Contains(updateScope.Usage, "future")
}

func TestCalendarControlCLIReportsArchiveFailureWithoutMessageID(t *testing.T) {
	var output strings.Builder
	cmd := newCalendarControlCmd(func(context.Context, calcontrol.Request) (*calcontrol.Result, error) {
		return &calcontrol.Result{Writes: []calcontrol.WriteReceipt{{
			Action: "update", CalendarID: "team", Event: gcal.Event{ID: "event"}, ArchiveError: "archive store unavailable",
		}}}, nil
	})
	cmd.SetArgs([]string{"update", "team", "event", "--account", "person@example.com", "--summary", "Planning"})
	cmd.SetOut(&output)

	err := cmd.Execute()

	require.ErrorContains(t, err, "archive write failed: archive store unavailable")
	assert.Contains(t, output.String(), "(not archived: archive store unavailable)")
	assert.NotContains(t, output.String(), "archive message 0")
}
