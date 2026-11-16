package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingRecordICSNotes(t *testing.T) {
	for _, mode := range []string{"file", "email"} {
		t.Run(mode, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			raw := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nORGANIZER:mailto:organizer@example.com")
			if mode == "email" {
				mail, err := st.GetOrCreateSource("mbox", "owner@example.com")
				require.NoError(t, err)
				source = mail.ID
				persistInviteEmailForSource(t, st, source, "invitation", inviteEmail(raw))
			} else {
				_, err := importICalendarForTest(t, st, source, raw)
				require.NoError(t, err)
			}
			ids := emailEvents(t, st, source)
			require.Len(t, ids, 1)
			_, note := projectionFixture(t, st, "twenty-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"family@example.com","startsAt":"2027-03-02T12:00:00Z"}}`)
			for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: ids[0]}, {Kind: "msg", MessageID: note}} {
				record, err := st.MeetingRecordContext(t.Context(), seed)
				require.NoError(t, err)
				assert.Equal(t, []int64{note}, record.MessageIDs)
				require.Len(t, record.Links, 1)
				assert.Equal(t, ids[0], record.Links[0].MessageID)
			}
		})
	}
}

func TestMeetingRecordICSRangeEdit(t *testing.T) {
	for _, withMaster := range []bool{false, true} {
		t.Run(fmt.Sprint(withMaster), func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			if withMaster {
				_, err := importICalendarForTest(t, st, source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=20"))
				require.NoError(t, err)
			}
			_, err := importICalendarForTest(t, st, source, calendarDocument("REQUEST", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H"))
			require.NoError(t, err)
			ids := emailEvents(t, st, source)
			edit := ids[len(ids)-1]
			record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: edit})
			require.NoError(t, err)
			require.Len(t, record.Links, 1)
			assert.Equal(t, edit, record.Links[0].MessageID)
			require.NotNil(t, record.Links[0].Start)
			assert.Equal(t, time.Date(2027, 3, 3, 14, 0, 0, 0, time.UTC), *record.Links[0].Start)
			require.NotNil(t, record.Links[0].End)
			assert.Equal(t, time.Date(2027, 3, 3, 15, 0, 0, 0, time.UTC), *record.Links[0].End)
			assert.Contains(t, record.Unexpandable, CalendarUnexpandable{MessageID: edit, Reason: "unsupported recurrence: RANGE=THISANDFUTURE"})
			from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 2))
			require.NoError(t, err)
			assert.Empty(t, page.Occurrences)
			assert.NotEmpty(t, page.Unexpandable)
		})
	}
}

func TestMeetingRecordICSProjectionUpgrade(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	_, err := importICalendarForTest(t, st, source, calendarFamilyDocument())
	require.NoError(t, err)
	ids := emailEvents(t, st, source)
	require.Len(t, ids, 2)
	_, err = st.db.ExecContext(t.Context(), `UPDATE calendar_events SET ical_uid = '', projection_version = 3`)
	require.NoError(t, err)
	_, err = st.db.ExecContext(t.Context(), `UPDATE applied_migrations SET version = 3 WHERE name = 'calendar_event_projection_v1'`)
	require.NoError(t, err)
	_, err = st.db.ExecContext(t.Context(), `UPDATE archive_metadata SET value = '44' WHERE key = 'schema_version'`)
	require.NoError(t, err)
	require.NoError(t, st.InitSchemaContext(t.Context()))
	for _, id := range ids {
		row := calendarRows(t, st, id)[0]
		assert.Equal(t, "family@example.com", row.Event.ICalUID)
		assert.NotEmpty(t, row.Event.ProviderEventID)
		if row.Event.Kind == calendarExceptionKind {
			assert.NotEmpty(t, row.Event.SeriesProviderID)
		}
	}
}
