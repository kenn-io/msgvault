package calcontrol

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/gcal"
)

func TestFutureSeriesSplitAndDelete(t *testing.T) {
	for _, action := range []string{"update", "delete"} {
		t.Run(action, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, f, r := fixture(t)
			r.Action = action
			r.EventID = "series"
			r.Scope = "future"
			r.OriginalStart = "2026-10-03T09:00:00Z"
			start, end := *r.Event.Start, *r.Event.End
			if action == "update" {
				r.Event = gcal.EventInput{Summary: r.Event.Summary}
			} else {
				r.Event = gcal.EventInput{}
			}
			f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {ID: "series", ETag: `"v1"`, Summary: "Original", Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"}}}
			f.instances = []gcal.Event{{ID: "instance", RecurringEventID: "series", OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)}, Start: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)}, End: gcal.EventDateTime{DateTime: end.DateTime.Add(24 * time.Hour)}}}
			result, err := s.Execute(context.Background(), r, nil)
			requirements.NoError(err)
			requirements.NotEmpty(result.Plan)
			assertions.Equal("update", result.Plan[0].Action)
			requirements.NotNil(result.Plan[0].Event.Recurrence)
			assertions.Contains((*result.Plan[0].Event.Recurrence)[0], "UNTIL=20261003T085959Z")
			if action == "update" {
				requirements.Len(result.Plan, 2)
				requirements.NotNil(result.Plan[1].Event.Start)
				assertions.Equal(f.instances[0].Start.DateTime, result.Plan[1].Event.Start.DateTime)
				requirements.NotNil(result.Plan[1].Event.Recurrence)
				assertions.Contains((*result.Plan[1].Event.Recurrence)[0], "COUNT=3")
				assertions.Equal([]string{"patch:series", "insert"}, f.calls)
			} else {
				requirements.Len(result.Plan, 1)
				assertions.Equal([]string{"patch:series"}, f.calls)
			}
		})
	}
}

func TestFutureSplitRejectsOverlappingRetainedOccurrences(t *testing.T) {
	for _, tc := range []struct {
		name      string
		original  string
		start     string
		end       string
		allDay    bool
		wantError bool
	}{
		{"duplicate occurrence", "2026-10-03T09:00:00-04:00", "2026-10-02T09:00:00-04:00", "2026-10-02T10:00:00-04:00", false, true},
		{"later replacement overlaps retained occurrence", "2026-10-03T09:00:00-04:00", "2026-10-01T09:00:00-04:00", "2026-10-01T10:00:00-04:00", false, true},
		{"inside final retained occurrence", "2026-10-04T09:00:00-04:00", "2026-10-03T09:30:00-04:00", "2026-10-03T10:30:00-04:00", false, true},
		{"at final retained end", "2026-10-04T09:00:00-04:00", "2026-10-03T14:00:00Z", "2026-10-03T15:00:00Z", false, false},
		{"earlier without overlap", "2026-10-03T09:00:00-04:00", "2026-10-03T08:00:00-04:00", "2026-10-03T09:00:00-04:00", false, false},
		{"earlier between retained occurrences", "2026-10-04T09:00:00-04:00", "2026-10-03T08:00:00-04:00", "2026-10-03T09:00:00-04:00", false, false},
		{"first occurrence has no retained series", "2026-10-02T09:00:00-04:00", "2026-10-01T09:00:00-04:00", "2026-10-01T10:00:00-04:00", false, false},
		{"inside retained all-day occurrence", "2026-10-05", "2026-10-03", "2026-10-04", true, true},
		{"at retained all-day end", "2026-10-05", "2026-10-04", "2026-10-05", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, f, r := fixture(t)
			f.Calendars[0].TimeZone = "America/New_York"
			start, end := gcal.EventDateTime{TimeZone: "America/New_York"}, gcal.EventDateTime{TimeZone: "America/New_York"}
			selected, selectedEnd, changedStart, changedEnd := start, end, start, end
			rule := "RRULE:FREQ=DAILY;COUNT=4"
			if tc.allDay {
				start.Date, end.Date = "2026-10-02", "2026-10-04"
				selected.Date, changedStart.Date, changedEnd.Date = tc.original, tc.start, tc.end
				selectedEnd.Date = "2026-10-07"
				rule = "RRULE:FREQ=DAILY;INTERVAL=3;COUNT=4"
			} else {
				var err error
				start.DateTime, err = time.Parse(time.RFC3339, "2026-10-02T09:00:00-04:00")
				requirements.NoError(err)
				end.DateTime = start.DateTime.Add(time.Hour)
				selected.DateTime, err = time.Parse(time.RFC3339, tc.original)
				requirements.NoError(err)
				selectedEnd.DateTime = selected.DateTime.Add(time.Hour)
				changedStart.DateTime, err = time.Parse(time.RFC3339, tc.start)
				requirements.NoError(err)
				changedEnd.DateTime, err = time.Parse(time.RFC3339, tc.end)
				requirements.NoError(err)
			}
			r.Action, r.EventID, r.Scope, r.OriginalStart = "update", "series", "future", tc.original
			r.Event = gcal.EventInput{Start: &changedStart, End: &changedEnd}
			f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {
				ID: "series", ETag: `"v1"`, Start: start, End: end, Recurrence: []string{rule},
			}}
			f.instances = []gcal.Event{{ID: "instance", RecurringEventID: "series", OriginalStartTime: selected, Start: selected, End: selectedEnd}}

			result, err := s.Execute(t.Context(), r, nil)
			if tc.wantError {
				requirements.ErrorIs(err, ErrInvalid)
				assertions.Empty(f.calls, "reject before shortening the original or inserting a replacement")
				return
			}
			requirements.NoError(err)
			wantCalls := []string{"patch:series", "insert"}
			if tc.original == "2026-10-02T09:00:00-04:00" {
				wantCalls = []string{"patch:series"}
			}
			assertions.Equal(wantCalls, f.calls)
			requirements.NotEmpty(result.Plan)
			assertions.Equal(&changedStart, result.Plan[len(result.Plan)-1].Event.Start)
		})
	}
}

