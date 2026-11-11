package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func TestReviewCancelledOccurrenceWithoutMaster(t *testing.T) {
	for _, tc := range []struct{ name, original, eventID, master, identity string }{
		{"timed", `{"dateTime":"2027-03-14T10:00:00Z"}`, "series_20270314T100000Z", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, "20270314T100000Z"},
		{"all-day", `{"date":"2027-03-14"}`, "series_20270314", `{"id":"series","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, "20270314"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "sparse", fmt.Sprintf(`{"id":%q,"recurringEventId":"series","originalStartTime":%s,"status":"cancelled"}`, tc.eventID, tc.original), "owner@example.com")
			_, note := projectionFixture(t, st, "note", "granola_json", fmt.Sprintf(`{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":%q}}`, tc.eventID))
			for _, seed := range []MeetingLinkRef{{Kind: "msg", MessageID: note}, {Kind: "evt", MessageID: event}} {
				record, err := st.MeetingRecordContext(t.Context(), seed)
				require.NoError(err)
				require.Len(record.Links, 1)
				assert.Equal(fmt.Sprintf("evt:%d", event), record.Links[0].Ref)
				assert.Equal("cancelled", record.Links[0].Status)
				assert.Equal([]int64{note}, record.MessageIDs)
				assert.Equal([]CalendarUnexpandable{{MessageID: event, Reason: "missing series master and event bounds"}}, record.Unexpandable)
				encoded, err := json.Marshal(record.Links[0])
				require.NoError(err)
				assert.Contains(string(encoded), `"start":null`)
				assert.Contains(string(encoded), `"end":null`)
			}
			_, master := calendarFixture(t, st, "master", tc.master, "owner@example.com")
			for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: event}, {Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, tc.identity)}} {
				record, err := st.MeetingRecordContext(t.Context(), seed)
				require.NoError(err)
				require.Len(record.Links, 1)
				assert.Equal(fmt.Sprintf("evt:%d@%s", master, tc.identity), record.Links[0].Ref)
				assert.NotNil(record.Links[0].Start)
				assert.NotNil(record.Links[0].End)
				assert.Empty(record.Unexpandable)
			}
		})
	}
}

func TestMeetingRecordPartialBoundsAndAliases(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, partial := calendarFixture(t, st, "partial", `{"id":"series_20270314T100000Z","recurringEventId":"series","iCalUID":"uid@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-15T12:00:00Z"}}`, "owner@example.com")
	_, other := projectionFixture(t, st, "other-week", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-21T10:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: partial})
	require.NoError(err)
	require.Len(record.Links, 1)
	require.NotNil(record.Links[0].Start)
	assert.Equal(time.Date(2027, 3, 15, 12, 0, 0, 0, time.UTC), *record.Links[0].Start)
	assert.Nil(record.Links[0].End)
	assert.NotContains(record.MessageIDs, other)
	assert.Equal([]CalendarUnexpandable{{MessageID: partial, Reason: "missing series master and event bounds"}}, record.Unexpandable)
	keys := occurrenceKeys(CalendarOccurrence{row: CalendarEventRow{ICalUID: "uid@example.org"}})
	assert.Empty(keys)
}

func TestMeetingRecordOrphanAllDayDoesNotGuessOriginalZone(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "orphan", `{"id":"edit","recurringEventId":"series","iCalUID":"zone@example.org","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-15","timeZone":"Asia/Tokyo"},"end":{"date":"2027-03-16","timeZone":"Asia/Tokyo"}}`, "owner@example.com")
	_, unrelated := projectionFixture(t, st, "unrelated", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"zone@example.org","startsAt":"2027-03-13T15:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event})
	require.NoError(t, err)
	require.Len(t, record.Links, 1)
	assert.NotContains(t, record.MessageIDs, unrelated)
}

