package muesli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/store"
)

// TestImportedMeetingReachesExistingPerson proves the end-to-end contract a
// user relies on: a Muesli attendee whose email already belongs to a person
// shows up as that person's meeting after activity projection, without any
// name matching.
func TestImportedMeetingReachesExistingPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	ctx := t.Context()

	participantID, err := f.st.EnsureParticipant("alice@example.com", "Alice Example", "example.com")
	require.NoError(err)
	person, _, err := f.st.CreatePersonFromParticipantContext(ctx, participantID)
	require.NoError(err)

	meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	insertRow(t, f.muesli, "meeting_participants", map[string]any{
		"meeting_id": meetingID, "participant_identifier": "email:alice@example.com",
		"display_name": "Alice (Contacts)", "email_address": "ALICE@example.com",
		"insertion_order": 0, "source": "manual",
	})
	_, err = f.run(t, ImportOptions{})
	require.NoError(err)

	projector, err := activity.NewProjector(f.st, activity.Options{
		Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25,
	})
	require.NoError(err)
	_, err = projector.RunOnce(ctx)
	require.NoError(err)

	var role, refKind string
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(`
		SELECT aep.role, ae.ref_kind
		FROM activity_event_persons aep
		JOIN activity_events ae ON ae.message_id = aep.message_id
		JOIN messages m ON m.id = aep.message_id
		WHERE m.source_id = ? AND aep.person_id = ?`),
		f.source.ID, person.ID).Scan(&role, &refKind))
	assert.Equal(string(store.RoleAttendee), role)
	assert.Equal(string(store.RefKindMeeting), refKind)
}
