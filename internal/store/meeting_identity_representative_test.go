package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingIdentityEachNoteKeepsScheduledCopy(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, early := calendarFixture(t, st, "early", singleLinkRaw("early", "meeting@example.org"))
	_, late := calendarFixture(t, st, "late", strings.NewReplacer("T10:", "T12:", "T11:", "T13:").Replace(singleLinkRaw("late", "meeting@example.org")))
	_, earlyNote := identityNote(t, st, "early-note", "meeting@example.org")
	_, lateNote := projectionFixture(t, st, "late-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"meeting@example.org","startsAt":"2027-03-14T12:00:00Z"}}`)
	earlyRef, lateRef := fmt.Sprintf("evt:%d", early), fmt.Sprintf("evt:%d", late)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: lateNote})
	require.NoError(err)
	require.Len(record.Links, 1)
	require.Equal(lateRef, record.Links[0].Ref)
	for _, refs := range [][]string{nil, {earlyRef, lateRef}} {
		got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs, TranscriptIDs: []int64{lateNote}})
		require.NoError(err)
		assert.Equal(lateRef, got.ByTranscript[lateNote].OccurrenceRef)
		assert.Equal([]string{lateRef}, got.ByTranscript[lateNote].ExactOccurrenceRefs)
		assert.Equal(earlyRef, got.ByTranscript[earlyNote].OccurrenceRef)
		assert.Equal([]string{earlyRef}, got.ByTranscript[earlyNote].ExactOccurrenceRefs)
		assert.Equal("occ:"+earlyRef, got.ByTranscript[lateNote].Key)
		assert.Equal(got.ByTranscript[earlyNote].Key, got.ByTranscript[lateNote].Key)
		assert.Equal([]int64{earlyNote, lateNote}, got.ByTranscript[lateNote].LinkedTranscriptIDs)
	}
}