func TestMeetingRecordCompleteAndPartialCopies(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, sparse := calendarFixture(t, st, "sparse-copy", `{"id":"sparse","recurringEventId":"series","iCalUID":"copies@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`, "first@example.com")
	_, complete := calendarFixture(t, st, "complete-copy", `{"id":"complete","recurringEventId":"series","iCalUID":"copies@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "second@example.com")
	_, note := projectionFixture(t, st, "copies-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"copies@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
	require.NoError(err)
	require.Len(record.Links, 2)
	assert.Equal(fmt.Sprintf("evt:%d", complete), record.Links[0].Ref)
	assert.Equal(fmt.Sprintf("evt:%d", sparse), record.Links[1].Ref)
	assert.NotNil(record.Links[0].Start)
	assert.NotNil(record.Links[0].End)
	assert.Nil(record.Links[1].Start)
	assert.Nil(record.Links[1].End)
}

func TestMeetingRecordNoteLookupSharesBudget(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"event_20270314T100000Z"}}`
	projectionFixture(t, st, "first", "granola_json", raw)
	projectionFixture(t, st, "second", "granola_json", raw)
	budget := 1
	_, err := st.meetingNoteKeys(t.Context(), []string{canonicalMeetingKey("google_event_id", "owner@example.com", "event_20270314T100000Z", nil).text()}, &budget)
	assert.ErrorIs(t, err, ErrQueryTooBroad)
}

func TestMeetingRecordReportsUnsupportedSeries(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, series := calendarFixture(t, st, "unsupported-series", `{"id":"unsupported","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=YEARLY;BYWEEKNO=20"]}`)
	_, copy := calendarFixture(t, st, "supported-copy", `{"id":"copy","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	for _, ref := range []MeetingLinkRef{{Kind: "msg", MessageID: note}, {Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: series, Identity: occurrenceIdentity(t, "20270314T100000Z")}}} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		if !assert.NoError(err) {
			continue
		}
		assert.Equal([]CalendarUnexpandable{{MessageID: series, Reason: "unsupported recurrence"}}, record.Unexpandable)
		if ref.Kind == "msg" {
			require.Len(record.Links, 1)
			assert.Equal(fmt.Sprintf("evt:%d", copy), record.Links[0].Ref)
			assert.Equal([]int64{note}, record.MessageIDs)
		} else {
			assert.Empty(record.Links)
			assert.Empty(record.MessageIDs)
		}
	}
	_, supported := calendarFixture(t, st, "supported-series", `{"id":"supported","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: supported, Identity: occurrenceIdentity(t, "20270314T100000Z")}})
	require.NoError(err)
	assert.Empty(record.Unexpandable)
}

func TestMeetingRecordUnsupportedSeriesDoesNotProveOccurrence(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, series := calendarFixture(t, st, "unsupported-series", `{"id":"unsupported","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=YEARLY;BYWEEKNO=20"]}`)
	_, note := projectionFixture(t, st, "unproven-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-21T10:00:00Z"}}`)
	calendarFixture(t, st, "unproven-copy", `{"id":"copy","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-21T10:00:00Z"},"end":{"dateTime":"2027-03-21T11:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: series, Identity: occurrenceIdentity(t, "20270321T100000Z")}})
	require.NoError(err)
	assert.Equal([]CalendarUnexpandable{{MessageID: series, Reason: "unsupported recurrence"}}, record.Unexpandable)
	assert.NotContains(record.MessageIDs, note)
	assert.Empty(record.Links)
}

func TestMeetingRecordReadsNotesBeforeCalendarImport(t *testing.T) {
	for _, tc := range []struct{ name, format, first, second, other string }{
		{"twenty", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`, `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`, ""},
		{"granola", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"event_20270314T100000Z","scheduled_start_time":"2027-03-14T10:00:00Z"}}`, `{"owner":{"email":"OWNER@example.com"},"calendar_event":{"calendar_event_id":"event_20270314T100000Z","scheduled_start_time":"2027-03-14T12:00:00Z"}}`, `{"owner":{"email":"other@example.com"},"calendar_event":{"calendar_event_id":"event_20270314T100000Z"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, first := projectionFixture(t, st, "first", tc.format, tc.first)
			_, second := projectionFixture(t, st, "second", tc.format, tc.second)
			for _, id := range []int64{first, second} {
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: id})
				require.NoError(err)
				assert.Equal([]int64{first, second}, record.MessageIDs)
			}
			if tc.other != "" {
				_, other := projectionFixture(t, st, "other", tc.format, tc.other)
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: other})
				require.NoError(err)
				assert.Equal([]int64{other}, record.MessageIDs)
			}
		})
	}
}

func TestMeetingRecordSeparatesSingleEventsWithSameUID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, first := calendarFixture(t, st, "first-event", `{"id":"first","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, second := calendarFixture(t, st, "second-event", `{"id":"second","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-21T10:00:00Z"},"end":{"dateTime":"2027-03-21T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
	require.NoError(err)
	require.Len(record.Links, 1)
	assert.Equal(fmt.Sprintf("evt:%d", first), record.Links[0].Ref)
	record, err = st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: second})
	require.NoError(err)
	assert.Empty(record.MessageIDs)
}

func TestMeetingRecordOccurrenceResolution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series", `{"id":"series","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, moved := calendarFixture(t, st, "moved", `{"id":"moved","recurringEventId":"series","iCalUID":"uid@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-15T12:00:00Z"},"end":{"dateTime":"2027-03-15T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	ref := fmt.Sprintf("evt:%d@20270314T100000Z", master)
	for _, seed := range []MeetingLinkRef{{Kind: "msg", MessageID: note}, {Kind: "evt", MessageID: moved}, {Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, "20270314T100000Z")}} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		require.NoError(err)
		require.Len(record.Links, 1)
		assert.Equal(ref, record.Links[0].Ref)
		assert.Equal(time.Date(2027, 3, 15, 12, 0, 0, 0, time.UTC), *record.Links[0].Start)
		assert.Equal([]int64{note}, record.MessageIDs)
	}
	_, cancelled := calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"},"status":"cancelled"}`)
	_, cancelledNote := projectionFixture(t, st, "cancelled-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-21T10:00:00Z"}}`)
	for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: cancelled}, {Kind: "msg", MessageID: cancelledNote}} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		require.NoError(err)
		require.Len(record.Links, 1)
		assert.Equal(fmt.Sprintf("evt:%d@20270321T100000Z", master), record.Links[0].Ref)
		assert.Equal([]int64{cancelledNote}, record.MessageIDs)
	}
	_, orphan := calendarFixture(t, st, "orphan-moved", `{"id":"orphan","recurringEventId":"absent","iCalUID":"orphan@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-15T12:00:00Z"},"end":{"dateTime":"2027-03-15T13:00:00Z"}}`)
	_, orphanNote := projectionFixture(t, st, "orphan-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"orphan@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: orphan}, {Kind: "msg", MessageID: orphanNote}} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		require.NoError(err)
		require.Len(record.Links, 1)
		assert.Equal(fmt.Sprintf("evt:%d", orphan), record.Links[0].Ref)
		assert.Equal([]int64{orphanNote}, record.MessageIDs)
	}
}

func TestMeetingRecordAllDayOccurrence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series", `{"id":"series","iCalUID":"uid@example.org","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T00:00:00Z"}}`)
	ref := fmt.Sprintf("evt:%d@20270314", master)
	for _, seed := range []MeetingLinkRef{{Kind: "msg", MessageID: note}, {Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, "20270314")}} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		require.NoError(err)
		require.Len(record.Links, 1)
		assert.Equal(ref, record.Links[0].Ref)
		assert.Equal(time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC), *record.Links[0].Start)
		assert.Equal([]int64{note}, record.MessageIDs)
	}
}

func TestMeetingRecordRetainsExceptionForUnsupportedSeries(t *testing.T) {
	assertions, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, series := calendarFixture(t, st, "series", `{"id":"series","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=YEARLY;BYWEEKNO=20"]}`)
	_, exception := calendarFixture(t, st, "exception", `{"id":"exception","recurringEventId":"series","iCalUID":"uid@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-15T12:00:00Z"},"end":{"dateTime":"2027-03-15T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "unsupported-edit-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: exception}, {Kind: "evt", MessageID: series, Identity: occurrenceIdentity(t, "20270314T100000Z")}, {Kind: "msg", MessageID: note}} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		require.NoError(err)
		require.Len(record.Links, 1)
		assertions.Equal(fmt.Sprintf("evt:%d", exception), record.Links[0].Ref)
		assertions.Equal(time.Date(2027, 3, 15, 12, 0, 0, 0, time.UTC), *record.Links[0].Start)
		assertions.Equal([]int64{note}, record.MessageIDs)
		assertions.Equal([]CalendarUnexpandable{{MessageID: series, Reason: "unsupported recurrence"}}, record.Unexpandable)
	}

	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: series, Identity: occurrenceIdentity(t, "20270321T100000Z")})
	require.NoError(err)
	assertions.Empty(record.Links)
	assertions.Empty(record.MessageIDs)
	assertions.Equal([]CalendarUnexpandable{{MessageID: series, Reason: "unsupported recurrence"}}, record.Unexpandable)
}

