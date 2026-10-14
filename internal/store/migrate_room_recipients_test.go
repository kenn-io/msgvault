package store

import (
	"database/sql"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoomRecipientMigrationTwiceNoop(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	event, source := calendarLinkFixture(t, st, "primary@example.com", `{"id":"room-event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"resource@example.com","resource":true}]}`)
	room, err := st.EnsureParticipant("resource@example.com", "Example Room", "example.com")
	requirements.NoError(err)
	human, err := st.EnsureParticipant("human@example.org", "Example Human", "example.org")
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(event, "to", []int64{room, human}, []string{"Example Room", "Example Human"}))
	requirements.NoError(st.ReplaceMessageRecipients(event, "from", []int64{room}, []string{"Example Room"}))
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET sender_id=? WHERE id=?`), room, event)
	requirements.NoError(err)
	ordinary, err := st.PersistMessage(&MessagePersistData{Message: &Message{SourceID: source, SourceMessageID: "ordinary", MessageType: "email", SenderID: sql.NullInt64{Int64: human, Valid: true}}, Conversation: &ConversationPersistData{SourceConversationID: "ordinary", ConversationType: "email"}})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(ordinary, "to", []int64{room}, []string{"Example Room"}))
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM applied_migrations WHERE name=?`), migrationRoomRecipientsV1)
	requirements.NoError(err)
	requirements.NoError(st.InitSchema())
	var count int64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM message_recipients WHERE message_id=? AND participant_id=?`), event, room).Scan(&count))
	assertions.Zero(count)
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM message_recipients WHERE message_id=? AND participant_id=?`), ordinary, room).Scan(&count))
	assertions.Equal(int64(1), count)
	var sender sql.NullInt64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT sender_id FROM messages WHERE id=?`), event).Scan(&sender))
	assertions.False(sender.Valid)
	before, err := st.DerivedDataRevision()
	requirements.NoError(err)
	requirements.NoError(st.InitSchema())
	after, err := st.DerivedDataRevision()
	requirements.NoError(err)
	assertions.Equal(before, after)
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM calendar_event_attendees WHERE message_id=? AND role='room'`), event).Scan(&count))
	assertions.Equal(int64(1), count)
}

func TestRoomRecipientMigrationPreservesMixedParticipantHumanEdges(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(strconv.FormatBool(legacy), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st := newRFC822IDBackfillBackendStore(t)
			event, _ := calendarLinkFixture(t, st, "primary@example.org", `{"id":"human-event","organizer":{"email":"human@example.org"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"human@example.org"}]}`)
			primary, alias := "human@example.org", "room@resource.calendar.google.com"
			if legacy {
				primary, alias = alias, primary
			}
			mixed, err := st.EnsureParticipant(primary, "Example Mixed Contact", "example.org")
			requirements.NoError(err)
			requirements.NoError(st.SetParticipantIdentifier(mixed, "email", alias))
			requirements.NoError(st.ReplaceMessageRecipients(event, "to", []int64{mixed}, []string{"Example Mixed Contact"}))
			requirements.NoError(st.ReplaceMessageRecipients(event, "from", []int64{mixed}, []string{"Example Mixed Contact"}))
			if legacy {
				_, err = st.DB().Exec(st.Rebind(`UPDATE message_recipients SET email_address='' WHERE message_id=?`), event)
				requirements.NoError(err)
			}
			_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET sender_id=? WHERE id=?`), mixed, event)
			requirements.NoError(err)
			_, err = st.DB().Exec(st.Rebind(`DELETE FROM applied_migrations WHERE name=?`), migrationRoomRecipientsV1)
			requirements.NoError(err)
			requirements.NoError(st.InitSchema())
			var count, sender int64
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM message_recipients WHERE message_id=? AND participant_id=?`), event, mixed).Scan(&count))
			assertions.Equal(int64(2), count)
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT sender_id FROM messages WHERE id=?`), event).Scan(&sender))
			assertions.Equal(mixed, sender)
		})
	}
}
