package meetingarchive

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestArchiverExcludesRooms(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("meeting_import", "rooms@example.org")
	requirements.NoError(err)
	snapshot := testSnapshot(source.ID)
	snapshot.Organizer = &Person{Email: "room@c.resource.calendar.google.com", Name: "Room", Anchor: "test:room"}
	snapshot.Attendees = []Person{
		{Email: "room@c.resource.calendar.google.com", Name: "Room", Anchor: "test:room"},
		{Email: "human@example.org", Name: "Human", OtherEmails: []string{"alias@example.org"}, Anchor: "test:human"},
	}
	archiver := New(st)
	_, err = archiver.LinkIdentities(t.Context(), source.ID, append(snapshot.Attendees, *snapshot.Organizer))
	requirements.NoError(err)
	result, err := archiver.Upsert(t.Context(), snapshot, UpsertOptions{})
	requirements.NoError(err)
	var count int
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM participants WHERE email_address LIKE '%resource.calendar.google.com'`).Scan(&count))
	assertions.Zero(count)
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`), result.MessageID).Scan(&count))
	assertions.Equal(1, count)
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM conversation_participants`).Scan(&count))
	assertions.Equal(1, count, "only the human joins the conversation roster")
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM messages WHERE id = ? AND sender_id IS NOT NULL`), result.MessageID).Scan(&count))
	assertions.Zero(count)
}

func TestArchiverExcludesRoomsKeepsOtherIdentities(t *testing.T) {
	room := "room@c.resource.calendar.google.com"
	for _, tc := range []struct {
		name   string
		person Person
		want   Person
	}{
		{"primary room with phone", Person{Email: room, Phone: "+15550100001"}, Person{Phone: "+15550100001"}},
		{"secondary room", Person{Email: "human@example.org", OtherEmails: []string{room, "alias@example.org"}}, Person{Email: "human@example.org", OtherEmails: []string{"alias@example.org"}}},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, tc.person.Normalized()) })
	}
}