func TestFutureSplitChecksRetainedExceptions(t *testing.T) {
	for _, tc := range []struct {
		name         string
		day          int
		hour         int
		minute       int
		exceptionDay int
		summaryOnly  bool
		cancelled    bool
		wantError    bool
	}{
		{name: "overlaps rescheduled earlier occurrence", day: 3, hour: 8, exceptionDay: 3, wantError: true},
		{name: "starts when earlier occurrence ends", day: 3, hour: 8, minute: 30, exceptionDay: 3},
		{name: "cancelled exception has no occupied time", day: 3, hour: 8, exceptionDay: 3, cancelled: true},
		{name: "replacement ends before rescheduled occurrence", day: 3, hour: 8, exceptionDay: 10},
		{name: "cancelled original has no occupied time", day: 2, hour: 9, exceptionDay: 3, cancelled: true},
		{name: "rescheduled original has no occupied time", day: 2, hour: 9, exceptionDay: 10},
		{name: "summary edit preserves existing overlap", day: 3, hour: 8, exceptionDay: 3, summaryOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, f, r := fixture(t)
			start, end := *r.Event.Start, *r.Event.End
			selected := gcal.EventDateTime{DateTime: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC), TimeZone: "UTC"}
			selectedEnd := gcal.EventDateTime{DateTime: selected.DateTime.Add(time.Hour), TimeZone: "UTC"}
			changedStart := gcal.EventDateTime{DateTime: time.Date(2026, 10, tc.day, tc.hour, tc.minute, 0, 0, time.UTC), TimeZone: "UTC"}
			changedEnd := gcal.EventDateTime{DateTime: changedStart.DateTime.Add(time.Hour), TimeZone: "UTC"}
			r.Action, r.EventID, r.Scope, r.OriginalStart = "update", "series", "future", "2026-10-03T09:00:00Z"
			if tc.summaryOnly {
				r.Event = gcal.EventInput{Summary: r.Event.Summary}
			} else {
				r.Event = gcal.EventInput{Start: &changedStart, End: &changedEnd}
			}
			f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {
				ID: "series", ETag: `"v1"`, Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"},
			}}
			f.instances = []gcal.Event{{ID: "instance", RecurringEventID: "series", OriginalStartTime: selected, Start: selected, End: selectedEnd}}
			exception := gcal.Event{
				ID: "earlier", RecurringEventID: "series", OriginalStartTime: start,
				Start: gcal.EventDateTime{DateTime: time.Date(2026, 10, tc.exceptionDay, 7, 30, 0, 0, time.UTC)},
				End:   gcal.EventDateTime{DateTime: time.Date(2026, 10, tc.exceptionDay, 8, 30, 0, 0, time.UTC)},
			}
			if tc.summaryOnly {
				exception.Start.DateTime = selected.DateTime.Add(30 * time.Minute)
				exception.End.DateTime = selected.DateTime.Add(90 * time.Minute)
			}
			if tc.cancelled {
				exception.Status, exception.Start, exception.End = gcal.StatusCancelled, gcal.EventDateTime{}, gcal.EventDateTime{}
			}
			f.FullEvents["team@example.com"] = [][]gcal.Event{{exception}}

			_, err := s.Execute(t.Context(), r, nil)
			if tc.wantError {
				requirements.ErrorIs(err, ErrInvalid)
				assertions.Empty(f.calls)
				return
			}
			requirements.NoError(err)
			assertions.Equal([]string{"patch:series", "insert"}, f.calls)
		})
	}
}

