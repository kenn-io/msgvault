package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestParticipantsByEmailMapsSavedAndUnsaved(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	alice, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(alice)
	require.NoError(err)
	bob, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err)
	_, err = st.db.Exec(`INSERT INTO participant_identifiers (participant_id,identifier_type,identifier_value,is_primary) VALUES (?,'email','Alias@Example.com',FALSE)`, bob)
	require.NoError(err)
	got, err := st.ParticipantsByEmailContext(t.Context(), []string{" ALICE@Example.com ", "bob@example.com", "alias@example.com", "missing@example.com", ""})
	require.NoError(err)
	assert.Equal(map[string]AddressParticipant{
		"alice@example.com": {ParticipantID: alice, PersonID: person.ID},
		"bob@example.com":   {ParticipantID: bob},
		"alias@example.com": {ParticipantID: bob},
	}, got)
	// Ambiguous aliases must not pick a participant by row order.
	_, err = st.db.Exec(`INSERT INTO participant_identifiers (participant_id,identifier_type,identifier_value,is_primary) VALUES (?,'apple_id','bob@example.com',FALSE)`, alice)
	require.NoError(err)
	got, err = st.ParticipantsByEmailContext(t.Context(), []string{"bob@example.com"})
	require.NoError(err)
	assert.Empty(got)
}

func TestParticipantsByEmailRespectsSourceScope(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	alice, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err)
	hidden, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err)
	data, note := projectionFixture(t, st, "visible", "meeting_json", `{"action_items":[]}`)
	require.NoError(st.ReplaceMessageRecipients(note, "attendee", []int64{alice}, nil))
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{data.Message.SourceID})
	got, err := st.ParticipantsByEmailContext(ctx, []string{"alice@example.com", "bob@example.com"})
	require.NoError(err)
	assert.Equal(map[string]AddressParticipant{"alice@example.com": {ParticipantID: alice}}, got)
	assert.NotContains(got, "bob@example.com", hidden)
}

func TestMeetingActionItemCountsByTranscript(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, one := projectionFixture(t, st, "one", "meeting_json", `{"action_items":[{"title":"Send brief"},{"title":"Book room","status":"done"}]}`)
	_, empty := projectionFixture(t, st, "empty", "meeting_json", `{"action_items":[]}`)
	_, deleted := projectionFixture(t, st, "deleted", "meeting_json", meetingProjectionRaw)
	_, err := st.db.Exec(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`, deleted)
	require.NoError(err)
	eventData, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.org"))
	got, err := st.MeetingActionItemCountsContext(t.Context(), []int64{one, one, empty, deleted, event, 999999})
	require.NoError(err)
	assert.Equal(map[int64]int{one: 2, empty: 0}, got)
	scoped, err := st.MeetingActionItemCountsContext(sourcescope.WithSourceIDs(t.Context(), []int64{eventData.Message.SourceID}), []int64{one})
	require.NoError(err)
	assert.Empty(scoped)
	scoped, err = st.MeetingActionItemCountsContext(sourcescope.WithSourceIDs(t.Context(), []int64{data.Message.SourceID}), []int64{one, empty})
	require.NoError(err)
	assert.Equal(map[int64]int{one: 2, empty: 0}, scoped)
}
