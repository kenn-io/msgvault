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
		name, uid, fraction string
		nanoseconds         int
	}{
		{"without own UID", "", "123", 123000000},
		{"with own UID", `,"iCalUID":"uid@example.org"`, "123", 123000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			calendarFixture(t, st, "master", `{"id":"series","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, "owner@example.com")
			wire := fmt.Sprintf(`{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00.%sZ"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}%s}`, tc.fraction, tc.uid)
			imported, err := gcalmodel.DecodeEvent([]byte(wire))
			requirements.NoError(err)
			requirements.Equal(tc.nanoseconds, imported.OriginalStartTime.DateTime.Nanosecond())
			_, edit := calendarFixture(t, st, "edit", string(imported.Raw), "owner@example.com")
			_, exactID := projectionFixture(t, st, "exact-id", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"edit"}}`)
			_, exactTime := projectionFixture(t, st, "exact-time", "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T06:00:00.%s-04:00"}}`, tc.fraction))
			_, otherTime := projectionFixture(t, st, "other-time", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00.124Z"}}`)
			_, wholeSecond := projectionFixture(t, st, "whole-second", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"series_20270314T100000Z"}}`)
			t.Run("exact link", func(t *testing.T) {
				require := require.New(t)
				for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: edit}, {Kind: "msg", MessageID: exactID}, {Kind: "msg", MessageID: exactTime}} {
					record, err := st.MeetingRecordContext(t.Context(), seed)
					require.NoError(err)
					require.Len(record.Links, 1)
					require.Equal(fmt.Sprintf("evt:%d", edit), record.Links[0].Ref)
					require.Equal([]int64{exactID, exactTime}, record.MessageIDs)
					require.NotContains(record.MessageIDs, wholeSecond)
				}
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: otherTime})
				require.NoError(err)
				require.Empty(record.Links)
				require.Equal([]int64{otherTime}, record.MessageIDs)
				record, err = st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: wholeSecond})
				require.NoError(err)
				for _, link := range record.Links {
					require.NotEqual(edit, link.MessageID)
				}
			})
			t.Run("listing reference", func(t *testing.T) {
				require := require.New(t)
				_, sibling := calendarFixture(t, st, "sibling", `{"id":"sibling","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"},"start":{"dateTime":"2027-03-21T12:00:00Z"},"end":{"dateTime":"2027-03-21T13:00:00Z"}}`, "owner@example.com")
				from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
				page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 8))
				require.NoError(err)
				for _, item := range []struct {
					id    int64
					nanos int
				}{{edit, tc.nanoseconds}, {sibling, 0}} {
					var listed CalendarOccurrence
					for _, occurrence := range page.Occurrences {
						if occurrence.MessageID == item.id {
							listed = occurrence
							break
						}
					}
					require.NotEmpty(listed.Ref)
					require.Equal(item.nanos, listed.original.Instant.Nanosecond())
					id, key, hasKey := strings.Cut(strings.TrimPrefix(listed.Ref, "evt:"), "@")
					messageID, err := strconv.ParseInt(id, 10, 64)
					require.NoError(err)
					ref := MeetingLinkRef{Kind: "evt", MessageID: messageID}
					if hasKey {
						_, ref.Identity, hasKey = calrecur.ParseInstanceID("event_" + key)
						require.True(hasKey)
					}
					record, err := st.MeetingRecordContext(t.Context(), ref)
					require.NoError(err)
					require.Len(record.Links, 1)
					require.Equal(item.id, record.Links[0].MessageID)
					require.Equal(listed.Ref, record.Links[0].Ref)
				}
			})
		})
	}
}