func TestFutureSeriesUsesCalendarTimezoneWhenEventTimezoneIsMissing(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	loc, err := time.LoadLocation("America/New_York")
	requirements.NoError(err)
	start := gcal.EventDateTime{DateTime: time.Date(2024, 3, 8, 9, 0, 0, 0, loc)}
	end := gcal.EventDateTime{DateTime: time.Date(2024, 3, 8, 10, 0, 0, 0, loc)}
	occurrenceStart := time.Date(2024, 3, 11, 9, 0, 0, 0, loc)
	occurrenceEnd := occurrenceStart.Add(time.Hour)
	f.Calendars[0].TimeZone = "America/New_York"
	r.Action, r.EventID, r.Scope = "update", "series", "future"
	r.OriginalStart = "2024-03-11T09:00:00-04:00"
	r.DryRun = true
	summary := "Updated future event"
	r.Event = gcal.EventInput{Summary: &summary}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {
		ID: "series", Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=5"},
	}}
	f.FullEvents["team@example.com"] = [][]gcal.Event{{}}
	f.instances = []gcal.Event{{
		ID: "instance", RecurringEventID: "series",
		OriginalStartTime: gcal.EventDateTime{DateTime: occurrenceStart},
		Start:             gcal.EventDateTime{DateTime: occurrenceStart},
		End:               gcal.EventDateTime{DateTime: occurrenceEnd},
	}}

	result, err := s.Execute(context.Background(), r, nil)
	requirements.NoError(err)
	requirements.Len(result.Plan, 2)
	requirements.NotNil(result.Plan[0].Event.Recurrence)
	assertions.Contains((*result.Plan[0].Event.Recurrence)[0], "UNTIL=20240311T125959Z")
	requirements.NotNil(result.Plan[1].Event.Start)
	assertions.Equal("America/New_York", result.Plan[1].Event.Start.TimeZone)
	assertions.Equal("2024-03-11T13:00:00Z", result.Plan[1].Event.Start.DateTime.UTC().Format(time.RFC3339))
}

