package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingSparseCancelledMasterPreservesExceptionUID(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "exception", `{"id":"series_20270314T100000Z","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, master := calendarFixture(t, st, "master", `{"id":"series","status":"cancelled"}`)
	calendarFixture(t, st, "copy", linkSeriesRaw, "copy@example.com")
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	links, err := meetingLinksForTest(t, st, note)
	require.NoError(err)
	require.Len(links, 1)
	assert.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), links[0].Ref)
}

func TestMeetingSparseCancelledExceptionPreservesRecordUID(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", strings.Replace(linkSeriesRaw, `"iCalUID":"weekly@example.org",`, "", 1))
	calendarFixture(t, st, "exception", `{"id":"series_20270314T100000Z","iCalUID":"weekly@example.org","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Key: "20270314T100000Z"})
	require.NoError(t, err)
	assert.Equal(t, []int64{note}, record.MessageIDs)
}

func TestMeetingDecisionUsesResolvedOrphanIdentity(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, orphan := calendarFixture(t, st, "orphan", `{"id":"series_20270314T100000Z","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan}))
	_, master := calendarFixture(t, st, "master", strings.ReplaceAll(linkSeriesRaw, "weekly@example.org", "changed@example.org"))
	_, other := calendarFixture(t, st, "other", linkSeriesRaw, "other@example.com")
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: other, Key: "20270314T100000Z"}))
	links, err := meetingLinksForTest(t, st, note)
	require.NoError(err)
	require.Len(links, 2)
	assert.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), links[0].Ref)
	assert.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", other), links[1].Ref)
}

func TestMeetingGranolaRetainedExceptionAfterRecurrenceRemoval(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	calendarFixture(t, st, "exception", `{"id":"series_20270314T100000Z","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	want := fmt.Sprintf("evt:%d@20270314T100000Z", master)
	assert.Equal(want, linkedMeetingRefs(t, st, note)[note])
	require.NoError(st.UpsertMessageRawWithFormat(master, []byte(singleLinkRaw("series", "weekly@example.org")), "gcal_json"))
	assert.Equal(want, linkedMeetingRefs(t, st, note)[note])
}

func TestMeetingRecordOrphanDecisionAfterRecurrenceRemoval(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, orphan := calendarFixture(t, st, "orphan", `{"id":"series_20270314T100000Z","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan}))
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	_, copyID := calendarFixture(t, st, "copy", linkSeriesRaw, "copy@example.com")
	require.NoError(st.UpsertMessageRawWithFormat(master, []byte(singleLinkRaw("series", "weekly@example.org")), "gcal_json"))
	assert.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), linkedMeetingRefs(t, st, note)[note])
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: copyID, Key: "20270314T100000Z"})
	require.NoError(err)
	assert.Equal([]int64{note}, record.MessageIDs)
}
