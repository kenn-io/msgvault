package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingRecordNotesAndCopies(t *testing.T) {
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
	assert.Equal([]int64{automatic}, record.MessageIDs)
	require.Len(record.Links, 1)
	assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: event}), record.Links[0].Ref)
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
	data, _ := calendarFixture(t, st, "copy", strings.ReplaceAll(singleLinkRaw("copy", "old@example.org"), "old@example.org", "new@example.org"), "second@example.com")
	require.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: copyID}))
	links, err := st.MeetingLinksContext(t.Context(), note)
	require.NoError(err)
	assert.Empty(links)
	require.NoError(st.RemoveSource(data.Message.SourceID))
	require.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&count))
	assert.Zero(count)
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
	links, err := st.MeetingLinksContext(t.Context(), note)
	require.NoError(err)
	require.Len(links, 1)
	assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: automatic}), links[0].Ref)
}
