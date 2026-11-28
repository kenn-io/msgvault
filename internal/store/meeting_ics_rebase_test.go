package store

import (
	"fmt"
	"strconv"
	"strings"
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
		t.Run(strconv.FormatBool(withMaster), func(t *testing.T) {
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

func TestMeetingRecordICSSeparateEditWithRange(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	for _, properties := range []string{
		"DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=20",
		"RECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H",
		"RECURRENCE-ID:20270304T120000Z\r\nDTSTART:20270304T150000Z\r\nDURATION:PT1H",
	} {
		_, err := importICalendarForTest(t, st, source, calendarDocument("REQUEST", properties))
		require.NoError(t, err)
	}
	ids := emailEvents(t, st, source)
	require.Len(t, ids, 3)
	edit := ids[2]
	_, note := projectionFixture(t, st, "separate-edit", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"family@example.com","startsAt":"2027-03-04T12:00:00Z"}}`)
	for _, ref := range []MeetingLinkRef{{Kind: "evt", MessageID: edit}, {Kind: "msg", MessageID: note}} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		require.NoError(t, err)
		require.Len(t, record.Links, 1)
		assert.Equal(t, edit, record.Links[0].MessageID)
		assert.Equal(t, []int64{note}, record.MessageIDs)
		require.NotNil(t, record.Links[0].Start)
		assert.Equal(t, time.Date(2027, 3, 4, 15, 0, 0, 0, time.UTC), *record.Links[0].Start)
		assert.Contains(t, record.Unexpandable, CalendarUnexpandable{MessageID: ids[1], Reason: "unsupported recurrence: RANGE=THISANDFUTURE"})
	}
	from := time.Date(2027, 3, 4, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	require.NoError(t, err)
	assert.Empty(t, page.Occurrences)
	assert.NotEmpty(t, page.Unexpandable)
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
	_, err = st.db.ExecContext(t.Context(), `UPDATE archive_metadata SET value = '45' WHERE key = 'schema_version'`)
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

func frozenICSFixture(t *testing.T, master, method, properties string) (*Store, int64, int64) {
	t.Helper()
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	_, err := importICalendarForTest(t, st, source, []byte(master))
	require.NoError(t, err)
	_, err = importICalendarForTest(t, st, source, calendarDocument("REQUEST", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H"))
	require.NoError(t, err)
	_, err = importICalendarForTest(t, st, source, calendarDocument(method, properties))
	require.NoError(t, err)
	ids := emailEvents(t, st, source)
	require.Len(t, ids, 3)
	return st, ids[0], ids[2]
}

func TestFrozenICSUntimedCancellation(t *testing.T) {
	master := string(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=20\r\nSUMMARY:Original"))
	st, series, cancel := frozenICSFixture(t, master, "CANCEL", "RECURRENCE-ID:20270304T120000Z\r\nSTATUS:CANCELLED")
	_, note := projectionFixture(t, st, "cancel-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"family@example.com","startsAt":"2027-03-04T12:00:00Z"}}`)
	for _, ref := range []MeetingLinkRef{{Kind: "evt", MessageID: cancel}, {Kind: "evt", MessageID: series, Identity: occurrenceIdentity(t, "20270304T120000Z")}, {Kind: "msg", MessageID: note}} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		require.NoError(t, err)
		require.Len(t, record.Links, 1)
		assert.Equal(t, cancel, record.Links[0].MessageID)
		assert.Nil(t, record.Links[0].Start)
		assert.Nil(t, record.Links[0].End)
		assert.Equal(t, "cancelled", record.Links[0].Status)
		assert.Equal(t, []int64{note}, record.MessageIDs)
	}
}

func TestFrozenICSTimedEditSeriesRef(t *testing.T) {
	master := string(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=20"))
	st, series, edit := frozenICSFixture(t, master, "REQUEST", "RECURRENCE-ID:20270304T120000Z\r\nDTSTART:20270304T150000Z\r\nDURATION:PT1H")
	_, note := projectionFixture(t, st, "timed-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"family@example.com","startsAt":"2027-03-04T12:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: series, Identity: occurrenceIdentity(t, "20270304T120000Z")})
	require.NoError(t, err)
	require.Len(t, record.Links, 1)
	assert.Equal(t, edit, record.Links[0].MessageID)
	assert.Equal(t, []int64{note}, record.MessageIDs)
}

func TestFrozenICSManualDecisionOnEditedOccurrence(t *testing.T) {
	master := string(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=20"))
	st, series, edit := frozenICSFixture(t, master, "REQUEST", "RECURRENCE-ID:20270304T120000Z\r\nDTSTART:20270304T150000Z\r\nDURATION:PT1H")
	_, note := projectionFixture(t, st, "frozen-manual-note", "meeting_json", `{}`)
	seriesRef := MeetingLinkRef{Kind: "evt", MessageID: series, Identity: occurrenceIdentity(t, "20270304T120000Z")}
	noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
	_, err := st.DecideMeetingLinkContext(t.Context(), seriesRef, noteRef, "link")
	require.NoError(t, err)
	for _, ref := range []MeetingLinkRef{seriesRef, {Kind: "evt", MessageID: edit}, noteRef} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		require.NoError(t, err)
		require.Len(t, record.Links, 1)
		assert.Equal(t, edit, record.Links[0].MessageID)
		require.NotNil(t, record.Links[0].Start)
		assert.Equal(t, time.Date(2027, 3, 4, 15, 0, 0, 0, time.UTC), *record.Links[0].Start)
		assert.Equal(t, []int64{note}, record.MessageIDs)
	}
}