func TestMeetingRecordUnsupportedAllDayEditOriginalZone(t *testing.T) {
	assertions, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, series := calendarFixture(t, st, "zone-series", `{"id":"zone-series","iCalUID":"zone@example.org","start":{"date":"2027-03-14","timeZone":"America/New_York"},"end":{"date":"2027-03-15","timeZone":"America/New_York"},"recurrence":["RRULE:FREQ=YEARLY;BYWEEKNO=20"]}`, "owner@example.com")
	require.NoError(st.UpdateSourceSyncConfig(t.Context(), data.Message.SourceID, `{"calendar_id":"owner@example.com","time_zone":"America/New_York"}`))
	_, edit := calendarFixture(t, st, "zone-edit", `{"id":"zone-edit","recurringEventId":"zone-series","iCalUID":"zone@example.org","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-15","timeZone":"Asia/Tokyo"},"end":{"date":"2027-03-16","timeZone":"Asia/Tokyo"}}`, "owner@example.com")
	_, note := projectionFixture(t, st, "zone-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"zone@example.org","startsAt":"2027-03-14T05:00:00Z"}}`)
	for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: edit}, {Kind: "evt", MessageID: series, Identity: occurrenceIdentity(t, "20270314")}, {Kind: "msg", MessageID: note}} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		require.NoError(err)
		require.Len(record.Links, 1)
		assertions.Equal(fmt.Sprintf("evt:%d", edit), record.Links[0].Ref)
		assertions.Equal("2027-03-14T15:00:00Z", record.Links[0].Start.UTC().Format(time.RFC3339))
		assertions.Equal([]int64{note}, record.MessageIDs)
		assertions.Equal([]CalendarUnexpandable{{MessageID: series, Reason: "unsupported recurrence"}}, record.Unexpandable)
	}
}

