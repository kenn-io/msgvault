package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingRecordFromNoteIncludesOtherNotes(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "shared@example.org"))
	_, a := projectionFixture(t, st, "a", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org"}}`)
	_, b := projectionFixture(t, st, "b", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org"}}`)
	for _, ref := range []MeetingLinkRef{{Kind: "evt", MessageID: event}, {Kind: "msg", MessageID: a}, {Kind: "msg", MessageID: b}} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		require.NoError(t, err)
		assert.Equal(t, []int64{a, b}, record.MessageIDs)
	}
}

func TestMeetingAutomaticLinkSurvivesCalendarSourceMerge(t *testing.T) {
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	history, event := calendarFixture(t, st, "event", singleLinkRaw("event", "merge@example.org"))
	note := granolaLinkFixture(t, st, "event")
	destination, err := st.GetOrCreateSource("gcal", "destination@example.com")
	require.NoError(err)
	require.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
	_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: history.Message.SourceID, IntoSourceID: destination.ID})
	require.NoError(err)
	assert.Equal(t, fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
}

func TestMeetingRecordBoundsRecurringExceptions(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	raw := strings.ReplaceAll(strings.ReplaceAll(linkSeriesRaw, "2027-03-07", "2026-01-04"), "FREQ=WEEKLY;COUNT=6", "FREQ=DAILY;COUNT=1000")
	_, master := calendarFixture(t, st, "master", raw)
	for i := range 201 {
		at := time.Date(2026, 1, 4, 10, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		calendarFixture(t, st, fmt.Sprintf("old-%d", i), fmt.Sprintf(`{"id":"old-%d","iCalUID":"weekly@example.org","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":%q}}`, i, at.Format(time.RFC3339)))
	}
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Key: "20270314T100000Z"})
	require.NoError(t, err)
	assert.Equal(t, []int64{note}, record.MessageIDs)
}

func TestMeetingManualLinkAddsAutomaticLink(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, automatic := calendarFixture(t, st, "automatic", singleLinkRaw("automatic", "automatic@example.org"))
	_, manual := calendarFixture(t, st, "manual", singleLinkRaw("manual", "manual@example.org"))
	note := granolaLinkFixture(t, st, "automatic")
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: manual}))
	links, err := meetingLinksForTest(t, st, note)
	require.NoError(err)
	require.Len(links, 2)
	assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: automatic}), links[0].Ref)
	assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: manual}), links[1].Ref)
	require.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: automatic}))
	links, err = meetingLinksForTest(t, st, note)
	require.NoError(err)
	require.Len(links, 1)
	assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: manual}), links[0].Ref)
}

func TestMeetingSplitBeforeMasterArrives(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, exception := calendarFixture(t, st, "exception", `{"id":"series_20270314T100000Z","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	require.NoError(t, st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: exception}))
	calendarFixture(t, st, "master", linkSeriesRaw)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
	require.NoError(t, err)
	assert.Empty(t, record.Links)
}

func TestMeetingRecordIgnoresOtherRecurringDates(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := strings.ReplaceAll(strings.ReplaceAll(linkSeriesRaw, "2027-03-07", "2024-01-01"), "FREQ=WEEKLY;COUNT=6", "FREQ=DAILY;COUNT=1500")
	_, master := calendarFixture(t, st, "master", raw)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2024-01-01T10:00:00Z"}}`)
	start := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	for i := 1; i < 1100; i++ {
		at := start.AddDate(0, 0, i).Format(time.RFC3339)
		projectionFixture(t, st, fmt.Sprintf("daily-%d", i), "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":%q}}`, at))
	}
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Key: "20240101T100000Z"})
	requirements.NoError(err)
	assertions.Equal([]int64{note}, record.MessageIDs)
}
