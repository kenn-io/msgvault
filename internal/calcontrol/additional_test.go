package calcontrol

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/gcal"
)

func TestWriterRoleAndExactCalendarGrant(t *testing.T) {
	s, f, r := fixture(t)
	f.Calendars[0].AccessRole = "writer"
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite}, Sources: []agentgrant.SourceRef{{Type: "gcal", Identifier: "person@example.com/team@example.com"}}}
	_, err := s.Execute(context.Background(), r, grant)
	require.NoError(t, err)
	assert.Equal(t, "team@example.com", f.calendar)
}
func TestMoveVerifiesBothCalendarsAndArchivesBothSides(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	r.Action = "move"
	r.EventID = "event"
	r.Destination = "other@example.com"
	r.Event = gcal.EventInput{}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"event": {ID: "event", Summary: "Original"}}
	result, err := s.Execute(context.Background(), r, nil)
	requirements.NoError(err)
	requirements.Len(result.Writes, 2)
	assertions.Equal("team@example.com", result.Writes[0].CalendarID)
	assertions.Equal("cancelled", result.Writes[0].Event.Status)
	assertions.Equal("other@example.com", result.Writes[1].CalendarID)
	f.calls = nil
	f.Calendars[2].AccessRole = "reader"
	_, err = s.Execute(context.Background(), r, nil)
	requirements.Error(err)
	assertions.Empty(f.calls)
}
func TestAvailabilityRequiresIndependentCalendarReadGrant(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	r.Action = "freebusy"
	r.Event = gcal.EventInput{}
	r.TimeMin = time.Now()
	r.TimeMax = r.TimeMin.Add(time.Hour)
	f.busy = gcal.FreeBusyResponse{Calendars: map[string]gcal.CalendarBusy{"team@example.com": {Busy: []gcal.BusyPeriod{}}}}
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite}, Sources: []agentgrant.SourceRef{{Type: "gcal", Identifier: "person@example.com/team@example.com"}}}
	_, err := s.Execute(context.Background(), r, grant)
	requirements.Error(err)
	assertions.Empty(f.calls)
	grant.Permissions = []agentgrant.Permission{agentgrant.PermissionCalendarRead}
	_, err = s.Execute(context.Background(), r, grant)
	requirements.NoError(err)
	assertions.Equal([]string{"freebusy"}, f.calls)
}

type failingInsert struct{ *controlFake }

func (f failingInsert) InsertEvent(context.Context, string, gcal.EventInput, gcal.MutationOptions) (*gcal.Event, error) {
	return nil, errors.New("provider unavailable")
}

type unknownOutcomeInsert struct{ *controlFake }

func (f unknownOutcomeInsert) InsertEvent(context.Context, string, gcal.EventInput, gcal.MutationOptions) (*gcal.Event, error) {
	f.calls = append(f.calls, "insert")
	return nil, fmt.Errorf("%w: provider response was lost", gcal.ErrOutcomeUnknown)
}

type missingEventInsert struct{ *controlFake }

func (f missingEventInsert) InsertEvent(context.Context, string, gcal.EventInput, gcal.MutationOptions) (*gcal.Event, error) {
	f.calls = append(f.calls, "insert")
	return &gcal.Event{}, nil
}

func TestFuturePartialFailureReturnsCompletedWrite(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	start, end := *r.Event.Start, *r.Event.End
	r.Action = "update"
	r.EventID = "series"
	r.Scope = "future"
	r.OriginalStart = "2026-10-03T09:00:00Z"
	r.Event = gcal.EventInput{Summary: r.Event.Summary}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {ID: "series", Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"}}}
	f.instances = []gcal.Event{{ID: "instance", RecurringEventID: "series", OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)}, Start: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)}, End: gcal.EventDateTime{DateTime: end.DateTime.Add(24 * time.Hour)}}}
	s.Client = failingInsert{f}
	result, err := s.Execute(context.Background(), r, nil)
	requirements.NoError(err)
	requirements.Len(result.Writes, 1)
	assertions.True(result.Writes[0].Archived)
	assertions.Contains(result.Error, "after completed writes")
	assertions.NotContains(result.Error, "retry")
	assertions.Equal("calendar_partial", result.OutcomeCode)
	assertions.Equal([]string{"patch:series"}, f.calls)
}

