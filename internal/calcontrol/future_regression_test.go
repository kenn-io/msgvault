package calcontrol

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/teambition/rrule-go"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/gcal"
)

func TestFutureRecurrenceMustMatchReplacementStart(t *testing.T) {
	for _, tc := range []struct {
		name, oldRule, replacement             string
		oldAllDay, newAllDay, first, wantError bool
	}{
		{name: "all-day to timed inherited UNTIL", oldRule: "RRULE:FREQ=DAILY;UNTIL=20261020", oldAllDay: true, wantError: true},
		{name: "timed to all-day inherited UNTIL", oldRule: "RRULE:FREQ=DAILY;UNTIL=20261020T090000Z", newAllDay: true, wantError: true},
		{name: "first occurrence inherited UNTIL", oldRule: "RRULE:FREQ=DAILY;UNTIL=20261020", oldAllDay: true, first: true, wantError: true},
		{name: "explicit mismatched UNTIL", oldRule: "RRULE:FREQ=DAILY;COUNT=14", replacement: "RRULE:FREQ=DAILY;UNTIL=20261020", wantError: true},
		{name: "explicit compatible timed UNTIL", oldRule: "RRULE:FREQ=DAILY;UNTIL=20261020", replacement: "RRULE:FREQ=DAILY;UNTIL=20261020T090000Z", oldAllDay: true},
		{name: "explicit compatible all-day UNTIL", oldRule: "RRULE:FREQ=DAILY;UNTIL=20261020T090000Z", replacement: "RRULE:FREQ=DAILY;UNTIL=20261020", newAllDay: true},
		{name: "weekly Monday moved Tuesday", oldRule: "RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=4", wantError: true},
		{name: "explicit Monday still mismatches Tuesday", oldRule: "RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=4", replacement: "RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=3", wantError: true},
		{name: "explicit Tuesday matches", oldRule: "RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=4", replacement: "RRULE:FREQ=WEEKLY;BYDAY=TU;COUNT=3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, f, r := fixture(t)
			start := gcal.EventDateTime{DateTime: time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC), TimeZone: "UTC"}
			end := gcal.EventDateTime{DateTime: start.DateTime.Add(time.Hour), TimeZone: "UTC"}
			selected := gcal.EventDateTime{DateTime: start.DateTime.AddDate(0, 0, 7), TimeZone: "UTC"}
			selectedEnd := gcal.EventDateTime{DateTime: selected.DateTime.Add(time.Hour), TimeZone: "UTC"}
			if tc.first {
				selected, selectedEnd = start, end
			}
			if tc.oldAllDay {
				start = gcal.EventDateTime{Date: "2026-10-05"}
				end = gcal.EventDateTime{Date: "2026-10-06"}
				selected = gcal.EventDateTime{Date: "2026-10-12"}
				selectedEnd = gcal.EventDateTime{Date: "2026-10-13"}
				if tc.first {
					selected, selectedEnd = start, end
				}
			}
			newStart := gcal.EventDateTime{DateTime: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC), TimeZone: "UTC"}
			newEnd := gcal.EventDateTime{DateTime: newStart.DateTime.Add(time.Hour), TimeZone: "UTC"}
			if tc.newAllDay {
				newStart = gcal.EventDateTime{Date: "2026-10-13"}
				newEnd = gcal.EventDateTime{Date: "2026-10-14"}
			}
			r.Action, r.EventID, r.Scope = "update", "series", "future"
			r.OriginalStart = selected.DateTime.Format(time.RFC3339)
			if tc.oldAllDay {
				r.OriginalStart = selected.Date
			}
			r.Event = gcal.EventInput{Start: &newStart, End: &newEnd}
			if tc.replacement != "" {
				r.Event.Recurrence = &[]string{tc.replacement}
			}
			f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {ID: "series", Start: start, End: end, Recurrence: []string{tc.oldRule}}}
			f.instances = []gcal.Event{{ID: "instance", RecurringEventID: "series", OriginalStartTime: selected, Start: selected, End: selectedEnd}}
			_, err := s.Execute(t.Context(), r, nil)
			if tc.wantError {
				requirements.ErrorIs(err, ErrInvalid)
				if tc.name == "explicit mismatched UNTIL" {
					assertions.Contains(err.Error(), "UNTIL must match")
				}
				assertions.Empty(f.calls)
			} else {
				requirements.NoError(err)
				assertions.Equal([]string{"patch:series", "insert"}, f.calls)
			}
		})
	}
}

