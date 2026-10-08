package meetingarchive

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
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
	checks := assert.New(t)
	must := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("notion_meetings", "work")
	must.NoError(err)
	must.NoError(st.AddAccountIdentity(source.ID, "user@example.com", "account-email"))
	archiver := New(st)

	first, err := archiver.Upsert(context.Background(), testSnapshot(source.ID), UpsertOptions{})
	must.NoError(err)
	var before string
	must.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT CAST(last_modified AS TEXT) FROM messages WHERE id = ?`,
	), first.MessageID).Scan(&before))

	second, err := archiver.Upsert(context.Background(), testSnapshot(source.ID), UpsertOptions{})
	must.NoError(err)
	checks.False(second.Created)
	checks.False(second.Changed)
	checks.Equal(first.MessageID, second.MessageID)

	var after string
	must.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT CAST(last_modified AS TEXT) FROM messages WHERE id = ?`,
	), first.MessageID).Scan(&after))
	checks.Equal(before, after)

	organizerA, err := st.EnsureParticipant("organizer-a@example.com", "Organizer A", "example.com")
	must.NoError(err)
	organizerB, err := st.EnsureParticipant("organizer-b@example.com", "Organizer B", "example.com")
	must.NoError(err)
	attendeeA, err := st.EnsureParticipant("attendee-a@example.com", "Attendee A", "example.com")
	must.NoError(err)
	attendeeB, err := st.EnsureParticipant("attendee-b@example.com", "Attendee B", "example.com")
	must.NoError(err)
	for _, change := range []string{"organizer changed", "organizer removed", "attendee changed", "attendee removed", "observed snapshot changed", "mixed resolved and canonical"} {
		t.Run(change, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			snapshot := testSnapshot(source.ID)
			snapshot.SourceMessageID += change
			snapshot.SourceConversationID = snapshot.SourceMessageID
			snapshot.Organizer = &Person{ParticipantID: organizerA, Name: "Observed Organizer"}
			snapshot.Attendees = []Person{{ParticipantID: attendeeA, Name: "Observed Attendee"}, {ParticipantID: attendeeA, Name: "Duplicate Attendee"}}
			first, err := archiver.Upsert(t.Context(), snapshot, UpsertOptions{})
			require.NoError(err)
			switch change {
			case "organizer changed":
				snapshot.Organizer.ParticipantID = organizerB
			case "organizer removed":
				snapshot.Organizer = nil
			case "attendee changed":
				snapshot.Attendees = []Person{{ParticipantID: attendeeB, Name: "Observed Attendee"}}
			case "attendee removed":
				snapshot.Attendees = nil
			case "observed snapshot changed":
				snapshot.Attendees = []Person{{ParticipantID: attendeeA, Name: "Updated observation", Email: "observed@example.com"}}
			case "mixed resolved and canonical":
				snapshot.Attendees = []Person{{ParticipantID: attendeeB, Name: "Observed Attendee"}, {Name: "Canonical Attendee", Email: "canonical@example.com"}}
			}
			second, err := archiver.Upsert(t.Context(), snapshot, UpsertOptions{})
			require.NoError(err)
			assert.True(second.Changed)
			assert.Equal(first.MessageID, second.MessageID)
			var sender sql.NullInt64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT sender_id FROM messages WHERE id = ?`), first.MessageID).Scan(&sender))
			if snapshot.Organizer == nil {
				assert.False(sender.Valid)
			} else {
				assert.Equal(snapshot.Organizer.ParticipantID, sender.Int64)
			}
			for _, role := range []string{"from", "to"} {
				rows, err := st.DB().Query(st.Rebind(`SELECT participant_id, COALESCE(display_name, ''), COALESCE(email_address, '') FROM message_recipients WHERE message_id = ? AND recipient_type = ? ORDER BY id`), first.MessageID, role)
				require.NoError(err)
				defer func() { assert.NoError(rows.Close()) }()
				people := snapshot.Attendees
				if role == "from" {
					people = nil
					if snapshot.Organizer != nil {
						people = []Person{*snapshot.Organizer}
					}
				}
				var observed []Person
				for rows.Next() {
					var person Person
					require.NoError(rows.Scan(&person.ParticipantID, &person.Name, &person.Email))
					observed = append(observed, person)
				}
				require.NoError(rows.Err())
				if change == "mixed resolved and canonical" && role == "to" {
					require.Len(observed, 2)
					assert.Equal(attendeeB, observed[0].ParticipantID)
					assert.Equal("canonical@example.com", observed[1].Email)
					assert.Positive(observed[1].ParticipantID)
				} else {
					if len(people) > 1 {
						people = people[:1]
					}
					assert.Equal(people, observed)
				}
			}
			var beforeRow sql.NullInt64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT MIN(id) FROM message_recipients WHERE message_id = ?`), first.MessageID).Scan(&beforeRow))
			third, err := archiver.Upsert(t.Context(), snapshot, UpsertOptions{})
			require.NoError(err)
			if change == "mixed resolved and canonical" {
				assert.True(third.Changed, "mixed IDs retain the canonical write path")
			} else {
				assert.False(third.Changed)
				var afterRow sql.NullInt64
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT MIN(id) FROM message_recipients WHERE message_id = ?`), first.MessageID).Scan(&afterRow))
				assert.Equal(beforeRow, afterRow)
			}
		})
	}
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

func TestTranscriptEnrichmentPreservesRecordingAttachmentCounts(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("twilio", "work")
	require.NoError(err)
	snapshot := testSnapshot(source.ID)
	snapshot.Organizer = nil
	result, err := New(st).Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)
	require.NoError(st.UpsertAttachmentRecordWithStats(t.Context(), result.MessageID, store.AttachmentWrite{
		Filename: "recording.wav", MIMEType: "audio/wav", ContentHash: strings.Repeat("a", 64), StoragePath: "aa/" + strings.Repeat("a", 64), Size: 100,
		SourcePartKey: "recording:synthetic", Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
	}, true))
	snapshot.Raw = []byte(`{"transcript":"New retained speech"}`)
	snapshot.Body = "New retained speech"
	enriched, err := New(st).Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)
	assert.Equal(result.MessageID, enriched.MessageID)
	var attached bool
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT has_attachments, attachment_count FROM messages WHERE id = ?"), result.MessageID).Scan(&attached, &count))
	assert.True(attached)
	assert.Equal(1, count)
}
