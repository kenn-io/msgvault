package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingRecordIncludesNotesOnMovedCopy(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	calendarFixture(t, st, "copy", linkSeriesRaw, "copy@example.com")
	_, exception := calendarFixture(t, st, "moved", `{"id":"series_20270314T100000Z","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-15T12:00:00Z"},"end":{"dateTime":"2027-03-15T13:00:00Z"}}`, "copy@example.com")
	var notes []int64
	for i, start := range []string{"2027-03-14T10:00:00Z", "2027-03-15T12:00:00Z"} {
		_, note := projectionFixture(t, st, fmt.Sprintf("note-%d", i), "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":%q}}`, start))
		notes = append(notes, note)
		links, err := meetingLinksForTest(t, st, note)
		requirements.NoError(err)
		requirements.Len(links, 1)
	}
	for _, ref := range []OccurrenceRef{{MessageID: master, Key: "20270314T100000Z"}, {MessageID: exception}} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", OccurrenceRef: ref})
		requirements.NoError(err)
		assert.Equal(t, notes, record.MessageIDs)
	}
}

func TestMeetingRecordIncludesOrphanDecisionThroughSeriesCopy(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, exception := calendarFixture(t, st, "orphan", `{"id":"series_20270314T100000Z","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: exception}))
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	_, copyID := calendarFixture(t, st, "copy", linkSeriesRaw, "copy@example.com")

	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
	require.NoError(err)
	require.Len(record.Links, 1)
	assert.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), record.Links[0].Ref)
	assert.Equal([]int64{note}, record.MessageIDs)

	record, err = st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: copyID, Key: "20270314T100000Z"})
	require.NoError(err)
	assert.Equal([]int64{note}, record.MessageIDs)
}

func TestMeetingRecordAllDayCopiesShareNotesAcrossCalendarZones(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"series","iCalUID":"all-day@example.org","start":{"date":"2027-03-07"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`
	_, utc := calendarFixture(t, st, "utc", raw)
	tokyoData, tokyo := calendarFixture(t, st, "tokyo", raw, "copy@example.com")
	requirements.NoError(st.UpdateSourceSyncConfig(tokyoData.Message.SourceID, `{"calendar_id":"copy@example.com","time_zone":"Asia/Tokyo"}`))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"all-day@example.org","startsAt":"2027-03-14T00:00:00Z"}}`)
	for _, id := range []int64{utc, tokyo} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: id, Key: "20270314"})
		requirements.NoError(err)
		assert.Equal(t, []int64{note}, record.MessageIDs, "calendar copy %d", id)
	}
}

func TestMeetingRecordRecurringGoogleEvidence(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	projectionFixture(t, st, "other-note", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"series_20270321T100000Z","scheduled_start_time":"2027-03-21T10:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Key: "20270314T100000Z"})
	require.NoError(err)
	assert.Equal([]int64{note}, record.MessageIDs)
}

func TestMeetingDecisionsFollowMessageIdentity(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "old@example.org"))
	_, copyID := calendarFixture(t, st, "copy", singleLinkRaw("copy", "old@example.org"), "second@example.com")
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: copyID}))
	var count int
	require.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&count))
	assert.Equal(1, count)
	calendarFixture(t, st, "copy", strings.ReplaceAll(singleLinkRaw("copy", "old@example.org"), "old@example.org", "new@example.org"), "second@example.com")
	require.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: copyID}))
	links, err := meetingLinksForTest(t, st, note)
	require.NoError(err)
	assert.Empty(links)
}

func TestMeetingDecisionReplacesUnexpandableCopy(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, automatic := calendarFixture(t, st, "automatic", singleLinkRaw("automatic", "automatic@example.org"))
	_, first := calendarFixture(t, st, "first", linkSeriesRaw)
	_, second := calendarFixture(t, st, "second", linkSeriesRaw, "second@example.com")
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"automatic@example.org"}}`)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: first, Key: "20270314T100000Z"}))
	require.NoError(st.UpsertMessageRawWithFormat(first, []byte(strings.Replace(linkSeriesRaw, "FREQ=WEEKLY", "FREQ=HOURLY", 1)), "gcal_json"))
	require.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: second, Key: "20270314T100000Z"}))
	require.NoError(st.UpsertMessageRawWithFormat(first, []byte(linkSeriesRaw), "gcal_json"))
	links, err := meetingLinksForTest(t, st, note)
	require.NoError(err)
	require.Len(links, 1)
	assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: automatic}), links[0].Ref)
}

func TestMeetingUnlinkReadableCopyAfterSeriesExceedsBudget(t *testing.T) {
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, first := calendarFixture(t, st, "first", linkSeriesRaw)
	_, second := calendarFixture(t, st, "second", linkSeriesRaw, "second@example.com")
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: first, Key: "20270314T100000Z"}))
	calendarFixture(t, st, "first", `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"1800-01-01T10:00:00Z"},"end":{"dateTime":"1800-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`)
	require.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: second, Key: "20270314T100000Z"}))
	calendarFixture(t, st, "first", linkSeriesRaw)
	links, err := meetingLinksForTest(t, st, note)
	require.NoError(err)
	assert.Empty(t, links)
}

func TestMeetingRecordNotesAndCopies(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"automatic notes", func(t *testing.T) {
			t.Helper()
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "shared@example.org"))
			_, a := projectionFixture(t, st, "a", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org"}}`)
			_, b := projectionFixture(t, st, "b", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org"}}`)
			for _, ref := range []MeetingLinkRef{{Kind: "evt", MessageID: event}, {Kind: "msg", MessageID: a}, {Kind: "msg", MessageID: b}} {
				record, err := st.MeetingRecordContext(t.Context(), ref)
				require.NoError(t, err)
				assert.Equal(t, []int64{a, b}, record.MessageIDs)
			}
		}},
		{"decisions and copies", func(t *testing.T) {
			t.Helper()
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "record@example.org"))
			_, copyID := calendarFixture(t, st, "copy", singleLinkRaw("copy", "record@example.org"), "second@example.com")
			_, automatic := projectionFixture(t, st, "automatic", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"record@example.org"}}`)
			_, manual := projectionFixture(t, st, "manual", "plaud_json", `{}`)
			_, excluded := projectionFixture(t, st, "excluded", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"record@example.org"}}`)
			require.NoError(st.LinkMeetingContext(t.Context(), manual, OccurrenceRef{MessageID: copyID}))
			require.NoError(st.UnlinkMeetingContext(t.Context(), excluded, OccurrenceRef{MessageID: copyID}))
			for _, id := range []int64{event, copyID} {
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: id})
				require.NoError(err)
				assert.Equal([]int64{automatic, manual}, record.MessageIDs)
				require.Len(record.Links, 1)
				assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: id}), record.Links[0].Ref)
			}
			record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: automatic})
			require.NoError(err)
			assert.Equal([]int64{automatic, manual}, record.MessageIDs)
			require.Len(record.Links, 1)
			assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: event}), record.Links[0].Ref)
		}},
	} {
		t.Run(tc.name, tc.run)
	}
}
