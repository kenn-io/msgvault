package store

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReview1252UIDMovedSingleCopy(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := singleLinkRaw("old", "moved@example.org")
	_, old := calendarFixture(t, st, "old", raw)
	_, moved := calendarFixture(t, st, "moved", strings.NewReplacer("old", "moved", "10:00:00", "12:00:00", "11:00:00", "13:00:00").Replace(raw))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"moved@example.org","startsAt":"2027-03-14T12:00:00Z"}}`)
	links, err := st.MeetingLinksContext(t.Context(), note)
	require.NoError(err)
	require.Len(links, 1)
	assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: moved}), links[0].Ref)
	assert.Equal(time.Date(2027, 3, 14, 12, 0, 0, 0, time.UTC), links[0].Start)
	require.Less(old, moved)
}

func TestMeetingCopyPrecedenceFallback(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, first := calendarFixture(t, st, "first", singleLinkRaw("first", "copies@example.org"))
	calendarFixture(t, st, "second", singleLinkRaw("second", "copies@example.org"), "second@example.com")
	for _, scheduled := range []string{"", "2027-03-14T15:00:00Z"} {
		_, note := projectionFixture(t, st, "note"+scheduled, "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"copies@example.org","startsAt":"`+scheduled+`"}}`)
		links, err := st.MeetingLinksContext(t.Context(), note)
		require.NoError(err)
		require.Len(links, 1)
		assert.Equal(formatMeetingOccurrence(OccurrenceRef{MessageID: first}), links[0].Ref)
	}
}
