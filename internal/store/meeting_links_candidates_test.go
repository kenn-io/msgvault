package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemberCandidatesMultipleManualLinks(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, one := calendarFixture(t, st, "one", singleLinkRaw("one", "one@example.org"))
	_, two := calendarFixture(t, st, "two", singleLinkRaw("two", "two@example.org"))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{}}`)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: one}))
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: two}))
	links, err := st.MeetingLinksContext(t.Context(), note)
	require.NoError(err)
	assert.Len(links, 2)
}
