package granola

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/store"
)

// TestImportedNoteReachesExistingPerson pins the contract that matters to a
// user: an attendee whose email already belongs to a person gets the meeting
// on that person after activity projection, with no name matching.
func TestImportedNoteReachesExistingPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	api := &fakeAPI{notes: map[string][]byte{"not_Ab12Cd34Ef56Gh": loadFixture(t, "note_full.json")}}
	imp, st := newTestImporter(t, api)
	participantID, err := st.EnsureParticipant("carol@example.com", "Carol", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(err)

	_, err = imp.Import(t.Context(), ImportOptions{Identifier: "alice@example.com", AccountEmail: "alice@example.com"})
	require.NoError(err)
	projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)

	var role string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT aep.role FROM activity_event_persons aep
		JOIN messages m ON m.id = aep.message_id
		WHERE m.source_message_id = ? AND aep.person_id = ?`),
		"not_Ab12Cd34Ef56Gh", person.ID).Scan(&role))
	assert.Equal(string(store.RoleAttendee), role)
}
