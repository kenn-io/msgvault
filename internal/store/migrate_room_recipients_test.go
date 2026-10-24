package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedLegacyRoomEdges(t *testing.T, st *Store) {
	t.Helper()
	requirements := require.New(t)
	source, err := st.GetOrCreateSource("meeting_import", "legacy@example.org")
	requirements.NoError(err)
	room, err := st.EnsureParticipant("room@c.resource.calendar.google.com", "Room", "c.resource.calendar.google.com")
	requirements.NoError(err)
	resource, err := st.EnsureParticipant("resource@example.org", "Resource", "example.org")
	requirements.NoError(err)
	human, err := st.EnsureParticipant("human@example.org", "Human", "example.org")
	requirements.NoError(err)
	for _, kind := range []string{"email", "calendar_event", "meeting_transcript"} {
		id, err := st.PersistMessage(&MessagePersistData{
			Message:      &Message{SourceID: source.ID, SourceMessageID: kind, MessageType: kind, SenderID: sql.NullInt64{Int64: room, Valid: true}},
			Conversation: &ConversationPersistData{SourceConversationID: kind, ConversationType: "meeting"},
		})
		requirements.NoError(err)
		var conversationID int64
		requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_id FROM messages WHERE id = ?`), id).Scan(&conversationID))
		for _, participant := range []int64{room, resource, human} {
			requirements.NoError(st.EnsureConversationParticipant(conversationID, participant, "member"))
		}
		for _, role := range []string{"from", "to"} {
			requirements.NoError(st.ReplaceMessageRecipients(id, role, []int64{room, resource, human}, []string{"Room", "Resource", "Human"}))
		}
		if kind == "calendar_event" {
			requirements.NoError(st.UpsertMessageRawWithFormat(id, []byte(`{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"resource@example.org","resource":true}]}`), "gcal_json"))
		}
	}
}

func assertRepairedRoomEdges(t *testing.T, st *Store) {
	t.Helper()
	assertions, requirements := assert.New(t), require.New(t)
	var count int
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM message_recipients mr JOIN messages m ON m.id = mr.message_id WHERE m.message_type IN ('calendar_event','meeting_transcript')`).Scan(&count))
	assertions.Equal(6, count, "the event keeps its human; the transcript also keeps its unflagged resource address")
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM message_recipients mr JOIN messages m ON m.id = mr.message_id WHERE m.message_type = 'email'`).Scan(&count))
	assertions.Equal(6, count, "email room edges remain")
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages WHERE message_type IN ('calendar_event','meeting_transcript') AND sender_id IS NOT NULL`).Scan(&count))
	assertions.Zero(count)
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM conversation_participants cp WHERE EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = cp.conversation_id AND m.message_type IN ('calendar_event','meeting_transcript'))`).Scan(&count))
	assertions.Equal(3, count, "event resource evidence stays local")
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM conversation_participants cp WHERE EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = cp.conversation_id AND m.message_type = 'email')`).Scan(&count))
	assertions.Equal(3, count, "email roster remains")
}

func TestRoomRecipientsMigration(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	seedLegacyRoomEdges(t, st)
	var baseline int64
	if !st.IsPostgreSQL() {
		requirements.NoError(st.DB().QueryRow(`SELECT coalesce(max(seq), 0) FROM cache_related_change_journal`).Scan(&baseline))
	}
	_, err := st.DB().Exec(st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), "room_recipients_v1")
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	assertRepairedRoomEdges(t, st)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	assertRepairedRoomEdges(t, st)
	if !st.IsPostgreSQL() {
		for _, kind := range []string{"calendar_event", "meeting_transcript"} {
			var count int
			requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM cache_related_change_journal j
				JOIN messages m ON m.id = j.message_id
				WHERE j.dataset = 'message_facts' AND j.seq > ? AND m.message_type = ?`, baseline, kind).Scan(&count))
			assert.Positive(t, count, "migration invalidates repaired %s facts", kind)
		}
	}
}

func TestRoomRecipientsSubsetRepair(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(path)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	requirements.NoError(st.InitSchema())
	seedLegacyRoomEdges(t, st)
	destination := t.TempDir()
	_, err = CopySubset(path, destination, 3, false)
	requirements.NoError(err)
	subset, err := OpenForTest(filepath.Join(destination, "msgvault.db"))
	requirements.NoError(err)
	defer func() { requirements.NoError(subset.Close()) }()
	assertRepairedRoomEdges(t, subset)
}

func TestRoomRecipientsMigrationKeepsResourceFlagsLocal(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	seedLegacyRoomEdges(t, st)
	_, err := st.DB().Exec(`DELETE FROM messages WHERE message_type = 'email'`)
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), "room_recipients_v1")
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	for _, kind := range []string{"calendar_event", "meeting_transcript"} {
		var recipients, roster int
		requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM message_recipients r JOIN participants p ON p.id = r.participant_id JOIN messages m ON m.id = r.message_id WHERE p.email_address = 'resource@example.org' AND m.message_type = ?`), kind).Scan(&recipients))
		requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM conversation_participants cp JOIN participants p ON p.id = cp.participant_id WHERE p.email_address = 'resource@example.org' AND EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = cp.conversation_id AND m.message_type = ?)`), kind).Scan(&roster))
		if kind == "calendar_event" {
			assertions.Zero(recipients)
			assertions.Zero(roster)
		} else {
			assertions.Equal(2, recipients)
			assertions.Equal(1, roster)
		}
	}
}

func TestRoomRecipientsMigrationCandidatesFollowEdges(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source, err := st.GetOrCreateSource("gcal", "calendar@example.test")
	requirements.NoError(err)
	for i := range 64 {
		_, err := st.EnsureParticipant(fmt.Sprintf("room-%d@c.resource.calendar.google.com", i), "Room", "c.resource.calendar.google.com")
		requirements.NoError(err)
	}
	for i := range 128 {
		_, err := st.PersistMessage(&MessagePersistData{
			Message:      &Message{SourceID: source.ID, SourceMessageID: fmt.Sprintf("event-%d", i), MessageType: "calendar_event"},
			Conversation: &ConversationPersistData{SourceConversationID: fmt.Sprintf("event-%d", i), ConversationType: "calendar"},
		})
		requirements.NoError(err)
	}
	var candidates int
	requirements.NoError(st.DB().QueryRow(roomRecipientEdgesSQL + `SELECT count(*) FROM room_edges`).Scan(&candidates))
	assert.Zero(t, candidates, "unrelated rooms and events generate no repair pairs")
	requirements.NoError(st.repairRoomRecipientsContext(t.Context()))
}

func TestRoomRecipientsMigrationRemovesRosterOnlyRooms(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "roster@example.test"))
	room, err := st.EnsureParticipant("room@c.resource.calendar.google.com", "Room", "c.resource.calendar.google.com")
	requirements.NoError(err)
	resource, err := st.EnsureParticipant("resource@example.test", "Resource", "example.test")
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(`{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"resource@example.test","resource":true}]}`), "gcal_json"))
	var conversation int64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_id FROM messages WHERE id = ?`), event).Scan(&conversation))
	requirements.NoError(st.EnsureConversationParticipant(conversation, room, "member"))
	requirements.NoError(st.EnsureConversationParticipant(conversation, resource, "member"))
	requirements.NoError(st.repairRoomRecipientsContext(t.Context()))
	var rooms int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM conversation_participants WHERE conversation_id = ? AND participant_id IN (?, ?)`), conversation, room, resource).Scan(&rooms))
	assert.Zero(t, rooms)
}