func TestFutureSeriesFirstOccurrenceUsesCalendarTimezoneForChangedBounds(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	loc, err := time.LoadLocation("America/New_York")
	requirements.NoError(err)
	start := gcal.EventDateTime{DateTime: time.Date(2024, 3, 8, 9, 0, 0, 0, loc)}
	end := gcal.EventDateTime{DateTime: time.Date(2024, 3, 8, 10, 0, 0, 0, loc)}
	changedStart := gcal.EventDateTime{DateTime: time.Date(2024, 3, 8, 10, 0, 0, 0, loc)}
	changedEnd := gcal.EventDateTime{DateTime: time.Date(2024, 3, 8, 11, 0, 0, 0, loc)}
	f.Calendars[0].TimeZone = "America/New_York"
	r.Action, r.EventID, r.Scope = "update", "series", "future"
	r.OriginalStart = start.DateTime.Format(time.RFC3339)
	r.Event = gcal.EventInput{Start: &changedStart, End: &changedEnd}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {
		ID: "series", Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=5"},
	}}
	f.FullEvents["team@example.com"] = [][]gcal.Event{{}}
	f.instances = []gcal.Event{{
		ID: "instance", RecurringEventID: "series",
		OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime},
		Start:             start,
		End:               end,
	}}

	result, err := s.Execute(context.Background(), r, nil)

	requirements.NoError(err)
	requirements.Len(result.Plan, 1)
	requirements.NotNil(f.input.Start)
	requirements.NotNil(f.input.End)
	assertions.Equal("America/New_York", f.input.Start.TimeZone)
	assertions.Equal("America/New_York", f.input.End.TimeZone)
	assertions.Equal([]string{"patch:series"}, f.calls)
}

func TestFutureSeriesRequiresTimezoneForTimedRecurrence(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	loc, err := time.LoadLocation("America/New_York")
	requirements.NoError(err)
	start := gcal.EventDateTime{DateTime: time.Date(2024, 3, 8, 9, 0, 0, 0, loc)}
	end := gcal.EventDateTime{DateTime: time.Date(2024, 3, 8, 10, 0, 0, 0, loc)}
	occurrenceStart := time.Date(2024, 3, 11, 9, 0, 0, 0, loc)
	f.Calendars[0].TimeZone = ""
	r.Action, r.EventID, r.Scope = "update", "series", "future"
	r.OriginalStart = "2024-03-11T09:00:00-04:00"
	r.DryRun = true
	summary := "Updated future event"
	r.Event = gcal.EventInput{Summary: &summary}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {
		ID: "series", Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=5"},
	}}
	f.FullEvents["team@example.com"] = [][]gcal.Event{{}}
	f.instances = []gcal.Event{{
		ID: "instance", RecurringEventID: "series",
		OriginalStartTime: gcal.EventDateTime{DateTime: occurrenceStart},
		Start:             gcal.EventDateTime{DateTime: occurrenceStart},
		End:               gcal.EventDateTime{DateTime: occurrenceStart.Add(time.Hour)},
	}}

	_, err = s.Execute(context.Background(), r, nil)
	requirements.ErrorIs(err, ErrInvalid)
	assertions.Contains(err.Error(), "time zone")
	assertions.Empty(f.calls)
}

func TestFutureSplitRejectsStartAfterExistingUntilWithoutNewRule(t *testing.T) {
	requirements := require.New(t)
	s, f, r := fixture(t)
	start, end := *r.Event.Start, *r.Event.End
	r.Action, r.EventID, r.Scope, r.OriginalStart = "update", "series", "future", "2026-10-03T09:00:00Z"
	newStart := gcal.EventDateTime{DateTime: time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC), TimeZone: "UTC"}
	newEnd := gcal.EventDateTime{DateTime: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), TimeZone: "UTC"}
	r.Event = gcal.EventInput{Start: &newStart, End: &newEnd}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {
		ID: "series", Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY;UNTIL=20261004T090000Z"},
	}}
	instanceStart := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	f.instances = []gcal.Event{{
		ID: "instance", RecurringEventID: "series",
		OriginalStartTime: gcal.EventDateTime{DateTime: instanceStart},
		Start:             gcal.EventDateTime{DateTime: instanceStart},
		End:               gcal.EventDateTime{DateTime: instanceStart.Add(time.Hour)},
	}}

	_, err := s.Execute(context.Background(), r, nil)

	requirements.ErrorIs(err, ErrInvalid)
	requirements.Empty(f.calls)
}

