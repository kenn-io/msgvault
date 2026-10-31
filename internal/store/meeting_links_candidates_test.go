package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func TestMemberCandidatesAutomaticAmbiguity(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "series", linkSeriesRaw)
	calendarFixture(t, st, "single", singleLinkRaw("single", "weekly@example.org"))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	budget := calrecur.RequestReadBudget
	candidates, err := st.meetingMemberCandidates(t.Context(), st.meetingReader(t.Context()), note, &budget)
	require.NoError(t, err)
	require.Len(t, candidates, 2)
	for _, c := range candidates {
		assert.Equal(t, "automatic", c.Provenance)
	}
	assert.NotEqual(t, candidates[0].Identity, candidates[1].Identity)
	assert.Empty(t, linkedMeetingRefs(t, st, note)[note])
}

func TestMemberCandidatesMultipleManualLinks(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, one := calendarFixture(t, st, "one", singleLinkRaw("one", "one@example.org"))
	_, two := calendarFixture(t, st, "two", singleLinkRaw("two", "two@example.org"))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{}}`)
	require.NoError(t, st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: one}))
	require.NoError(t, st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: two}))
	budget := calrecur.RequestReadBudget
	candidates, err := st.meetingMemberCandidates(t.Context(), st.meetingReader(t.Context()), note, &budget)
	require.NoError(t, err)
	require.Len(t, candidates, 2)
	for _, c := range candidates {
		assert.Equal(t, "manual", c.Provenance)
	}
	links, err := st.MeetingLinksContext(t.Context(), note)
	require.NoError(t, err)
	assert.Len(t, links, 2)
}
