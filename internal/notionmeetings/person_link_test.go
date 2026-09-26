package notionmeetings

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

func boundPersonForEmail(t *testing.T, st *store.Store, email string) int64 {
	t.Helper()
	var personID int64
	err := st.DB().QueryRow(st.Rebind(`
		SELECT pp.person_id FROM person_participants pp
		JOIN participants p ON p.id = pp.participant_id
		WHERE p.email_address = ?`), email).Scan(&personID)
	if err != nil {
		return 0
	}
	return personID
}

func TestHydratorAnchorsOnlyVerifiedNotionUsers(t *testing.T) {
	require := require.New(t)
	hydrated, err := NewHydrator(completeHydrationSource()).Hydrate(context.Background(), hydrationMeeting())
	require.NoError(err)

	require.Len(hydrated.Attendees, 1)
	assert.Equal(t, meetingarchive.Anchor("notion-user", "user-1"), hydrated.Attendees[0].Anchor)
}

func TestNotionUserKeepsPersonAcrossEmailChange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	_, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	participantID, err := st.EnsureParticipant("attendee@example.com", "", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(err)

	source.users[""].Results[0].Person.Email = "attendee.new@example.com"
	_, err = imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)

	assert.Equal(person.ID, boundPersonForEmail(t, st, "attendee.new@example.com"),
		"the same Notion user under a new email joins the person")
}

func TestNotionChecksumSkipStillLinksAttendees(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, imp := newImporterFixture(t)
	_, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	// Simulate linking that never completed after the meeting was written.
	_, err = st.DB().Exec(`DELETE FROM participant_contact_observations`)
	require.NoError(err)

	second, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(0), second.MeetingsUpdated, "the unchanged meeting is not rewritten")

	var observed int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT count(*) FROM participant_contact_observations WHERE provider_user_id = ?`),
		meetingarchive.Anchor("notion-user", "user-1")).Scan(&observed))
	assert.Equal(1, observed)
}