func TestMeetingRecordCalendarStateAndMissing(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	for _, tc := range []struct {
		name, status string
		deleted      bool
	}{
		{"confirmed", "confirmed", false},
		{"cancelled", "cancelled", false},
		{"deleted", "confirmed", true},
	} {
		_, id := calendarFixture(t, st, tc.name, fmt.Sprintf(`{"id":%q,"status":%q,"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, tc.name, tc.status))
		if tc.deleted {
			_, err := st.db.Exec(`UPDATE messages SET deleted_from_source_at = ? WHERE id = ?`, time.Now(), id)
			require.NoError(err)
		}
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: id})
		require.NoError(err)
		require.Len(record.Links, 1)
		assert.Equal(tc.status, record.Links[0].Status)
		assert.Equal(tc.deleted, record.Links[0].DeletedFromSource)
		assert.Empty(record.Unexpandable)
	}
	_, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: 999999})
	assert.ErrorIs(err, sql.ErrNoRows)
}

// Granola documents this occurrence ID in https://docs.granola.ai/api-reference/get-note.
func TestMeetingRecordScopedGoogleIDs(t *testing.T) {
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, own := calendarFixture(t, st, "own", `{"id":"2su99n6iiik37iiknmb5t4fkfh_20260127T153000Z","iCalUID":"own@example.org","start":{"dateTime":"2026-01-27T15:30:00Z"},"end":{"dateTime":"2026-01-27T16:30:00Z"}}`, "owner@example.com")
	_, shared := calendarFixture(t, st, "shared", `{"id":"2su99n6iiik37iiknmb5t4fkfh_20260127T153000Z","iCalUID":"shared@example.org","start":{"dateTime":"2026-01-27T15:30:00Z"},"end":{"dateTime":"2026-01-27T16:30:00Z"}}`, "shared@example.com")
	_, note := projectionFixture(t, st, "note", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"2su99n6iiik37iiknmb5t4fkfh_20260127T153000Z","scheduled_start_time":"2026-01-27T15:30:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
	require.NoError(err)
	require.Len(record.Links, 1)
	assert.Equal(t, fmt.Sprintf("evt:%d", own), record.Links[0].Ref)
	for _, tc := range []struct {
		id   int64
		want []int64
	}{{own, []int64{note}}, {shared, []int64{}}} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: tc.id})
		require.NoError(err)
		assert.Equal(t, tc.want, record.MessageIDs)
	}
}

func TestMeetingRecordExactFractionalAndOffsetTimes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "fractional", `{"id":"event","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00.123Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, match := projectionFixture(t, st, "match", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T06:00:00.123-04:00"}}`)
	_, other := projectionFixture(t, st, "other", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00.124Z"}}`)
	for _, seed := range []MeetingLinkRef{{Kind: "msg", MessageID: match}, {Kind: "evt", MessageID: event}} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		require.NoError(err)
		require.Len(record.Links, 1)
		assert.Equal(fmt.Sprintf("evt:%d", event), record.Links[0].Ref)
		assert.Equal([]int64{match}, record.MessageIDs)
	}
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: other})
	require.NoError(err)
	assert.Empty(record.Links)
	assert.Equal([]int64{other}, record.MessageIDs)
}