func TestFuturePartialUnknownOutcomeIsExplicitAndKeepsCompletedReceipt(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	start, end := *r.Event.Start, *r.Event.End
	r.Action = "update"
	r.EventID = "series"
	r.Scope = "future"
	r.OriginalStart = "2026-10-03T09:00:00Z"
	r.Event = gcal.EventInput{Summary: r.Event.Summary}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {
		ID: "series", Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"},
	}}
	f.instances = []gcal.Event{{
		ID: "instance", RecurringEventID: "series",
		OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)},
		Start:             gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)},
		End:               gcal.EventDateTime{DateTime: end.DateTime.Add(24 * time.Hour)},
	}}
	s.Client = unknownOutcomeInsert{f}

	result, err := s.Execute(context.Background(), r, nil)
	requirements.NoError(err)
	requirements.Len(result.Writes, 1)
	assertions.True(result.Writes[0].Archived)
	assertions.Contains(result.Error, "reconcile the calendar and receipts")
	assertions.NotContains(result.Error, "retry")
	assertions.Equal("calendar_outcome_unknown", result.OutcomeCode)
	assertions.Equal([]string{"patch:series", "insert"}, f.calls)

	payload, err := json.Marshal(result)
	requirements.NoError(err)
	var response map[string]any
	requirements.NoError(json.Unmarshal(payload, &response))
	assertions.Equal(true, response["outcome_unknown"])
}

func TestFuturePartialMissingEventMarksOutcomeUnknownAndKeepsCompletedReceipt(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	start, end := *r.Event.Start, *r.Event.End
	r.Action = "update"
	r.EventID = "series"
	r.Scope = "future"
	r.OriginalStart = "2026-10-03T09:00:00Z"
	r.Event = gcal.EventInput{Summary: r.Event.Summary}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {
		ID: "series", Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"},
	}}
	f.instances = []gcal.Event{{
		ID: "instance", RecurringEventID: "series",
		OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)},
		Start:             gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)},
		End:               gcal.EventDateTime{DateTime: end.DateTime.Add(24 * time.Hour)},
	}}
	s.Client = missingEventInsert{f}

	result, err := s.Execute(context.Background(), r, nil)

	requirements.NoError(err)
	requirements.Len(result.Writes, 1)
	assertions.True(result.Writes[0].Archived)
	assertions.True(result.OutcomeUnknown)
	assertions.Contains(result.Error, "outcome unknown")
	assertions.Contains(result.Error, "reconcile the calendar and receipts")
	assertions.NotContains(result.Error, "retry")
	assertions.Equal("calendar_outcome_unknown", result.OutcomeCode)
	assertions.Equal([]string{"patch:series", "insert"}, f.calls)
}

func TestExpectedPlanFingerprintRejectsChangedFuturePlanBeforeWrites(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	start, end := *r.Event.Start, *r.Event.End
	r.Action = "update"
	r.EventID = "series"
	r.Scope = "future"
	r.OriginalStart = "2026-10-03T09:00:00Z"
	r.Event = gcal.EventInput{Summary: r.Event.Summary}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {
		ID: "series", Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"},
	}}
	f.instances = []gcal.Event{{
		ID: "instance", RecurringEventID: "series",
		OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)},
		Start:             gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)},
		End:               gcal.EventDateTime{DateTime: end.DateTime.Add(24 * time.Hour)},
	}}
	r.DryRun = true
	preview, err := s.Execute(context.Background(), r, nil)
	requirements.NoError(err)
	requirements.NotEmpty(preview.PlanFingerprint)

	r.DryRun = false
	r.ExpectedPlanFingerprint = preview.PlanFingerprint
	changedMaster := f.EventsByID["team@example.com"]["series"]
	changedMaster.Recurrence = []string{"RRULE:FREQ=DAILY;COUNT=5"}
	f.EventsByID["team@example.com"]["series"] = changedMaster
	result, err := s.Execute(context.Background(), r, nil)

	requirements.ErrorIs(err, ErrPlanChanged)
	assertions.Nil(result)
	assertions.NotContains(f.calls, "patch:series")
	assertions.NotContains(f.calls, "insert")
}