func TestFutureSplitCopiesOnlyWritableAttendeeFields(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	start, end := *r.Event.Start, *r.Event.End
	r.Action, r.EventID, r.Scope, r.OriginalStart = "update", "series", "future", "2026-10-03T09:00:00Z"
	r.DryRun = true
	r.Event = gcal.EventInput{Summary: r.Event.Summary}
	attendees := []gcal.Attendee{{
		Email: "guest@example.com", DisplayName: "Guest", Self: true, Organizer: true, ResponseStatus: "accepted",
		Resource: true, Optional: true, AdditionalGuests: 2, Comment: "Join remotely",
	}}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {
		ID: "series", Start: start, End: end, Attendees: attendees, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"},
	}}
	f.instances = []gcal.Event{{
		ID: "instance", RecurringEventID: "series", OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)},
		Start: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)}, End: gcal.EventDateTime{DateTime: end.DateTime.Add(24 * time.Hour)},
	}}

	result, err := s.Execute(context.Background(), r, nil)
	requirements.NoError(err)
	requirements.Len(result.Plan, 2)
	requirements.NotNil(result.Plan[1].Event.Attendees)
	requirements.Len(*result.Plan[1].Event.Attendees, 1)
	attendee := (*result.Plan[1].Event.Attendees)[0]
	assertions.Equal("guest@example.com", attendee.Email)
	assertions.Equal("Guest", attendee.DisplayName)
	assertions.False(attendee.Self)
	assertions.False(attendee.Organizer)
	assertions.Empty(attendee.ResponseStatus)
	assertions.True(attendee.Resource)
	assertions.True(attendee.Optional)
	assertions.Equal(2, attendee.AdditionalGuests)
	assertions.Equal("Join remotely", attendee.Comment)
	requirements.NoError(validateEventInput(result.Plan[1].Event))
	assertions.Empty(f.calls)
}

func TestFutureSplitRejectsGuestRSVPStateBeforeWriting(t *testing.T) {
	for _, responseStatus := range []string{"accepted", "declined", "tentative"} {
		t.Run(responseStatus, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, f, r := fixture(t)
			start, end := *r.Event.Start, *r.Event.End
			r.Action, r.EventID, r.Scope, r.OriginalStart = "update", "series", "future", "2026-10-03T09:00:00Z"
			r.Event = gcal.EventInput{Summary: r.Event.Summary}
			f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {
				ID: "series", Start: start, End: end,
				Attendees:  []gcal.Attendee{{Email: "guest@example.com", ResponseStatus: responseStatus}},
				Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"},
			}}
			f.instances = []gcal.Event{{
				ID: "instance", RecurringEventID: "series", OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)},
				Start: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)}, End: gcal.EventDateTime{DateTime: end.DateTime.Add(24 * time.Hour)},
			}}

			_, err := s.Execute(context.Background(), r, nil)
			requirements.ErrorIs(err, ErrInvalid)
			assertions.Contains(err.Error(), "RSVP")
			assertions.Empty(f.calls)
		})
	}
}

func TestDelegatedFuturePlanRequiresEventContentGrantForProviderDetails(t *testing.T) {
	for _, tc := range []struct {
		name        string
		permissions []agentgrant.Permission
		wantDetails bool
	}{
		{
			name:        "write only",
			permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite, agentgrant.PermissionCalendarInvite},
		},
		{
			name:        "availability read does not expose event details",
			permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite, agentgrant.PermissionCalendarInvite, agentgrant.PermissionCalendarRead},
		},
		{
			name:        "event content permission exposes event details",
			permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite, agentgrant.PermissionCalendarInvite, agentgrant.PermissionCalendarEventRead},
			wantDetails: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, f, r := fixture(t)
			start, end := *r.Event.Start, *r.Event.End
			updatedSummary := "Updated planning"
			r.Action, r.EventID, r.Scope, r.OriginalStart = "update", "series", "future", "2026-10-03T09:00:00Z"
			r.DryRun = true
			r.Event = gcal.EventInput{Summary: &updatedSummary}
			f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {
				ID: "series", Start: start, End: end, Summary: "Private series", Description: "Private agenda", Location: "Private room",
				Attendees:  []gcal.Attendee{{Email: "guest@example.com"}},
				Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"},
			}}
			f.instances = []gcal.Event{{
				ID: "instance", RecurringEventID: "series", OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)},
				Start: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)}, End: gcal.EventDateTime{DateTime: end.DateTime.Add(24 * time.Hour)},
			}}
			grant := &agentgrant.Grant{
				Permissions: tc.permissions,
				Sources:     []agentgrant.SourceRef{{Type: gcal.SourceType, Identifier: "person@example.com/team@example.com"}},
			}

			result, err := s.Execute(context.Background(), r, grant)
			requirements.NoError(err)
			requirements.Len(result.Plan, 2)
			if tc.wantDetails {
				requirements.NotNil(result.Plan[1].Event.Description)
				assertions.Equal("Private agenda", *result.Plan[1].Event.Description)
				requirements.NotNil(result.Plan[1].Event.Attendees)
				requirements.Len(*result.Plan[1].Event.Attendees, 1)
				assertions.Equal("guest@example.com", (*result.Plan[1].Event.Attendees)[0].Email)
			} else {
				assertions.Equal(gcal.EventInput{}, result.Plan[1].Event)
			}
			assertions.Empty(f.calls)
		})
	}
}