func TestFirstFuturePatchValidatesMergedBounds(t *testing.T) {
	for _, tc := range []struct {
		name               string
		startOnly, invalid bool
	}{
		{"valid start-only", true, false}, {"start after preserved end", true, true}, {"valid end-only", false, false}, {"end before preserved start", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, f, r := fixture(t)
			start, end := *r.Event.Start, *r.Event.End
			r.Action, r.EventID, r.Scope, r.OriginalStart = "update", "series", "future", start.DateTime.Format(time.RFC3339)
			changed := start
			if tc.startOnly {
				changed.DateTime = start.DateTime.Add(30 * time.Minute)
				if tc.invalid {
					changed.DateTime = end.DateTime.Add(time.Hour)
				}
				r.Event = gcal.EventInput{Start: &changed}
			} else {
				changed.DateTime = end.DateTime.Add(time.Hour)
				if tc.invalid {
					changed.DateTime = start.DateTime.Add(-time.Hour)
				}
				r.Event = gcal.EventInput{End: &changed}
			}
			f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {ID: "series", Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"}}}
			f.instances = []gcal.Event{{ID: "instance", RecurringEventID: "series", OriginalStartTime: start, Start: start, End: end}}
			_, err := s.Execute(t.Context(), r, nil)
			if tc.invalid {
				requirements.ErrorIs(err, ErrInvalid)
				assertions.Empty(f.calls)
				return
			}
			requirements.NoError(err)
			if tc.startOnly {
				assertions.Nil(f.input.End)
			} else {
				assertions.Nil(f.input.Start)
			}
			assertions.Equal([]string{"patch:series"}, f.calls)
		})
	}
}

func TestFutureInstanceRejectsDifferentOriginalStart(t *testing.T) {
	s, f, r := fixture(t)
	start, end := *r.Event.Start, *r.Event.End
	r.Action, r.EventID, r.Scope, r.OriginalStart = "delete", "selected", "future", "2026-10-04T09:00:00Z"
	r.Event = gcal.EventInput{}
	selected := gcal.EventDateTime{DateTime: start.DateTime.AddDate(0, 0, 1)}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"selected": {ID: "selected", RecurringEventID: "series", OriginalStartTime: selected}, "series": {ID: "series", Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"}}}
	f.instances = []gcal.Event{{ID: "different", RecurringEventID: "series", OriginalStartTime: gcal.EventDateTime{DateTime: start.DateTime.AddDate(0, 0, 2)}}}
	_, err := s.Execute(t.Context(), r, nil)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "does not match")
	assert.Empty(t, f.calls)
}

func TestFutureCountRulesAndFirstDelete(t *testing.T) {
	for _, tc := range []struct {
		name, rule, start, selected string
		allDay, deleteFirst         bool
		remaining                   int
	}{
		{"weekdays", "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;COUNT=10", "2026-10-05", "2026-10-07", false, false, 8},
		{"monthly first Monday", "FREQ=MONTHLY;BYDAY=1MO;COUNT=4", "2026-10-05", "2026-11-02", false, false, 3},
		{"all-day count", "FREQ=DAILY;COUNT=4", "2026-10-02", "2026-10-03", true, false, 3},
		{"first future delete", "FREQ=DAILY;COUNT=4", "2026-10-02", "2026-10-02", false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, f, r := fixture(t)
			startDate, err := time.Parse("2006-01-02", tc.start)
			requirements.NoError(err)
			selectedDate, err := time.Parse("2006-01-02", tc.selected)
			requirements.NoError(err)
			start := gcal.EventDateTime{DateTime: startDate.Add(9 * time.Hour), TimeZone: "UTC"}
			end := gcal.EventDateTime{DateTime: start.DateTime.Add(time.Hour), TimeZone: "UTC"}
			selected := gcal.EventDateTime{DateTime: selectedDate.Add(9 * time.Hour), TimeZone: "UTC"}
			selectedEnd := gcal.EventDateTime{DateTime: selected.DateTime.Add(time.Hour), TimeZone: "UTC"}
			if tc.allDay {
				start = gcal.EventDateTime{Date: tc.start}
				end = gcal.EventDateTime{Date: startDate.AddDate(0, 0, 1).Format("2006-01-02")}
				selected = gcal.EventDateTime{Date: tc.selected}
				selectedEnd = gcal.EventDateTime{Date: selectedDate.AddDate(0, 0, 1).Format("2006-01-02")}
			}
			r.Action, r.EventID, r.Scope = "update", "series", "future"
			r.OriginalStart = selected.DateTime.Format(time.RFC3339)
			if tc.allDay {
				r.OriginalStart = selected.Date
			}
			r.Event = gcal.EventInput{Summary: r.Event.Summary}
			if tc.deleteFirst {
				r.Action = "delete"
				r.Event = gcal.EventInput{}
			}
			f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": {ID: "series", ETag: `"v1"`, Start: start, End: end, Recurrence: []string{"RRULE:" + tc.rule}}}
			f.instances = []gcal.Event{{ID: "instance", RecurringEventID: "series", OriginalStartTime: selected, Start: selected, End: selectedEnd}}
			result, err := s.Execute(t.Context(), r, nil)
			requirements.NoError(err)
			if tc.deleteFirst {
				assertions.Equal([]string{"delete:series"}, f.calls)
				assertions.Equal(`"v1"`, f.options.IfMatch)
				return
			}
			requirements.Len(result.Plan, 2)
			requirements.NotNil(result.Plan[1].Event.Recurrence)
			option, err := rrule.StrToROption((*result.Plan[1].Event.Recurrence)[0][6:])
			requirements.NoError(err)
			instant, _ := selected.Instant()
			option.Dtstart = instant
			rule, err := rrule.NewRRule(*option)
			requirements.NoError(err)
			assertions.Len(rule.All(), tc.remaining)
			assertions.Equal(`"v1"`, f.patchOptions[0].IfMatch)
		})
	}
}