func TestExpectedPlanFingerprintBindsNotificationMode(t *testing.T) {
	for _, tc := range []struct {
		name, previewMode, writeMode string
		changed                      bool
	}{
		{name: "default matches explicit none", writeMode: "none"},
		{name: "explicit none matches default", previewMode: "none"},
		{name: "unchanged all", previewMode: "all", writeMode: "all"},
		{name: "enable notifications", writeMode: "all", changed: true},
		{name: "change notification audience", previewMode: "all", writeMode: "externalOnly", changed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, f, r := fixture(t)
			r.Event.Attendees = new([]gcal.Attendee{{Email: "guest@example.com"}})
			r.SendUpdates, r.DryRun = tc.previewMode, true
			preview, err := s.Execute(t.Context(), r, nil)
			requirements.NoError(err)
			requirements.NotEmpty(preview.PlanFingerprint)
			assertions.Empty(f.calls)

			r.SendUpdates, r.DryRun = tc.writeMode, false
			r.ExpectedPlanFingerprint = preview.PlanFingerprint
			result, err := s.Execute(t.Context(), r, nil)
			if tc.changed {
				requirements.ErrorIs(err, ErrPlanChanged)
				assertions.Nil(result)
				assertions.Empty(f.calls)
				return
			}
			requirements.NoError(err)
			assertions.Equal([]string{"insert"}, f.calls)
			assertions.Equal(preview.SendUpdates, f.options.SendUpdates)
			requirements.Len(result.Writes, 1)
			assertions.True(result.Writes[0].Archived)
		})
	}
}

func TestExpectedPlanFingerprintBindsNormalizedOAuthAccount(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	summary := "Updated"
	r.Action, r.EventID = actionUpdate, "event"
	r.Event = gcal.EventInput{Summary: &summary}
	r.DryRun = true
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"event": {ID: "event", Summary: "Original"}}

	preview, err := s.Execute(context.Background(), r, nil)
	requirements.NoError(err)
	requirements.NotEmpty(preview.PlanFingerprint)
	assertions.Empty(f.calls)

	otherAccountService := *s
	otherAccountService.Source.Email = "alternate@example.com"
	r.Account = otherAccountService.Source.Email
	r.DryRun = false
	r.ExpectedPlanFingerprint = preview.PlanFingerprint

	_, err = otherAccountService.Execute(context.Background(), r, nil)

	requirements.ErrorIs(err, ErrPlanChanged)
	assertions.Empty(f.calls)
}

func TestFutureDetachedExceptionsAreRejectedBeforeWrite(t *testing.T) {
	s, f, r := fixture(t)
	start := *r.Event.Start
	r.Action = "delete"
	r.EventID = "series"
	r.Scope = "future"
	r.OriginalStart = "2026-10-03T09:00:00Z"
	r.Event = gcal.EventInput{}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {ID: "series", Start: start, Recurrence: []string{"RRULE:FREQ=DAILY"}}}
	f.FullEvents["team@example.com"] = [][]gcal.Event{{{ID: "exception", RecurringEventID: "series", OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime.Add(48 * time.Hour)}}}}
	_, err := s.Execute(context.Background(), r, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "detached exceptions")
	assert.Empty(t, f.calls)
}