func TestMeetingRecordAmbiguousUIDKeepsNotesTogether(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, single := calendarFixture(t, st, "single", `{"id":"single","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, _ = calendarFixture(t, st, "series-ambiguous", `{"id":"series","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T09:00:00Z"},"end":{"dateTime":"2027-03-14T10:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, moved := calendarFixture(t, st, "moved-ambiguous", `{"id":"moved","recurringEventId":"series","iCalUID":"uid@example.org","originalStartTime":{"dateTime":"2027-03-14T09:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, first := projectionFixture(t, st, "first-ambiguous", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	_, second := projectionFixture(t, st, "second-ambiguous", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	for _, id := range []int64{first, second} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: id})
		require.NoError(err)
		assert.Empty(record.Links)
		assert.Equal([]int64{first, second}, record.MessageIDs)
	}
	for _, id := range []int64{single, moved} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: id})
		require.NoError(err)
		assert.Empty(record.MessageIDs)
	}
}

func TestMeetingRecordFollowsAliasesAcrossCalendarCopies(t *testing.T) {
	for _, tc := range []struct{ name, format, raw string }{
		{"granola", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"first"}}`},
		{"twenty", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00Z"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, first := calendarFixture(t, st, "first-copy", `{"id":"first","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "owner@example.com")
			_, second := calendarFixture(t, st, "second-copy", `{"id":"second","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "other@example.com")
			_, note := projectionFixture(t, st, "note-copy", tc.format, tc.raw)
			for _, seed := range []MeetingLinkRef{{Kind: "msg", MessageID: note}, {Kind: "evt", MessageID: first}, {Kind: "evt", MessageID: second}} {
				record, err := st.MeetingRecordContext(t.Context(), seed)
				require.NoError(err)
				require.Len(record.Links, 2)
				assert.Equal(fmt.Sprintf("evt:%d", first), record.Links[0].Ref)
				assert.Equal(fmt.Sprintf("evt:%d", second), record.Links[1].Ref)
				assert.Equal([]int64{note}, record.MessageIDs)
			}
		})
	}
}
func TestMeetingRecordAllDayCopiesAcrossTimeZones(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	firstData, first := calendarFixture(t, st, "ny", `{"id":"ny","iCalUID":"uid@example.org","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`, "ny@example.com")
	secondData, second := calendarFixture(t, st, "tokyo", `{"id":"tokyo","iCalUID":"uid@example.org","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`, "tokyo@example.com")
	require.NoError(st.UpdateSourceSyncConfig(t.Context(), firstData.Message.SourceID, `{"calendar_id":"ny@example.com","time_zone":"America/New_York"}`))
	require.NoError(st.UpdateSourceSyncConfig(t.Context(), secondData.Message.SourceID, `{"calendar_id":"tokyo@example.com","time_zone":"Asia/Tokyo"}`))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T05:00:00Z"}}`)
	for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: first}, {Kind: "evt", MessageID: second}, {Kind: "msg", MessageID: note}} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		require.NoError(err)
		require.Len(record.Links, 2)
		assert.Equal(fmt.Sprintf("evt:%d", second), record.Links[0].Ref)
		assert.Equal(fmt.Sprintf("evt:%d", first), record.Links[1].Ref)
		assert.Equal([]int64{note}, record.MessageIDs)
	}
}