func TestDelegatedWriteOnlyFutureSplitExecutesRedactedPlan(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	start, end := *r.Event.Start, *r.Event.End
	updatedSummary := "Updated planning"
	r.Action, r.EventID, r.Scope, r.OriginalStart = "update", "series", "future", "2026-10-03T09:00:00Z"
	r.Event = gcal.EventInput{Summary: &updatedSummary}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {
		ID: "series", Start: start, End: end, Summary: "Private series", Description: "Private agenda", Location: "Private room",
		Attendees:  []gcal.Attendee{{Email: "guest@example.com"}},
		Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"},
	}}
	f.instances = []gcal.Event{{
		ID: "instance", RecurringEventID: "series", OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)},
		Start: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)}, End: gcal.EventDateTime{DateTime: end.DateTime.Add(24 * time.Hour)},
	}}
	grant := &agentgrant.Grant{
		Permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite, agentgrant.PermissionCalendarInvite},
		Sources:     []agentgrant.SourceRef{{Type: gcal.SourceType, Identifier: "person@example.com/team@example.com"}},
	}

	result, err := s.Execute(context.Background(), r, grant)
	requirements.NoError(err)
	requirements.Equal([]string{"patch:series", "insert"}, f.calls)
	requirements.NotNil(f.input.Description)
	assertions.Equal("Private agenda", *f.input.Description)
	requirements.NotNil(f.input.Attendees)
	requirements.Len(*f.input.Attendees, 1)
	assertions.Equal("guest@example.com", (*f.input.Attendees)[0].Email)
	assertions.Empty((*f.input.Attendees)[0].ResponseStatus)
	requirements.Len(result.Plan, 2)
	assertions.Equal(gcal.EventInput{}, result.Plan[1].Event)
}

func TestFutureSplitValidatesGeneratedWritesBeforeTruncating(t *testing.T) {
	s, f, r := fixture(t)
	start, end := *r.Event.Start, *r.Event.End
	r.Action, r.EventID, r.Scope, r.OriginalStart = "update", "series", "future", "2026-10-03T09:00:00Z"
	r.Event = gcal.EventInput{Summary: r.Event.Summary}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {
		ID: "series", Start: start, End: end,
		Attendees:  []gcal.Attendee{{Email: "guest@example.com"}, {Email: "GUEST@example.com"}},
		Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"},
	}}
	f.instances = []gcal.Event{{
		ID: "instance", RecurringEventID: "series", OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)},
		Start: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)}, End: gcal.EventDateTime{DateTime: end.DateTime.Add(24 * time.Hour)},
	}}

	_, err := s.Execute(context.Background(), r, nil)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Empty(t, f.calls)
}

