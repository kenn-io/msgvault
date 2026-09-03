package meetingarchive

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func testSnapshot(sourceID int64) Snapshot {
	return Snapshot{
		SourceID:             sourceID,
		AccountEmail:         "user@example.com",
		SourceMessageID:      "meeting-note-42",
		SourceConversationID: "meeting-note-42",
		Title:                "Weekly planning",
		StartedAt:            time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC),
		Body:                 "Weekly planning\n\nSummary:\nShip the importer.\n\nTranscript:\nTest Speaker: Ready.",
		Snippet:              "Weekly planning",
		Metadata:             []byte(`{"platform":"notion_meetings"}`),
		Raw:                  []byte(`{"discovery":{"id":"meeting-note-42"}}`),
		RawFormat:            "notion_meeting_json",
		Organizer: &Person{
			Name:  "Test Organizer",
			Email: "organizer@example.com",
		},
		Attendees: []Person{{Name: "Test Attendee", Email: "attendee@example.com"}},
	}
}

func TestArchiverCreatesProviderCanonicalMeeting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("notion_meetings", "work")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(source.ID, "user@example.com", "account-email"))

	result, err := New(st).Upsert(context.Background(), testSnapshot(source.ID), UpsertOptions{})
	require.NoError(err)
	assert.True(result.Created)
	assert.True(result.Changed)
	assert.NotZero(result.MessageID)

	var (
		messageType, sourceMessageID, subject string
		conversationType, conversationID      string
		body, rawFormat, metadata             string
		sender                                sql.NullString
		messageCount, participantCount        int
	)
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT m.message_type, m.source_message_id, m.subject,
		       c.conversation_type, c.source_conversation_id,
		       mb.body_text, mr.raw_format, m.metadata, p.email_address,
		       c.message_count, c.participant_count
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN message_bodies mb ON mb.message_id = m.id
		JOIN message_raw mr ON mr.message_id = m.id
		LEFT JOIN participants p ON p.id = m.sender_id
		WHERE m.id = ?
	`), result.MessageID).Scan(
		&messageType, &sourceMessageID, &subject,
		&conversationType, &conversationID,
		&body, &rawFormat, &metadata, &sender,
		&messageCount, &participantCount,
	))
	assert.Equal("meeting_transcript", messageType)
	assert.Equal("meeting-note-42", sourceMessageID)
	assert.Equal("Weekly planning", subject)
	assert.Equal("meeting", conversationType)
	assert.Equal("meeting-note-42", conversationID)
	assert.Contains(body, "Test Speaker: Ready.")
	assert.Equal("notion_meeting_json", rawFormat)
	assert.JSONEq(`{"platform":"notion_meetings"}`, metadata)
	assert.Equal("organizer@example.com", sender.String)
	assert.Equal(1, messageCount)
	assert.Equal(1, participantCount)

	var recipient, recipientEnvelope string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT p.email_address, mr.email_address
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ? AND mr.recipient_type = 'to'
	`), result.MessageID).Scan(&recipient, &recipientEnvelope))
	assert.Equal("attendee@example.com", recipient)
	assert.Equal("attendee@example.com", recipientEnvelope)

	var organizerEnvelope string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mr.email_address
		FROM message_recipients mr
		WHERE mr.message_id = ? AND mr.recipient_type = 'from'
	`), result.MessageID).Scan(&organizerEnvelope))
	assert.Equal("organizer@example.com", organizerEnvelope)

	raw, err := st.GetMessageRaw(result.MessageID)
	require.NoError(err)
	assert.JSONEq(`{"discovery":{"id":"meeting-note-42"}}`, string(raw))
}

func TestArchiverUnchangedSnapshotIsNoOp(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("notion_meetings", "work")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(source.ID, "user@example.com", "account-email"))
	archiver := New(st)

	first, err := archiver.Upsert(context.Background(), testSnapshot(source.ID), UpsertOptions{})
	require.NoError(err)
	var before string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT CAST(last_modified AS TEXT) FROM messages WHERE id = ?`,
	), first.MessageID).Scan(&before))

	second, err := archiver.Upsert(context.Background(), testSnapshot(source.ID), UpsertOptions{})
	require.NoError(err)
	assert.False(second.Created)
	assert.False(second.Changed)
	assert.Equal(first.MessageID, second.MessageID)

	var after string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT CAST(last_modified AS TEXT) FROM messages WHERE id = ?`,
	), first.MessageID).Scan(&after))
	assert.Equal(before, after)
}

func TestArchiverWithoutOrganizerDoesNotInventSender(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("notion_meetings", "work")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(source.ID, "user@example.com", "account-email"))
	snapshot := testSnapshot(source.ID)
	snapshot.Organizer = nil
	snapshot.Attendees = nil
	snapshot.Body = "Attendees: Unresolved User"

	result, err := New(st).Upsert(context.Background(), snapshot, UpsertOptions{})
	require.NoError(err)

	var sender sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT sender_id FROM messages WHERE id = ?`,
	), result.MessageID).Scan(&sender))
	assert.False(sender.Valid)

	var participantCount int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM participants`,
	).Scan(&participantCount))
	assert.Zero(participantCount)
}