func TestMeetingRecordAllDayMovedCopiesAcrossTimeZones(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	firstData, first := calendarFixture(t, st, "ny-series", `{"id":"ny","iCalUID":"uid@example.org","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, "ny@example.com")
	_, firstMoved := calendarFixture(t, st, "ny-moved", `{"id":"ny-moved","iCalUID":"uid@example.org","recurringEventId":"ny","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-15"},"end":{"date":"2027-03-16"}}`, "ny@example.com")
	secondData, second := calendarFixture(t, st, "tokyo-series", `{"id":"tokyo","iCalUID":"uid@example.org","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, "tokyo@example.com")
	_, secondMoved := calendarFixture(t, st, "tokyo-moved", `{"id":"tokyo-moved","iCalUID":"uid@example.org","recurringEventId":"tokyo","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-15"},"end":{"date":"2027-03-16"}}`, "tokyo@example.com")
	require.NoError(st.UpdateSourceSyncConfig(t.Context(), firstData.Message.SourceID, `{"calendar_id":"ny@example.com","time_zone":"America/New_York"}`))
	require.NoError(st.UpdateSourceSyncConfig(t.Context(), secondData.Message.SourceID, `{"calendar_id":"tokyo@example.com","time_zone":"Asia/Tokyo"}`))
	_, note := projectionFixture(t, st, "moved-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T05:00:00Z"}}`)
	for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: first, Identity: occurrenceIdentity(t, "20270314")}, {Kind: "evt", MessageID: second, Identity: occurrenceIdentity(t, "20270314")}, {Kind: "evt", MessageID: firstMoved}, {Kind: "evt", MessageID: secondMoved}, {Kind: "msg", MessageID: note}} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		require.NoError(err)
		require.Len(record.Links, 2)
		assert.Equal(fmt.Sprintf("evt:%d@20270314", second), record.Links[0].Ref)
		assert.Equal(fmt.Sprintf("evt:%d@20270314", first), record.Links[1].Ref)
		assert.Equal([]int64{note}, record.MessageIDs)
	}
	next, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: first, Identity: occurrenceIdentity(t, "20270321")})
	require.NoError(err)
	require.Len(next.Links, 2)
	assert.Empty(next.MessageIDs)
}

func TestMeetingRecordSkipsUnrelatedExceptionsWithinBudget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, master := calendarFixture(t, st, "master-budget", `{"id":"cap-series","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	bulkCalendarCandidates(t, st, data.Message.SourceID, data.Message.ConversationID, "exception", 150)
	_, err := st.db.Exec(`UPDATE calendar_events SET ical_uid = 'uid@example.org' WHERE kind = 'exception'`)
	require.NoError(err)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	require.NoError(err)
	defer release()
	at := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	key := canonicalMeetingKey("ical_uid", "", "uid@example.org", &at)
	budget := 100
	var unexpandable []CalendarUnexpandable
	matches, err := st.meetingOccurrencesForKeys(ctx, []meetingLinkKey{key}, &budget, &unexpandable)
	require.NoError(err)
	require.Len(matches[key], 1)
	assert.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), matches[key][0].Ref)
}

func TestMeetingRecordSeparatesSeriesMasterNotes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "series-master", `{"id":"series","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, "owner@example.com")
	_, first := projectionFixture(t, st, "week-one", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"series","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
	_, second := projectionFixture(t, st, "week-two", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"series","scheduled_start_time":"2027-03-21T10:00:00Z"}}`)
	for _, id := range []int64{first, second} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: id})
		require.NoError(err)
		assert.Equal([]int64{id}, record.MessageIDs)
		assert.Empty(record.Links)
	}
	for _, key := range []string{"20270314T100000Z", "20270321T100000Z"} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, key)})
		require.NoError(err)
		require.Len(record.Links, 1)
		assert.Equal(fmt.Sprintf("evt:%d@%s", master, key), record.Links[0].Ref)
		assert.Empty(record.MessageIDs)
	}
}

func occurrenceIdentity(t *testing.T, key string) calrecur.Identity {
	t.Helper()
	_, identity, ok := calrecur.ParseInstanceID("event_" + key)
	require.True(t, ok)
	return identity
}