func TestFutureSplitRejectsFieldsItCannotPreserve(t *testing.T) {
	for _, raw := range []string{`{"conferenceData":{"conferenceId":"meeting"}}`, `{"colorId":"4"}`, `{"eventLabelId":"label-1"}`, `{"extendedProperties":{"private":{"key":"value"}}}`, `{"guestsCanInviteOthers":false}`} {
		t.Run(raw, func(t *testing.T) {
			s, f, r := fixture(t)
			start, end := *r.Event.Start, *r.Event.End
			r.Action, r.EventID, r.Scope, r.OriginalStart = "update", "series", "future", "2026-10-03T09:00:00Z"
			r.Event = gcal.EventInput{Summary: r.Event.Summary}
			f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {ID: "series", Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"}, Raw: jsontext.Value(raw)}}
			f.instances = []gcal.Event{{ID: "instance", RecurringEventID: "series", OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)}, Start: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)}, End: gcal.EventDateTime{DateTime: end.DateTime.Add(24 * time.Hour)}}}
			_, err := s.Execute(context.Background(), r, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "manual series edit")
			assert.Empty(t, f.calls)
		})
	}
}

func TestFutureSplitRejectsCancelledException(t *testing.T) {
	s, f, r := fixture(t)
	start := *r.Event.Start
	r.Action, r.EventID, r.Scope, r.OriginalStart = "update", "series", "future", "2026-10-03T09:00:00Z"
	r.Event = gcal.EventInput{Summary: r.Event.Summary}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {ID: "series", Start: start, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"}}}
	f.FullEvents["team@example.com"] = [][]gcal.Event{{{ID: "cancelled", Status: gcal.StatusCancelled, RecurringEventID: "series", OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime.Add(48 * time.Hour)}}}}
	_, err := s.Execute(context.Background(), r, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "detached exceptions")
	assert.Empty(t, f.calls)
}