type restoreFailureFake struct {
	*controlFake

	restoreError    error
	missingResponse bool
}

func (f *restoreFailureFake) InsertEvent(context.Context, string, gcal.EventInput, gcal.MutationOptions) (*gcal.Event, error) {
	f.calls = append(f.calls, "insert")
	return nil, errors.New("quota exhausted")
}
func (f *restoreFailureFake) PatchEvent(ctx context.Context, cal, id string, in gcal.EventInput, opt gcal.MutationOptions) (*gcal.Event, error) {
	event, err := f.controlFake.PatchEvent(ctx, cal, id, in, opt)
	if len(f.patchOptions) == 1 {
		return event, err
	}
	if f.restoreError != nil {
		return nil, f.restoreError
	}
	if f.missingResponse {
		return nil, nil //nolint:nilnil // Exercise a provider success that omits the required event response.
	}
	restored := *event
	restored.ID = id
	restored.Recurrence = *in.Recurrence
	return &restored, nil
}

func TestFutureFailedReplacementRestoresWithReturnedETag(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		restoreError                            error
		missingETag, missingResponse, writeOnly bool
		code                                    string
		receipts                                int
	}{
		{name: "restored", code: "calendar_partial", receipts: 2},
		{name: "write-only restored", writeOnly: true, code: "calendar_partial", receipts: 2},
		{name: "changed meanwhile", restoreError: errors.New("precondition failed"), code: "calendar_partial", receipts: 1},
		{name: "unknown restoration", restoreError: gcal.ErrOutcomeUnknown, code: "calendar_outcome_unknown", receipts: 1},
		{name: "empty restoration response", missingResponse: true, code: "calendar_outcome_unknown", receipts: 1},
		{name: "missing truncation ETag", missingETag: true, code: "calendar_partial", receipts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, f, r := fixture(t)
			start, end := *r.Event.Start, *r.Event.End
			r.Action, r.EventID, r.Scope, r.OriginalStart = "update", "series", "future", "2026-10-03T09:00:00Z"
			r.Event = gcal.EventInput{Summary: r.Event.Summary}
			original := gcal.Event{ID: "series", ETag: `"v1"`, Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=4"}}
			f.EventsByID["team@example.com"] = map[string]gcal.Event{"series": original}
			selected := gcal.EventDateTime{DateTime: start.DateTime.AddDate(0, 0, 1)}
			f.instances = []gcal.Event{{ID: "instance", RecurringEventID: "series", OriginalStartTime: selected, Start: selected, End: gcal.EventDateTime{DateTime: selected.DateTime.Add(time.Hour)}}}
			f.result = original
			f.result.ETag = `"v2"`
			if tc.missingETag {
				f.result.ETag = ""
			}
			s.Client = &restoreFailureFake{controlFake: f, restoreError: tc.restoreError, missingResponse: tc.missingResponse}
			var archived []gcal.Event
			s.Persist = func(_ context.Context, _ gcal.Calendar, event gcal.Event) (int64, error) {
				archived = append(archived, event)
				return 42, nil
			}
			var grant *agentgrant.Grant
			if tc.writeOnly {
				grant = &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite}, Sources: []agentgrant.SourceRef{{Type: gcal.SourceType, Identifier: "person@example.com/team@example.com"}}}
			}
			result, err := s.Execute(t.Context(), r, grant)
			requirements.NoError(err)
			assertions.Equal(tc.code, result.OutcomeCode)
			assertions.Equal(tc.code == "calendar_outcome_unknown", result.OutcomeUnknown)
			requirements.Len(result.Writes, tc.receipts)
			assertions.Len(archived, tc.receipts)
			assertions.Equal(`"v1"`, f.patchOptions[0].IfMatch)
			if tc.missingETag {
				assertions.Equal([]string{"patch:series", "insert"}, f.calls)
				assertions.Contains(result.Error, "omitted its ETag")
				return
			}
			assertions.Equal([]string{"patch:series", "insert", "patch:series"}, f.calls)
			requirements.Len(f.patchOptions, 2)
			assertions.Equal(`"v2"`, f.patchOptions[1].IfMatch)
			assertions.Equal(gcal.EventInput{Recurrence: &original.Recurrence}, f.patchInputs[1])
			if tc.receipts == 2 {
				assertions.Equal("restore", result.Writes[1].Action)
				assertions.True(result.Writes[1].Archived)
				assertions.Equal(original.Recurrence, archived[1].Recurrence)
				assertions.Contains(result.Error, "original recurrence restored")
			}
			if tc.writeOnly {
				assertions.Empty(result.Writes[0].Event.ETag)
				assertions.Empty(result.Writes[1].Event.Recurrence)
			}
		})
	}
}