func TestMeetingRecordBareGoogleIDBeforeAndAfterSingle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, first := projectionFixture(t, st, "first-bare", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"event","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
	_, second := projectionFixture(t, st, "second-bare", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"event","scheduled_start_time":"2027-03-14T11:00:00Z"}}`)
	for _, id := range []int64{first, second} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: id})
		require.NoError(err)
		assert.Equal([]int64{id}, record.MessageIDs)
		assert.Empty(record.Links)
	}
	_, event := calendarFixture(t, st, "single-bare", `{"id":"event","start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, "owner@example.com")
	for _, seed := range []MeetingLinkRef{{Kind: "msg", MessageID: first}, {Kind: "msg", MessageID: second}, {Kind: "evt", MessageID: event}} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		require.NoError(err)
		require.Len(record.Links, 1)
		assert.Equal(fmt.Sprintf("evt:%d", event), record.Links[0].Ref)
		assert.Equal([]int64{first, second}, record.MessageIDs)
	}
	calendarFixture(t, st, "duplicate-bare", `{"id":"event","start":{"dateTime":"2027-03-21T12:00:00Z"},"end":{"dateTime":"2027-03-21T13:00:00Z"}}`, "owner@example.com")
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: first})
	require.NoError(err)
	assert.Equal([]int64{first}, record.MessageIDs)
	assert.Empty(record.Links)
}

func TestMeetingRecordAllDayCancellationAndOriginalZone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, master := calendarFixture(t, st, "all-day-master", `{"id":"series","iCalUID":"zone@example.org","start":{"date":"2027-03-14","timeZone":"America/New_York"},"end":{"date":"2027-03-15","timeZone":"America/New_York"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, "owner@example.com")
	require.NoError(st.UpdateSourceSyncConfig(t.Context(), data.Message.SourceID, `{"calendar_id":"owner@example.com","time_zone":"America/New_York"}`))
	_, cancelled := calendarFixture(t, st, "all-day-cancelled", `{"id":"cancelled","recurringEventId":"series","originalStartTime":{"date":"2027-03-14"},"status":"cancelled"}`, "owner@example.com")
	for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: cancelled}, {Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, "20270314")}} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		require.NoError(err)
		require.Len(record.Links, 1)
		assert.Equal(fmt.Sprintf("evt:%d@20270314", master), record.Links[0].Ref)
	}
	_, orphan := calendarFixture(t, st, "orphan-cancelled", `{"id":"orphan","recurringEventId":"missing","originalStartTime":{"date":"2027-03-14"},"status":"cancelled"}`, "owner@example.com")
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: orphan})
	require.NoError(err)
	require.Len(record.Links, 1)
	assert.Equal(fmt.Sprintf("evt:%d", orphan), record.Links[0].Ref)
	assert.Nil(record.Links[0].Start)
	_, moved := calendarFixture(t, st, "all-day-moved", `{"id":"moved","recurringEventId":"series","iCalUID":"zone@example.org","originalStartTime":{"date":"2027-03-21"},"start":{"date":"2027-03-22","timeZone":"Asia/Tokyo"},"end":{"date":"2027-03-23","timeZone":"Asia/Tokyo"}}`, "owner@example.com")
	_, note := projectionFixture(t, st, "original-zone-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"zone@example.org","startsAt":"2027-03-21T04:00:00Z"}}`)
	for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: moved}, {Kind: "msg", MessageID: note}} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		require.NoError(err)
		require.Len(record.Links, 1)
		assert.Equal(fmt.Sprintf("evt:%d@20270321", master), record.Links[0].Ref)
		assert.Equal([]int64{note}, record.MessageIDs)
	}
}

func TestUniqueMeetingOccurrencesOrder(t *testing.T) {
	start := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	items := []CalendarOccurrence{{Ref: "evt:10", MessageID: 10, Start: start}, {Ref: "evt:9", MessageID: 9, Start: start}, {Ref: "evt:11", MessageID: 11, Start: start.Add(-time.Hour)}, {Ref: "evt:9", MessageID: 9, Start: start}, {Ref: "evt:1", MessageID: 1}}
	got := uniqueMeetingOccurrences(items)
	require.Len(t, got, 4)
	assert.Equal(t, []string{"evt:11", "evt:9", "evt:10", "evt:1"}, []string{got[0].Ref, got[1].Ref, got[2].Ref, got[3].Ref})
}