func TestFrozenICSAllDayOriginalZone(t *testing.T) {
	master := string(calendarDocument("REQUEST", "DTSTART;VALUE=DATE:20270302\r\nDTEND;VALUE=DATE:20270303\r\nRRULE:FREQ=DAILY;COUNT=20"))
	master = strings.Replace(master, "BEGIN:VEVENT", "X-WR-TIMEZONE:America/New_York\r\nBEGIN:VEVENT", 1)
	st, _, edit := frozenICSFixture(t, master, "REQUEST", "RECURRENCE-ID;VALUE=DATE:20270304\r\nDTSTART;VALUE=DATE:20270306\r\nDTEND;VALUE=DATE:20270307")
	_, note := projectionFixture(t, st, "allday-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"family@example.com","startsAt":"2027-03-04T05:00:00Z"}}`)
	for _, ref := range []MeetingLinkRef{{Kind: "evt", MessageID: edit}, {Kind: "msg", MessageID: note}} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		require.NoError(t, err)
		require.Len(t, record.Links, 1)
		assert.Equal(t, edit, record.Links[0].MessageID)
		assert.Equal(t, []int64{note}, record.MessageIDs)
	}
}

func TestFrozenICSArchivedExceptions(t *testing.T) {
	for _, tc := range []struct {
		name, method, properties, original, noteTime string
		allDay, bounds, isRange                      bool
	}{
		{"range", "REQUEST", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\nDTSTART:20270306T150000Z\r\nDURATION:PT1H", "20270303T120000Z", "2027-03-03T12:00:00Z", false, true, true},
		{"timed", "REQUEST", "RECURRENCE-ID:20270304T120000Z\r\nDTSTART:20270306T150000Z\r\nDURATION:PT1H", "20270304T120000Z", "2027-03-04T12:00:00Z", false, true, false},
		{"all-day", "REQUEST", "RECURRENCE-ID;VALUE=DATE:20270304\r\nDTSTART;VALUE=DATE:20270306\r\nDTEND;VALUE=DATE:20270307", "20270304", "2027-03-04T05:00:00Z", true, true, false},
		{"untimed-cancel", "CANCEL", "RECURRENCE-ID:20270304T120000Z\r\nSTATUS:CANCELLED", "20270304T120000Z", "2027-03-04T12:00:00Z", false, false, false},
	} {
		for _, withMaster := range []bool{false, true} {
			t.Run(tc.name+"/master="+strconv.FormatBool(withMaster), func(t *testing.T) {
				st := newRFC822IDBackfillBackendStore(t)
				source := icsSource(t, st)
				if withMaster {
					props := "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=20"
					if tc.allDay {
						props = "DTSTART;VALUE=DATE:20270302\r\nDTEND;VALUE=DATE:20270303\r\nRRULE:FREQ=DAILY;COUNT=20"
					}
					master := strings.Replace(string(calendarDocument("REQUEST", props)), "BEGIN:VEVENT", "X-WR-TIMEZONE:America/New_York\r\nBEGIN:VEVENT", 1)
					_, err := importICalendarForTest(t, st, source, []byte(master))
					require.NoError(t, err)
				}
				if !tc.isRange {
					_, err := importICalendarForTest(t, st, source, calendarDocument("REQUEST", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H"))
					require.NoError(t, err)
				}
				_, err := importICalendarForTest(t, st, source, calendarDocument(tc.method, tc.properties))
				require.NoError(t, err)
				ids := emailEvents(t, st, source)
				edit := ids[len(ids)-1]
				rangeID := ids[0]
				if withMaster {
					rangeID = ids[1]
				}
				ref := MeetingLinkRef{Kind: "evt", MessageID: edit}
				expectedRef := fmt.Sprintf("evt:%d", edit)
				if withMaster {
					ref = MeetingLinkRef{Kind: "evt", MessageID: ids[0], Identity: occurrenceIdentity(t, tc.original)}
					expectedRef = fmt.Sprintf("evt:%d@%s", ids[0], tc.original)
				}
				var note int64
				if tc.noteTime != "" {
					_, note = projectionFixture(t, st, "matrix-note", "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"family@example.com","startsAt":%q}}`, tc.noteTime))
				}
				for _, seed := range []MeetingLinkRef{ref, {Kind: "evt", MessageID: edit}} {
					record, err := st.MeetingRecordContext(t.Context(), seed)
					require.NoError(t, err)
					require.Len(t, record.Links, 1)
					assert.Equal(t, expectedRef, record.Links[0].Ref)
					assert.Equal(t, tc.bounds, record.Links[0].Start != nil)
					assert.Equal(t, tc.bounds, record.Links[0].End != nil)
					assert.Contains(t, record.Unexpandable, CalendarUnexpandable{MessageID: rangeID, Reason: "unsupported recurrence: RANGE=THISANDFUTURE"})
					if note != 0 && (!tc.allDay || withMaster) {
						assert.Contains(t, record.MessageIDs, note)
					} else if note != 0 {
						assert.NotContains(t, record.MessageIDs, note)
					}
				}
				if note != 0 && (!tc.allDay || withMaster) {
					record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
					require.NoError(t, err)
					require.Len(t, record.Links, 1)
					assert.Equal(t, edit, record.Links[0].MessageID)
				}
				from := time.Date(2027, 3, 4, 0, 0, 0, 0, time.UTC)
				page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
				require.NoError(t, err)
				assert.Empty(t, page.Occurrences)
				warningID := rangeID
				if withMaster {
					warningID = ids[0]
				}
				assert.Contains(t, page.Unexpandable, CalendarUnexpandable{MessageID: warningID, Reason: "unsupported recurrence: RANGE=THISANDFUTURE"})
			})
		}
	}
}
