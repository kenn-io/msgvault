package store

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
)

func TestMeetingRecordFractionalExceptionImportAndListing(t *testing.T) {
	for _, tc := range []struct {
		name string
		uid  string
	}{
		{"without own UID", ""},
		{"with own UID", `,"iCalUID":"uid@example.org"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			calendarFixture(t, st, "master", `{"id":"series","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, "owner@example.com")
			wire := fmt.Sprintf(`{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00.0000001Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}%s}`, tc.uid)
			imported, err := gcalmodel.DecodeEvent([]byte(wire))
			requirements.NoError(err)
			requirements.Equal(100, imported.OriginalStartTime.DateTime.Nanosecond())
			_, edit := calendarFixture(t, st, "edit", string(imported.Raw), "owner@example.com")
			noteRaw := `{"id":"whole-second","owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"series_20270314T100000Z"}}`
			_, wholeSecond := projectionFixture(t, st, "whole-second", "granola_json", noteRaw)
			t.Run("exact link", func(t *testing.T) {
				require := require.New(t)
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: edit})
				require.NoError(err)
				require.NotContains(record.MessageIDs, wholeSecond)
			})
			t.Run("listing reference", func(t *testing.T) {
				require := require.New(t)
				from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
				page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
				require.NoError(err)
				var listed CalendarOccurrence
				for _, occurrence := range page.Occurrences {
					if occurrence.MessageID == edit {
						listed = occurrence
						break
					}
				}
				require.NotEmpty(listed.Ref)
				require.Equal(100, listed.original.Instant.Nanosecond())
				id, key, hasKey := strings.Cut(strings.TrimPrefix(listed.Ref, "evt:"), "@")
				messageID, err := strconv.ParseInt(id, 10, 64)
				require.NoError(err)
				ref := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: messageID}}
				if hasKey {
					_, ref.Identity, hasKey = calrecur.ParseInstanceID("event_" + key)
					require.True(hasKey)
				}
				record, err := st.MeetingRecordContext(t.Context(), ref)
				require.NoError(err)
				require.Len(record.Links, 1)
				require.Equal(edit, record.Links[0].MessageID)
				require.Equal(listed.Ref, record.Links[0].Ref)
			})
		})
	}
}

func TestMeetingRecordListingReferenceWithFractionalSibling(t *testing.T) {
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "master", `{"id":"series","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, "owner@example.com")
	calendarFixture(t, st, "fractional", `{"id":"fractional","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00.0000001Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, "owner@example.com")
	_, edit := calendarFixture(t, st, "edit", `{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"},"start":{"dateTime":"2027-03-21T12:00:00Z"},"end":{"dateTime":"2027-03-21T13:00:00Z"}}`, "owner@example.com")
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 8))
	require.NoError(err)
	var listed CalendarOccurrence
	for _, occurrence := range page.Occurrences {
		if occurrence.MessageID == edit {
			listed = occurrence
			break
		}
	}
	require.NotEmpty(listed.Ref)
	id, key, hasKey := strings.Cut(strings.TrimPrefix(listed.Ref, "evt:"), "@")
	messageID, err := strconv.ParseInt(id, 10, 64)
	require.NoError(err)
	ref := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: messageID}}
	if hasKey {
		_, ref.Identity, hasKey = calrecur.ParseInstanceID("event_" + key)
		require.True(hasKey)
	}
	record, err := st.MeetingRecordContext(t.Context(), ref)
	require.NoError(err)
	require.Len(record.Links, 1)
	require.Equal(edit, record.Links[0].MessageID)
	require.Equal(listed.Ref, record.Links[0].Ref)
}