func TestFutureAllDaySplitPreservesDateUntil(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	start, end := gcal.EventDateTime{Date: "2026-10-02"}, gcal.EventDateTime{Date: "2026-10-03"}
	r.Action, r.EventID, r.Scope, r.OriginalStart = "update", "series", "future", "2026-10-03"
	r.Event = gcal.EventInput{Summary: r.Event.Summary}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {ID: "series", Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY;UNTIL=20261006"}}}
	f.instances = []gcal.Event{{ID: "instance", RecurringEventID: "series", OriginalStartTime: gcal.EventDateTime{Date: "2026-10-03"}, Start: gcal.EventDateTime{Date: "2026-10-03"}, End: gcal.EventDateTime{Date: "2026-10-04"}}}
	result, err := s.Execute(context.Background(), r, nil)
	requirements.NoError(err)
	requirements.Len(result.Plan, 2)
	requirements.NotNil(result.Plan[0].Event.Recurrence)
	assertions.Contains(*result.Plan[0].Event.Recurrence, "RRULE:FREQ=DAILY;UNTIL=20261002")
	requirements.NotNil(result.Plan[1].Event.Recurrence)
	assertions.Contains(*result.Plan[1].Event.Recurrence, "RRULE:FREQ=DAILY;UNTIL=20261006")
	assertions.Equal("2026-10-03", result.Plan[1].Event.Start.Date)
}
func TestSingleSeriesRequiresOriginalAndPreservesRescheduledInstance(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	start, end := *r.Event.Start, *r.Event.End
	r.Action = "delete"
	r.EventID = "series"
	r.Event = gcal.EventInput{}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {ID: "series", Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY"}}}
	_, err := s.Execute(context.Background(), r, nil)
	requirements.Error(err)
	assertions.Empty(f.calls)
	r.OriginalStart = "2026-10-02T09:00:00Z"
	f.instances = []gcal.Event{{ID: "rescheduled", RecurringEventID: "series", OriginalStartTime: start, Start: gcal.EventDateTime{DateTime: start.DateTime.Add(7 * 24 * time.Hour)}}}
	_, err = s.Execute(context.Background(), r, nil)
	requirements.NoError(err)
	assertions.Equal([]string{"delete:rescheduled"}, f.calls)
}

func TestSingleSeriesMatchesOriginalStartLocallyAcrossPages(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	start, end := *r.Event.Start, *r.Event.End
	r.Action, r.EventID, r.OriginalStart = "delete", "series", "2026-10-03T09:00:00Z"
	r.Event = gcal.EventInput{}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{
		"series": {ID: "series", Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY"}},
	}
	selected := gcal.Event{
		ID: "rescheduled", RecurringEventID: "series",
		OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)},
		Start:             gcal.EventDateTime{DateTime: start.DateTime.Add(7 * 24 * time.Hour)},
	}
	f.instancePages = map[string]gcal.EventsPage{
		"":       {Items: []gcal.Event{{ID: "other"}}, NextPageToken: "page-2"},
		"page-2": {Items: []gcal.Event{selected}},
	}

	_, err := s.Execute(context.Background(), r, nil)
	requirements.NoError(err)
	assertions.Equal([]string{"delete:rescheduled"}, f.calls)
	assertions.Equal([]gcal.EventsListParams{{}, {PageToken: "page-2"}}, f.instanceCalls)
}

func TestSingleSeriesLookupStopsAfter100Pages(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	start, end := *r.Event.Start, *r.Event.End
	r.Action, r.EventID, r.OriginalStart = "delete", "series", "2026-10-03T09:00:00Z"
	r.Event = gcal.EventInput{}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{
		"series": {ID: "series", Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY"}},
	}
	f.instancePages = map[string]gcal.EventsPage{}
	token := ""
	for page := range 100 {
		next := fmt.Sprintf("page-%d", page+1)
		f.instancePages[token] = gcal.EventsPage{NextPageToken: next}
		token = next
	}
	f.instancePages[token] = gcal.EventsPage{}

	_, err := s.Execute(context.Background(), r, nil)

	requirements.Error(err)
	assertions.Contains(err.Error(), "instance lookup exceeds 100 pages")
	assertions.Len(f.instanceCalls, 100)
	assertions.Empty(f.calls)
}
func TestFutureRejectsUnmigratableRulesBeforeWriting(t *testing.T) {
	s, f, r := fixture(t)
	start := *r.Event.Start
	r.Action = "delete"
	r.EventID = "series"
	r.Event = gcal.EventInput{}
	r.Scope = "future"
	r.OriginalStart = "2026-10-03T09:00:00Z"
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {ID: "series", Start: start, Recurrence: []string{"RRULE:FREQ=DAILY", "EXDATE:20261004T090000Z"}}}
	_, err := s.Execute(context.Background(), r, nil)
	require.Error(t, err)
	assert.Empty(t, f.calls)
}
func TestFutureSplitRejectsPrivateCopyBeforeWriting(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	start := *r.Event.Start
	r.Action, r.EventID, r.Scope, r.OriginalStart = "update", "series", "future", "2026-10-03T09:00:00Z"
	r.Event = gcal.EventInput{Summary: r.Event.Summary}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{
		"series": {
			ID: "series", Start: start, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"},
			Raw: jsontext.Value(`{"privateCopy":true}`),
		},
	}
	f.instances = []gcal.Event{{
		ID: "instance", RecurringEventID: "series",
		OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)},
		Start:             gcal.EventDateTime{DateTime: start.DateTime.Add(24 * time.Hour)},
		End:               gcal.EventDateTime{DateTime: start.DateTime.Add(25 * time.Hour)},
	}}

	_, err := s.Execute(context.Background(), r, nil)
	requirements.ErrorIs(err, ErrInvalid)
	assertions.Contains(err.Error(), "privateCopy")
	assertions.Empty(f.calls)
}

func TestConflictIntersectionsExcludeTouchingBounds(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	start := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	result := conflicts(&gcal.FreeBusyResponse{Calendars: map[string]gcal.CalendarBusy{
		"a@example.com": {Busy: []gcal.BusyPeriod{{Start: start, End: start.Add(time.Hour)}}},
		"b@example.com": {Busy: []gcal.BusyPeriod{{Start: start.Add(30 * time.Minute), End: start.Add(2 * time.Hour)}}},
		"c@example.com": {Busy: []gcal.BusyPeriod{{Start: start.Add(2 * time.Hour), End: start.Add(3 * time.Hour)}}},
	}})
	requirements.Len(result, 1)
	assertions.Equal([]string{"a@example.com", "b@example.com"}, result[0].CalendarIDs)
	assertions.Equal(start.Add(30*time.Minute), result[0].Start)
	assertions.Equal(start.Add(time.Hour), result[0].End)
}
