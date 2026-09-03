package store_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type failingMessageIDReader struct{}

func (failingMessageIDReader) Read([]byte) (int, error) {
	return 0, errors.New("synthetic staged-ID read failure")
}

// TestUpsertMessagePersistsListID catches a missing list_id column or an
// upsert that omits the parsed email list identifier.
func TestUpsertMessagePersistsListID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	conversationID, err := f.Store.EnsureConversation(f.Source.ID, "list-thread", "List announcement")
	require.NoError(err, "ensure conversation")

	message := &store.Message{
		SourceID:        f.Source.ID,
		SourceMessageID: "list-message",
		ConversationID:  conversationID,
		MessageType:     store.MessageTypeEmail,
		ListID:          sql.NullString{String: "<announce.example.org>", Valid: true},
	}
	_, err = f.Store.UpsertMessage(message)
	require.NoError(err, "insert message")

	_, err = f.Store.UpsertMessage(message)
	require.NoError(err, "upsert message")

	var listID sql.NullString
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT list_id FROM messages WHERE source_id = ? AND source_message_id = ?`),
		f.Source.ID, message.SourceMessageID,
	).Scan(&listID), "read persisted list ID")
	assert.True(listID.Valid, "list ID should be present")
	assert.Equal("<announce.example.org>", listID.String, "list ID")
}

type siblingRecipientSnapshot struct {
	Type          string
	ParticipantID int64
	DisplayName   sql.NullString
	EnvelopeEmail sql.NullString
	EmailAddress  sql.NullString
	Participant   sql.NullString
	Domain        sql.NullString
}

type siblingActivitySnapshot struct {
	Revision          int64
	ProcessedRevision int64
	QueuedAt          string
}

type siblingEmbeddingChangeSnapshot struct {
	Sequence          int64
	Kind              string
	MessageID         sql.NullInt64
	OldMessageType    sql.NullString
	NewMessageType    sql.NullString
	OldConversationID sql.NullInt64
	NewConversationID sql.NullInt64
	OldSentAt         sql.NullString
	NewSentAt         sql.NullString
	ParticipantID     sql.NullInt64
}

type siblingPersonSweepChangeSnapshot struct {
	Sequence       int64
	PersonID       int64
	SourceLane     string
	ChangeKind     string
	EvidenceEffect string
	SourceID       sql.NullInt64
	MessageID      sql.NullInt64
	AttachmentID   sql.NullInt64
	OccurrenceKey  string
	RecordedAt     string
}

type siblingPersonSweepWorkSnapshot struct {
	PersonID             int64
	DirtyThroughSequence int64
	AvailableAt          string
	AttemptCount         int
	LastFailureClass     string
	LeaseOwner           string
	LeaseUntil           sql.NullString
	LeaseFence           int64
	CreatedAt            string
	UpdatedAt            string
}

type siblingFTSSnapshot struct {
	MessageID      int64
	Subject        string
	Body           string
	FromAddr       string
	ToAddr         string
	CcAddr         string
	SearchDocument string
}

type siblingAttachmentSnapshot struct {
	Filename           sql.NullString
	MIMEType           sql.NullString
	StoragePath        string
	ContentHash        sql.NullString
	Size               int64
	SourceAttachmentID sql.NullString
	Metadata           sql.NullString
	Role               string
	RoleSource         string
	SourcePartKey      sql.NullString
	ContentID          sql.NullString
}

type siblingConversationSnapshot struct {
	SourceConversationID string
	ConversationType     string
	Title                sql.NullString
	Participants         []string
}

type siblingMessageSnapshot struct {
	SourceID         int64
	SourceMessageID  string
	ConversationID   int64
	RFC822MessageID  sql.NullString
	MessageType      string
	SentAt           sql.NullTime
	ReceivedAt       sql.NullTime
	InternalDate     sql.NullTime
	SenderID         sql.NullInt64
	SourceIsFromMe   sql.NullBool
	IdentityIsFromMe bool
	IsFromMe         bool
	Subject          sql.NullString
	Snippet          sql.NullString
	SizeEstimate     sql.NullInt64
	HasAttachments   bool
	AttachmentCount  int
	Metadata         sql.NullString
	IndexingVersion  sql.NullInt64
	LastModified     sql.NullTime
	EmbedGen         sql.NullInt64
	ContentChangedAt sql.NullTime
	BodyText         sql.NullString
	BodyHTML         sql.NullString
	RawMIME          []byte
	RawFormat        string
	Recipients       []siblingRecipientSnapshot
	LabelIDs         []int64
	MessageLabels    []string
	Activity         siblingActivitySnapshot
	EmbeddingChanges []siblingEmbeddingChangeSnapshot
	PersonChanges    []siblingPersonSweepChangeSnapshot
	PersonWork       []siblingPersonSweepWorkSnapshot
	FTS              siblingFTSSnapshot
	Attachments      []siblingAttachmentSnapshot
	Conversation     siblingConversationSnapshot
	ParticipantRows  []string
	LabelRows        []string
}

func readSiblingMessageSnapshot(t *testing.T, st *store.Store, messageID int64) siblingMessageSnapshot {
	t.Helper()
	var snapshot siblingMessageSnapshot
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT source_id, source_message_id, conversation_id, rfc822_message_id, message_type,
		       sent_at, received_at, internal_date, sender_id,
		       source_is_from_me, identity_is_from_me, is_from_me,
		       subject, snippet, size_estimate, has_attachments, attachment_count, metadata,
		       indexing_version, last_modified, embed_gen, content_changed_at
		FROM messages
		WHERE id = ?
	`), messageID).Scan(
		&snapshot.SourceID,
		&snapshot.SourceMessageID,
		&snapshot.ConversationID,
		&snapshot.RFC822MessageID,
		&snapshot.MessageType,
		&snapshot.SentAt,
		&snapshot.ReceivedAt,
		&snapshot.InternalDate,
		&snapshot.SenderID,
		&snapshot.SourceIsFromMe,
		&snapshot.IdentityIsFromMe,
		&snapshot.IsFromMe,
		&snapshot.Subject,
		&snapshot.Snippet,
		&snapshot.SizeEstimate,
		&snapshot.HasAttachments,
		&snapshot.AttachmentCount,
		&snapshot.Metadata,
		&snapshot.IndexingVersion,
		&snapshot.LastModified,
		&snapshot.EmbedGen,
		&snapshot.ContentChangedAt,
	)
	require.NoError(t, err, "read sibling message fields")

	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT body_text, body_html FROM message_bodies WHERE message_id = ?
	`), messageID).Scan(&snapshot.BodyText, &snapshot.BodyHTML)
	require.NoError(t, err, "read sibling message body")
	snapshot.RawMIME, err = st.GetMessageRaw(messageID)
	require.NoError(t, err, "read sibling raw MIME")
	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT raw_format FROM message_raw WHERE message_id = ?
	`), messageID).Scan(&snapshot.RawFormat)
	require.NoError(t, err, "read sibling raw format")

	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT mr.recipient_type, mr.participant_id, mr.display_name, mr.email_address,
		       p.email_address, p.display_name, p.domain
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ?
		ORDER BY mr.recipient_type, mr.id
	`), messageID)
	require.NoError(t, err, "read sibling recipients")
	defer func() { require.NoError(t, rows.Close(), "close sibling recipient rows") }()
	for rows.Next() {
		var recipient siblingRecipientSnapshot
		require.NoError(t, rows.Scan(
			&recipient.Type,
			&recipient.ParticipantID,
			&recipient.DisplayName,
			&recipient.EnvelopeEmail,
			&recipient.EmailAddress,
			&recipient.Participant,
			&recipient.Domain,
		), "scan sibling recipient")
		snapshot.Recipients = append(snapshot.Recipients, recipient)
	}
	require.NoError(t, rows.Err(), "iterate sibling recipients")

	labelRows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT label_id FROM message_labels WHERE message_id = ? ORDER BY label_id
	`), messageID)
	require.NoError(t, err, "read sibling labels")
	defer func() { require.NoError(t, labelRows.Close(), "close sibling label rows") }()
	for labelRows.Next() {
		var labelID int64
		require.NoError(t, labelRows.Scan(&labelID), "scan sibling label")
		snapshot.LabelIDs = append(snapshot.LabelIDs, labelID)
	}
	require.NoError(t, labelRows.Err(), "iterate sibling labels")
	messageLabelRows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT COALESCE(l.source_label_id, ''), l.name,
		       COALESCE(l.label_type, ''), COALESCE(l.system_role, '')
		FROM message_labels ml
		JOIN labels l ON l.id = ml.label_id
		WHERE ml.message_id = ?
		ORDER BY l.source_label_id, l.name, l.id
	`), messageID)
	require.NoError(t, err, "read sibling message label descriptors")
	defer func() { require.NoError(t, messageLabelRows.Close(), "close sibling message label rows") }()
	for messageLabelRows.Next() {
		var sourceLabelID, name, labelType, systemRole string
		require.NoError(t, messageLabelRows.Scan(
			&sourceLabelID, &name, &labelType, &systemRole,
		), "scan sibling message label descriptor")
		snapshot.MessageLabels = append(snapshot.MessageLabels,
			sourceLabelID+"|"+name+"|"+labelType+"|"+systemRole)
	}
	require.NoError(t, messageLabelRows.Err(), "iterate sibling message label descriptors")

	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT revision, processed_revision, CAST(queued_at AS TEXT)
		FROM activity_projection_queue WHERE message_id = ?
	`), messageID).Scan(
		&snapshot.Activity.Revision,
		&snapshot.Activity.ProcessedRevision,
		&snapshot.Activity.QueuedAt,
	)
	require.NoError(t, err, "read sibling activity queue")

	embeddingRows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT sequence, kind, message_id, old_message_type, new_message_type,
		       old_conversation_id, new_conversation_id,
		       CAST(old_sent_at AS TEXT), CAST(new_sent_at AS TEXT), participant_id
		FROM embedding_changes WHERE message_id = ? ORDER BY sequence
	`), messageID)
	require.NoError(t, err, "read sibling embedding changes")
	defer func() { require.NoError(t, embeddingRows.Close(), "close sibling embedding changes") }()
	for embeddingRows.Next() {
		var change siblingEmbeddingChangeSnapshot
		require.NoError(t, embeddingRows.Scan(
			&change.Sequence,
			&change.Kind,
			&change.MessageID,
			&change.OldMessageType,
			&change.NewMessageType,
			&change.OldConversationID,
			&change.NewConversationID,
			&change.OldSentAt,
			&change.NewSentAt,
			&change.ParticipantID,
		), "scan sibling embedding change")
		snapshot.EmbeddingChanges = append(snapshot.EmbeddingChanges, change)
	}
	require.NoError(t, embeddingRows.Err(), "iterate sibling embedding changes")

	personScope := `
		SELECT pp.person_id
		FROM person_participants pp
		JOIN messages m ON m.sender_id = pp.participant_id
		WHERE m.id = ?
		UNION
		SELECT pp.person_id
		FROM person_participants pp
		JOIN message_recipients mr ON mr.participant_id = pp.participant_id
		WHERE mr.message_id = ?`
	personRows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT sequence, person_id, source_lane, change_kind, evidence_effect,
		       source_id, message_id, attachment_id, occurrence_key, CAST(recorded_at AS TEXT)
		FROM person_sweep_changes
		WHERE message_id = ? OR person_id IN (`+personScope+`)
		ORDER BY sequence
	`), messageID, messageID, messageID)
	require.NoError(t, err, "read sibling person sweep changes")
	defer func() { require.NoError(t, personRows.Close(), "close sibling person sweep changes") }()
	for personRows.Next() {
		var change siblingPersonSweepChangeSnapshot
		require.NoError(t, personRows.Scan(
			&change.Sequence,
			&change.PersonID,
			&change.SourceLane,
			&change.ChangeKind,
			&change.EvidenceEffect,
			&change.SourceID,
			&change.MessageID,
			&change.AttachmentID,
			&change.OccurrenceKey,
			&change.RecordedAt,
		), "scan sibling person sweep change")
		snapshot.PersonChanges = append(snapshot.PersonChanges, change)
	}
	require.NoError(t, personRows.Err(), "iterate sibling person sweep changes")

	workRows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT person_id, dirty_through_sequence, CAST(available_at AS TEXT), attempt_count,
		       last_failure_class, lease_owner, CAST(lease_until AS TEXT), lease_fence,
		       CAST(created_at AS TEXT), CAST(updated_at AS TEXT)
		FROM person_sweep_work
		WHERE person_id IN (`+personScope+`)
		ORDER BY person_id
	`), messageID, messageID)
	require.NoError(t, err, "read sibling person sweep work")
	defer func() { require.NoError(t, workRows.Close(), "close sibling person sweep work") }()
	for workRows.Next() {
		var work siblingPersonSweepWorkSnapshot
		require.NoError(t, workRows.Scan(
			&work.PersonID,
			&work.DirtyThroughSequence,
			&work.AvailableAt,
			&work.AttemptCount,
			&work.LastFailureClass,
			&work.LeaseOwner,
			&work.LeaseUntil,
			&work.LeaseFence,
			&work.CreatedAt,
			&work.UpdatedAt,
		), "scan sibling person sweep work")
		snapshot.PersonWork = append(snapshot.PersonWork, work)
	}
	require.NoError(t, workRows.Err(), "iterate sibling person sweep work")

	if st.IsPostgreSQL() {
		err = st.DB().QueryRowContext(t.Context(), `
			SELECT id, COALESCE(CAST(search_fts AS TEXT), '') FROM messages WHERE id = $1
		`, messageID).Scan(&snapshot.FTS.MessageID, &snapshot.FTS.SearchDocument)
		require.NoError(t, err, "read sibling PostgreSQL FTS document")
	} else {
		err = st.DB().QueryRowContext(t.Context(), `
			SELECT message_id, subject, body, from_addr, to_addr, cc_addr
			FROM messages_fts WHERE rowid = ?
		`, messageID).Scan(
			&snapshot.FTS.MessageID,
			&snapshot.FTS.Subject,
			&snapshot.FTS.Body,
			&snapshot.FTS.FromAddr,
			&snapshot.FTS.ToAddr,
			&snapshot.FTS.CcAddr,
		)
		require.NoError(t, err, "read sibling SQLite FTS document")
	}

	attachmentRows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT filename, mime_type, storage_path, content_hash, size,
		       source_attachment_id, attachment_metadata, attachment_role,
		       role_source, source_part_key, content_id
		FROM attachments WHERE message_id = ? ORDER BY id
	`), messageID)
	require.NoError(t, err, "read sibling attachments")
	defer func() { require.NoError(t, attachmentRows.Close(), "close sibling attachment rows") }()
	for attachmentRows.Next() {
		var attachment siblingAttachmentSnapshot
		require.NoError(t, attachmentRows.Scan(
			&attachment.Filename,
			&attachment.MIMEType,
			&attachment.StoragePath,
			&attachment.ContentHash,
			&attachment.Size,
			&attachment.SourceAttachmentID,
			&attachment.Metadata,
			&attachment.Role,
			&attachment.RoleSource,
			&attachment.SourcePartKey,
			&attachment.ContentID,
		), "scan sibling attachment")
		snapshot.Attachments = append(snapshot.Attachments, attachment)
	}
	require.NoError(t, attachmentRows.Err(), "iterate sibling attachments")

	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT source_conversation_id, conversation_type, title
		FROM conversations WHERE id = ?
	`), snapshot.ConversationID).Scan(
		&snapshot.Conversation.SourceConversationID,
		&snapshot.Conversation.ConversationType,
		&snapshot.Conversation.Title,
	)
	require.NoError(t, err, "read sibling conversation")
	conversationParticipantRows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT CAST(participant_id AS TEXT) || ':' || role
		FROM conversation_participants WHERE conversation_id = ?
		ORDER BY participant_id, role
	`), snapshot.ConversationID)
	require.NoError(t, err, "read sibling conversation participants")
	defer func() {
		require.NoError(t, conversationParticipantRows.Close(), "close sibling conversation participant rows")
	}()
	for conversationParticipantRows.Next() {
		var participant string
		require.NoError(t, conversationParticipantRows.Scan(&participant), "scan sibling conversation participant")
		snapshot.Conversation.Participants = append(snapshot.Conversation.Participants, participant)
	}
	require.NoError(t, conversationParticipantRows.Err(), "iterate sibling conversation participants")

	participantRows, err := st.DB().QueryContext(t.Context(), `
		SELECT COALESCE(email_address, ''), COALESCE(display_name, ''), COALESCE(domain, '')
		FROM participants ORDER BY email_address, display_name, domain, id`)
	require.NoError(t, err, "read participant directory")
	defer func() { require.NoError(t, participantRows.Close(), "close participant directory rows") }()
	for participantRows.Next() {
		var email, name, domain string
		require.NoError(t, participantRows.Scan(&email, &name, &domain), "scan participant directory row")
		snapshot.ParticipantRows = append(snapshot.ParticipantRows, email+"|"+name+"|"+domain)
	}
	require.NoError(t, participantRows.Err(), "iterate participant directory")

	labelDirectoryRows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT COALESCE(source_label_id, ''), name, COALESCE(label_type, ''), COALESCE(system_role, '')
		FROM labels WHERE source_id = ? ORDER BY source_label_id, name, id
	`), snapshot.SourceID)
	require.NoError(t, err, "read label directory")
	defer func() { require.NoError(t, labelDirectoryRows.Close(), "close label directory rows") }()
	for labelDirectoryRows.Next() {
		var sourceLabelID, name, labelType, systemRole string
		require.NoError(t, labelDirectoryRows.Scan(
			&sourceLabelID, &name, &labelType, &systemRole,
		), "scan label directory row")
		snapshot.LabelRows = append(snapshot.LabelRows,
			sourceLabelID+"|"+name+"|"+labelType+"|"+systemRole)
	}
	require.NoError(t, labelDirectoryRows.Err(), "iterate label directory")
	return snapshot
}

func TestPersistMessageWithParticipantsKeepsSiblingDependentsIsolatedOnUpsert(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	lastModifiedBaseline := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	contentChangedBaseline := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

	source, err := st.GetOrCreateSource("gmail", "sibling-isolation@example.test")
	require.NoError(err, "GetOrCreateSource")
	conversationA, err := st.EnsureConversation(source.ID, "thread-a", "Thread A")
	require.NoError(err, "EnsureConversation A")
	conversationB, err := st.EnsureConversation(source.ID, "thread-b", "Thread B")
	require.NoError(err, "EnsureConversation B")
	labelA, err := st.EnsureLabel(source.ID, "LABEL_A", "Label A", "user")
	require.NoError(err, "EnsureLabel A")
	labelB1, err := st.EnsureLabel(source.ID, "LABEL_B1", "Label B 1", "user")
	require.NoError(err, "EnsureLabel B1")
	labelB2, err := st.EnsureLabel(source.ID, "LABEL_B2", "Label B 2", "user")
	require.NoError(err, "EnsureLabel B2")
	bSenderParticipantID, err := st.EnsureParticipant(
		"sender-b@example.test", "Sender B", "example.test",
	)
	require.NoError(err, "EnsureParticipant B sender")
	bPerson, created, err := st.CreatePersonFromParticipant(bSenderParticipantID)
	require.NoError(err, "CreatePersonFromParticipant B sender")
	require.True(created, "B sender person fixture must be newly created")
	_, err = st.SetPersonTrackingContext(ctx, bPerson.ID, true)
	require.NoError(err, "track B sender person")
	require.NoError(st.EnableEmbeddingChangeJournal(ctx), "enable embedding journal")

	messageAID, err := st.PersistMessageWithParticipantsContext(ctx, []store.ParticipantPersistData{
		{EmailAddress: "sender-a@example.test", DisplayName: "Sender A", Domain: "example.test"},
		{EmailAddress: "recipient-a@example.test", DisplayName: "Recipient A", Domain: "example.test"},
	}, func(participantIDs []int64) *store.MessagePersistData {
		metadata := sql.NullString{String: `{"fixture":"a-v1"}`, Valid: true}
		return &store.MessagePersistData{
			Message: &store.Message{
				ConversationID:  conversationA,
				SourceID:        source.ID,
				SourceMessageID: "message-a",
				RFC822MessageID: sql.NullString{String: "<message-a@example.test>", Valid: true},
				MessageType:     "email",
				SentAt:          sql.NullTime{Time: time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC), Valid: true},
				SenderID:        sql.NullInt64{Int64: participantIDs[0], Valid: true},
				Subject:         sql.NullString{String: "Message A original", Valid: true},
				Snippet:         sql.NullString{String: "A original snippet", Valid: true},
				SizeEstimate:    101,
			},
			Metadata: &metadata,
			BodyText: sql.NullString{String: "Message A original body", Valid: true},
			BodyHTML: sql.NullString{String: "<p>Message A original body</p>", Valid: true},
			RawMIME:  []byte("raw-message-a-v1"),
			Recipients: []store.RecipientSet{
				{Type: "from", ParticipantIDs: []int64{participantIDs[0]}, DisplayNames: []string{"Sender A"}, EmailAddresses: []string{"sender-a@example.test"}},
				{Type: "to", ParticipantIDs: []int64{participantIDs[1]}, DisplayNames: []string{"Recipient A"}, EmailAddresses: []string{"recipient-a@example.test"}},
			},
			LabelIDs: []int64{labelA},
			FTS: &store.FTSDoc{
				Subject: "Message A original",
				Body:    "amber-original-token",
			},
		}
	})
	require.NoError(err, "persist message A")

	messageBID, err := st.PersistMessageWithParticipantsContext(ctx, []store.ParticipantPersistData{
		{EmailAddress: "sender-b@example.test", DisplayName: "Sender B", Domain: "example.test"},
		{EmailAddress: "recipient-b@example.test", DisplayName: "Recipient B", Domain: "example.test"},
		{EmailAddress: "copy-b@example.test", DisplayName: "Copy B", Domain: "example.test"},
		{EmailAddress: "blind-b@example.test", DisplayName: "Blind B", Domain: "example.test"},
	}, func(participantIDs []int64) *store.MessagePersistData {
		metadata := sql.NullString{String: `{"fixture":"b"}`, Valid: true}
		return &store.MessagePersistData{
			Message: &store.Message{
				ConversationID:  conversationB,
				SourceID:        source.ID,
				SourceMessageID: "message-b",
				RFC822MessageID: sql.NullString{String: "<message-b@example.test>", Valid: true},
				MessageType:     "email",
				SentAt:          sql.NullTime{Time: time.Date(2026, 8, 29, 11, 0, 0, 0, time.UTC), Valid: true},
				ReceivedAt:      sql.NullTime{Time: time.Date(2026, 8, 29, 11, 1, 0, 0, time.UTC), Valid: true},
				InternalDate:    sql.NullTime{Time: time.Date(2026, 8, 29, 11, 2, 0, 0, time.UTC), Valid: true},
				SenderID:        sql.NullInt64{Int64: participantIDs[0], Valid: true},
				IsFromMe:        true,
				Subject:         sql.NullString{String: "Message B subject", Valid: true},
				Snippet:         sql.NullString{String: "Message B snippet", Valid: true},
				SizeEstimate:    202,
				HasAttachments:  true,
				AttachmentCount: 2,
			},
			Metadata:  &metadata,
			BodyText:  sql.NullString{String: "Message B text body", Valid: true},
			BodyHTML:  sql.NullString{String: "<p>Message B HTML body</p>", Valid: true},
			RawMIME:   []byte("raw-message-b"),
			RawFormat: "mime",
			Recipients: []store.RecipientSet{
				{Type: "from", ParticipantIDs: []int64{participantIDs[0]}, DisplayNames: []string{"Sender B"}, EmailAddresses: []string{"sender-b@example.test"}},
				{Type: "to", ParticipantIDs: []int64{participantIDs[1]}, DisplayNames: []string{"Recipient B"}, EmailAddresses: []string{"recipient-b@example.test"}},
				{Type: "cc", ParticipantIDs: []int64{participantIDs[2]}, DisplayNames: []string{"Copy B"}, EmailAddresses: []string{"copy-b@example.test"}},
				{Type: "bcc", ParticipantIDs: []int64{participantIDs[3]}, DisplayNames: []string{"Blind B"}, EmailAddresses: []string{"blind-b@example.test"}},
			},
			LabelIDs: []int64{labelB1, labelB2},
			FTS: &store.FTSDoc{
				Subject:  "Message B subject",
				Body:     "cobalt-sibling-token",
				FromAddr: "sender-b@example.test",
				ToAddrs:  "recipient-b@example.test",
				CcAddrs:  "copy-b@example.test",
			},
		}
	})
	require.NoError(err, "persist message B")
	require.NotEqual(messageAID, messageBID, "fixture requires distinct sibling rows")
	require.NoError(st.SetEmbedGen(ctx, []int64{messageBID}, 73), "seed B embed generation")
	baselineResult, err := st.DB().ExecContext(ctx, st.Rebind(`
		UPDATE messages
		SET last_modified = ?, content_changed_at = ?
		WHERE id = ?
	`), lastModifiedBaseline, contentChangedBaseline, messageBID)
	require.NoError(err, "seed B historical timestamp baselines")
	baselineRows, err := baselineResult.RowsAffected()
	require.NoError(err, "read B historical timestamp baseline row count")
	require.Equal(int64(1), baselineRows, "seed exactly one B historical timestamp baseline")

	beforeB := readSiblingMessageSnapshot(t, st, messageBID)
	require.True(beforeB.IndexingVersion.Valid, "B FTS indexing version fixture must be seeded")
	require.True(beforeB.LastModified.Valid, "B last-modified fixture must be seeded")
	require.Equal(lastModifiedBaseline, beforeB.LastModified.Time.UTC(),
		"B last-modified fixture must use its fixed historical baseline")
	require.True(beforeB.EmbedGen.Valid, "B embed generation fixture must be seeded")
	require.True(beforeB.ContentChangedAt.Valid, "B content watermark fixture must be seeded")
	require.Equal(contentChangedBaseline, beforeB.ContentChangedAt.Time.UTC(),
		"B content watermark fixture must use its fixed historical baseline")
	require.Positive(beforeB.Activity.Revision, "B activity queue fixture must be seeded")
	require.NotEmpty(beforeB.EmbeddingChanges, "B embedding journal fixture must be seeded")
	require.NotEmpty(beforeB.PersonChanges, "B person sweep journal fixture must be seeded")
	require.NotEmpty(beforeB.PersonWork, "B person sweep work fixture must be seeded")
	require.Equal(messageBID, beforeB.FTS.MessageID, "B FTS row must belong to B")
	require.NotEmpty(beforeB.FTS.SearchDocument+beforeB.FTS.Subject,
		"B full FTS document fixture must be seeded")
	_, beforeBHits, err := st.SearchMessages("cobalt-sibling-token", 0, 10)
	require.NoError(err, "search B before A upsert")
	require.Equal(int64(1), beforeBHits, "B FTS fixture")

	repersistedAID, err := st.PersistMessageWithParticipantsContext(ctx, []store.ParticipantPersistData{
		{EmailAddress: "sender-a@example.test", DisplayName: "Sender A", Domain: "example.test"},
		{EmailAddress: "recipient-a@example.test", DisplayName: "Recipient A", Domain: "example.test"},
	}, func(participantIDs []int64) *store.MessagePersistData {
		metadata := sql.NullString{String: `{"fixture":"a-v2"}`, Valid: true}
		return &store.MessagePersistData{
			Message: &store.Message{
				ConversationID:  conversationA,
				SourceID:        source.ID,
				SourceMessageID: "message-a",
				RFC822MessageID: sql.NullString{String: "<message-a-v2@example.test>", Valid: true},
				MessageType:     "email",
				SentAt:          sql.NullTime{Time: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC), Valid: true},
				SenderID:        sql.NullInt64{Int64: participantIDs[0], Valid: true},
				Subject:         sql.NullString{String: "Message A updated", Valid: true},
				Snippet:         sql.NullString{String: "A updated snippet", Valid: true},
				SizeEstimate:    303,
			},
			Metadata: &metadata,
			BodyText: sql.NullString{String: "Message A updated body", Valid: true},
			BodyHTML: sql.NullString{String: "<p>Message A updated body</p>", Valid: true},
			RawMIME:  []byte("raw-message-a-v2"),
			Recipients: []store.RecipientSet{
				{Type: "from", ParticipantIDs: []int64{participantIDs[0]}, DisplayNames: []string{"Sender A"}, EmailAddresses: []string{"sender-a@example.test"}},
				{Type: "to", ParticipantIDs: []int64{participantIDs[1]}, DisplayNames: []string{"Recipient A"}, EmailAddresses: []string{"recipient-a@example.test"}},
			},
			LabelIDs: []int64{labelA},
			FTS: &store.FTSDoc{
				Subject: "Message A updated",
				Body:    "amber-updated-token",
			},
		}
	})
	require.NoError(err, "re-persist message A")
	assert.Equal(messageAID, repersistedAID, "upsert must return A's existing internal ID")

	afterB := readSiblingMessageSnapshot(t, st, messageBID)
	assert.Equal(beforeB, afterB, "A upsert must leave all B fields and dependents unchanged")
	_, afterBHits, err := st.SearchMessages("cobalt-sibling-token", 0, 10)
	require.NoError(err, "search B after A upsert")
	assert.Equal(int64(1), afterBHits, "A upsert must leave B's FTS document unchanged")

	updatedA := readSiblingMessageSnapshot(t, st, messageAID)
	assert.Equal("Message A updated", updatedA.Subject.String, "A subject")
	assert.Equal("Message A updated body", updatedA.BodyText.String, "A body")
	assert.Equal([]byte("raw-message-a-v2"), updatedA.RawMIME, "A raw MIME")
}

type repairStoreFixture struct {
	Store          *store.Store
	SourceID       int64
	ConversationID int64
	TargetID       int64
	SiblingID      int64
	Guard          store.MessageIdentityGuard
	Baseline       time.Time
}

func seedRepairStoreFixture(t *testing.T) repairStoreFixture {
	t.Helper()
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	baseline := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

	source, err := st.GetOrCreateSource("gmail", "repair-fixture@example.test")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "repair-thread", "email_thread", "Original repair thread")
	require.NoError(err)
	siblingConversationID, err := st.EnsureConversationWithType(
		source.ID, "sibling-thread", "email_thread", "Sibling thread")
	require.NoError(err)
	oldLabelID, err := st.EnsureLabel(source.ID, "OLD", "Old label", "user")
	require.NoError(err)
	require.NoError(st.EnableEmbeddingChangeJournal(ctx))

	targetID, err := st.PersistMessageWithParticipantsContext(ctx, []store.ParticipantPersistData{
		{EmailAddress: "old-sender@example.test", DisplayName: "Old Sender", Domain: "example.test"},
		{EmailAddress: "old-recipient@example.test", DisplayName: "Old Recipient", Domain: "example.test"},
	}, func(participantIDs []int64) *store.MessagePersistData {
		metadata := sql.NullString{String: `{"version":"old"}`, Valid: true}
		return &store.MessagePersistData{
			Message: &store.Message{
				ConversationID:  conversationID,
				SourceID:        source.ID,
				SourceMessageID: "repair-target",
				RFC822MessageID: sql.NullString{String: "<repair-old@example.test>", Valid: true},
				MessageType:     "email",
				SenderID:        sql.NullInt64{Int64: participantIDs[0], Valid: true},
				Subject:         sql.NullString{String: "Original target", Valid: true},
				Snippet:         sql.NullString{String: "Original snippet", Valid: true},
				SizeEstimate:    100,
			},
			Metadata: &metadata,
			BodyText: sql.NullString{String: "original target body", Valid: true},
			BodyHTML: sql.NullString{String: "<p>original target body</p>", Valid: true},
			RawMIME:  []byte("original-target-raw"),
			Recipients: []store.RecipientSet{
				{Type: "from", ParticipantIDs: participantIDs[:1], DisplayNames: []string{"Old Sender"}, EmailAddresses: []string{"old-sender@example.test"}},
				{Type: "to", ParticipantIDs: participantIDs[1:], DisplayNames: []string{"Old Recipient"}, EmailAddresses: []string{"old-recipient@example.test"}},
			},
			LabelIDs: []int64{oldLabelID},
			FTS: &store.FTSDoc{
				Subject:  "Original target",
				Body:     "original-target-token",
				FromAddr: "old-sender@example.test",
				ToAddrs:  "old-recipient@example.test",
			},
		}
	})
	require.NoError(err)

	siblingID, err := st.PersistMessageWithParticipantsContext(ctx, []store.ParticipantPersistData{
		{EmailAddress: "sibling-sender@example.test", DisplayName: "Sibling Sender", Domain: "example.test"},
	}, func(participantIDs []int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message: &store.Message{
				ConversationID:  siblingConversationID,
				SourceID:        source.ID,
				SourceMessageID: "repair-sibling",
				MessageType:     "email",
				SenderID:        sql.NullInt64{Int64: participantIDs[0], Valid: true},
				Subject:         sql.NullString{String: "Sibling subject", Valid: true},
				Snippet:         sql.NullString{String: "Sibling snippet", Valid: true},
				SizeEstimate:    200,
			},
			BodyText: sql.NullString{String: "sibling body", Valid: true},
			RawMIME:  []byte("sibling-raw"),
			Recipients: []store.RecipientSet{{
				Type: "from", ParticipantIDs: participantIDs,
				DisplayNames:   []string{"Sibling Sender"},
				EmailAddresses: []string{"sibling-sender@example.test"},
			}},
			LabelIDs: []int64{oldLabelID},
			FTS:      &store.FTSDoc{Subject: "Sibling subject", Body: "sibling-stable-token"},
		}
	})
	require.NoError(err)

	attachments := []store.AttachmentWrite{
		{
			Filename: "legacy.bin", MIMEType: "application/octet-stream",
			StoragePath: "legacy.bin", ContentHash: "legacy-hash", Size: 10,
			Role: store.AttachmentRoleUnknown, RoleSource: store.AttachmentRoleSourceLegacyAPI,
		},
		{
			Filename: "keyed.txt", MIMEType: "text/plain", StoragePath: "keyed.txt",
			ContentHash: "keyed-hash", Size: 11, SourcePartKey: "mime:part:1",
			Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceMIMEDisposition,
		},
		{
			Filename: "repaired-old.txt", MIMEType: "text/plain", StoragePath: "repaired-old.txt",
			ContentHash: "repair-old-hash", Size: 12, SourcePartKey: "repair:part:2",
			Role: store.AttachmentRoleInline, RoleSource: store.AttachmentRoleSourceRawMIMERepair,
		},
		{
			Filename: "provider.bin", MIMEType: "application/octet-stream", StoragePath: "provider.bin",
			ContentHash: "provider-hash", Size: 13, SourceAttachmentID: "provider:attachment:1",
			SourcePartKey: "provider:part:1", Role: store.AttachmentRoleStandalone,
			RoleSource: store.AttachmentRoleSourceProviderExplicit,
		},
	}
	for _, attachment := range attachments {
		require.NoError(st.UpsertAttachmentRecord(ctx, targetID, attachment))
	}
	require.NoError(st.RecomputeMessageAttachmentStats(targetID))
	require.NoError(st.SetEmbedGen(ctx, []int64{targetID}, 71))
	require.NoError(st.SetEmbedGen(ctx, []int64{siblingID}, 72))
	_, err = st.DB().ExecContext(ctx, st.Rebind(`
		UPDATE activity_projection_queue SET processed_revision = revision
		WHERE message_id IN (?, ?)
	`), targetID, siblingID)
	require.NoError(err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`
		UPDATE messages SET last_modified = ?, content_changed_at = ?
		WHERE id IN (?, ?)
	`), baseline, baseline, targetID, siblingID)
	require.NoError(err)

	return repairStoreFixture{
		Store: st, SourceID: source.ID, ConversationID: conversationID,
		TargetID: targetID, SiblingID: siblingID,
		Guard: store.MessageIdentityGuard{
			ID: targetID, SourceID: source.ID, SourceMessageID: "repair-target",
		},
		Baseline: baseline,
	}
}

func repairedMessageData(
	fixture repairStoreFixture,
	participantIDs []int64,
	replacement *[]store.AttachmentWrite,
) *store.MessagePersistData {
	metadata := sql.NullString{String: `{"version":"repaired"}`, Valid: true}
	return &store.MessagePersistData{
		Message: &store.Message{
			ID:              fixture.SiblingID,
			ConversationID:  fixture.ConversationID,
			SourceID:        fixture.SourceID,
			SourceMessageID: "repair-target",
			RFC822MessageID: sql.NullString{String: "<repair-new@example.test>", Valid: true},
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: time.Date(2026, 8, 30, 12, 30, 0, 0, time.UTC), Valid: true},
			SenderID:        sql.NullInt64{Int64: participantIDs[0], Valid: true},
			Subject:         sql.NullString{String: "Repaired target", Valid: true},
			Snippet:         sql.NullString{String: "Repaired snippet", Valid: true},
			SizeEstimate:    321,
		},
		Conversation: &store.ConversationPersistData{
			SourceConversationID: "repair-thread",
			ConversationType:     "email_thread",
			Title:                "Repaired repair thread",
			Participants: []store.ConversationParticipantRef{
				{ParticipantID: participantIDs[0], Role: "sender"},
				{ParticipantID: participantIDs[1], Role: "recipient"},
			},
		},
		Metadata:  &metadata,
		BodyText:  sql.NullString{String: "repaired target body", Valid: true},
		BodyHTML:  sql.NullString{String: "<p>repaired target body</p>", Valid: true},
		RawMIME:   []byte("repaired-target-raw"),
		RawFormat: "mime",
		Recipients: []store.RecipientSet{
			{Type: "from", ParticipantIDs: participantIDs[:1], DisplayNames: []string{"Repair Sender"}, EmailAddresses: []string{"repair-sender@example.test"}},
			{Type: "to", ParticipantIDs: participantIDs[1:], DisplayNames: []string{"Repair Recipient"}, EmailAddresses: []string{"repair-recipient@example.test"}},
		},
		LabelRefs: []store.MessageLabelRef{
			{SourceLabelID: "SENT", Info: store.LabelInfo{Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent}},
			{SourceLabelID: "REPAIRED", Info: store.LabelInfo{Name: "Repaired", Type: "user"}},
		},
		MIMEAttachmentReplacement: replacement,
		FTS: &store.FTSDoc{
			Subject:  "Repaired target",
			Body:     "repair-green-token",
			FromAddr: "repair-sender@example.test",
			ToAddrs:  "repair-recipient@example.test",
		},
	}
}

func repairParticipants() []store.ParticipantPersistData {
	return []store.ParticipantPersistData{
		{EmailAddress: "repair-sender@example.test", DisplayName: "Repair Sender", Domain: "example.test"},
		{EmailAddress: "repair-recipient@example.test", DisplayName: "Repair Recipient", Domain: "example.test"},
	}
}

func attachmentFilenames(snapshot siblingMessageSnapshot) []string {
	names := make([]string, len(snapshot.Attachments))
	for i, attachment := range snapshot.Attachments {
		names[i] = attachment.Filename.String
	}
	sort.Strings(names)
	return names
}

func attachmentBySourcePartKey(
	t *testing.T, snapshot siblingMessageSnapshot, sourcePartKey string,
) siblingAttachmentSnapshot {
	t.Helper()
	for _, attachment := range snapshot.Attachments {
		if attachment.SourcePartKey.String == sourcePartKey {
			return attachment
		}
	}
	require.FailNow(t, "attachment source-part key not found", sourcePartKey)
	return siblingAttachmentSnapshot{}
}

func changedMessageIDs(page store.ChangedMessagePage) []int64 {
	ids := make([]int64, len(page.Messages))
	for i, message := range page.Messages {
		ids[i] = message.ID
	}
	slices.Sort(ids)
	return ids
}

func TestPersistRepairMessageReplacesCompleteSnapshotAtomically(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := seedRepairStoreFixture(t)
	before := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)
	replacement := []store.AttachmentWrite{{
		Filename: "repaired.txt", MIMEType: "text/plain", StoragePath: "repaired.txt",
		ContentHash: "repaired-hash", Size: 42, SourcePartKey: "mime:part:new",
		ContentID: "repair-content-id", Role: store.AttachmentRoleInline,
		RoleSource: store.AttachmentRoleSourceRawMIMERepair,
	}}

	messageID, err := fixture.Store.PersistRepairMessageWithParticipantsContext(
		t.Context(), fixture.Guard, repairParticipants(),
		func(participantIDs []int64) *store.MessagePersistData {
			return repairedMessageData(fixture, participantIDs, &replacement)
		},
	)
	require.NoError(err)
	assert.Equal(fixture.TargetID, messageID, "repair must return the guarded internal ID")

	after := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)
	assert.Equal("repair-target", after.SourceMessageID)
	assert.Equal("Repaired target", after.Subject.String)
	assert.Equal("repaired target body", after.BodyText.String)
	assert.Equal("<p>repaired target body</p>", after.BodyHTML.String)
	assert.Equal([]byte("repaired-target-raw"), after.RawMIME)
	assert.Equal("mime", after.RawFormat)
	assert.JSONEq(`{"version":"repaired"}`, after.Metadata.String)
	assert.Equal("Repaired repair thread", after.Conversation.Title.String)
	assert.Len(after.Conversation.Participants, 2)
	assert.Equal([]string{"provider.bin", "repaired.txt"}, attachmentFilenames(after),
		"repair must replace every MIME-owned row and preserve provider-owned rows")
	assert.True(after.HasAttachments)
	assert.Equal(2, after.AttachmentCount)
	assert.False(after.EmbedGen.Valid, "content repair must invalidate the embedding generation")
	assert.True(after.ContentChangedAt.Time.After(fixture.Baseline))
	assert.Greater(after.Activity.Revision, before.Activity.Revision)
	assert.Less(after.Activity.ProcessedRevision, after.Activity.Revision,
		"repair must leave activity/conversation projection work pending")
	assert.Contains(after.ParticipantRows, "repair-sender@example.test|Repair Sender|example.test")
	assert.Contains(after.ParticipantRows, "repair-recipient@example.test|Repair Recipient|example.test")
	assert.Contains(after.LabelRows, "SENT|Sent|system|sent")
	assert.Contains(after.LabelRows, "REPAIRED|Repaired|user|")
	assert.Equal([]string{
		"REPAIRED|Repaired|user|",
		"SENT|Sent|system|sent",
	}, after.MessageLabels)
	assert.Len(after.Recipients, 2)
	assert.Equal("repair-sender@example.test", after.Recipients[0].EnvelopeEmail.String)
	assert.Equal("repair-recipient@example.test", after.Recipients[1].EnvelopeEmail.String)

	_, hits, err := fixture.Store.SearchMessages("repair-green-token", 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), hits, "repair must replace the FTS document")
	_, oldHits, err := fixture.Store.SearchMessages("original-target-token", 0, 10)
	require.NoError(err)
	assert.Zero(oldHits, "repair must remove the old FTS document")
	page, err := fixture.Store.ListChangedMessages(
		t.Context(), store.ChangedMessagesFrom(fixture.Baseline.Add(time.Second)), 20)
	require.NoError(err)
	assert.Equal([]int64{fixture.TargetID}, changedMessageIDs(page),
		"the committed repair must publish the normal change feed update")
}

func TestPersistRepairMessageAttachmentReplacementModes(t *testing.T) {
	t.Run("nil preserves every attachment row", func(t *testing.T) {
		assert := assert.New(t)
		fixture := seedRepairStoreFixture(t)
		before := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)
		messageID, err := fixture.Store.PersistRepairMessageWithParticipantsContext(
			t.Context(), fixture.Guard, repairParticipants(),
			func(participantIDs []int64) *store.MessagePersistData {
				return repairedMessageData(fixture, participantIDs, nil)
			},
		)
		require.NoError(t, err)
		assert.Equal(fixture.TargetID, messageID)
		after := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)
		assert.Equal(before.Attachments, after.Attachments)
		assert.True(after.HasAttachments)
		assert.Equal(len(before.Attachments), after.AttachmentCount)
	})

	t.Run("non-nil empty removes only MIME-owned rows", func(t *testing.T) {
		assert := assert.New(t)
		fixture := seedRepairStoreFixture(t)
		empty := []store.AttachmentWrite{}
		messageID, err := fixture.Store.PersistRepairMessageWithParticipantsContext(
			t.Context(), fixture.Guard, repairParticipants(),
			func(participantIDs []int64) *store.MessagePersistData {
				return repairedMessageData(fixture, participantIDs, &empty)
			},
		)
		require.NoError(t, err)
		assert.Equal(fixture.TargetID, messageID)
		after := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)
		assert.Equal([]string{"provider.bin"}, attachmentFilenames(after))
		assert.True(after.HasAttachments)
		assert.Equal(1, after.AttachmentCount)
		assert.True(after.Attachments[0].SourceAttachmentID.Valid,
			"the remaining row must be provider-owned")
	})
}

func TestPersistRepairMessageRejectsProviderAttachmentSourcePartCollision(t *testing.T) {
	fixture := seedRepairStoreFixture(t)
	before := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)
	providerBefore := attachmentBySourcePartKey(t, before, "provider:part:1")
	require.True(t, providerBefore.SourceAttachmentID.Valid,
		"collision fixture must be provider-owned")
	replacement := []store.AttachmentWrite{{
		Filename: "mime-collision.txt", MIMEType: "text/plain",
		StoragePath: "mime-collision.txt", ContentHash: "mime-collision-hash",
		Size: 99, SourcePartKey: "provider:part:1", ContentID: "mime-collision",
		Role:       store.AttachmentRoleInline,
		RoleSource: store.AttachmentRoleSourceRawMIMERepair,
	}}

	_, err := fixture.Store.PersistRepairMessageWithParticipantsContext(
		t.Context(), fixture.Guard, repairParticipants(),
		func(participantIDs []int64) *store.MessagePersistData {
			return repairedMessageData(fixture, participantIDs, &replacement)
		},
	)
	require.ErrorContains(t, err, "provider-owned attachment source-part collision")
	after := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)
	assert.Equal(t, before, after,
		"a source-part collision must roll back the complete repair transaction")
}

func TestPersistRepairMessageRejectsUnkeyedProviderAttachmentHashCollision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := seedRepairStoreFixture(t)
	require.NoError(fixture.Store.UpsertAttachmentRecord(t.Context(), fixture.TargetID, store.AttachmentWrite{
		Filename: "provider-unkeyed.bin", MIMEType: "application/octet-stream",
		StoragePath: "provider-unkeyed.bin", ContentHash: "shared-unkeyed-hash", Size: 14,
		SourceAttachmentID: "provider:attachment:unkeyed",
		Role:               store.AttachmentRoleStandalone,
		RoleSource:         store.AttachmentRoleSourceProviderExplicit,
	}))
	require.NoError(fixture.Store.RecomputeMessageAttachmentStats(fixture.TargetID))
	before := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)
	replacement := []store.AttachmentWrite{{
		Filename: "mime-unkeyed.bin", MIMEType: "application/octet-stream",
		StoragePath: "mime-unkeyed.bin", ContentHash: "shared-unkeyed-hash", Size: 14,
		Role:       store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceRawMIMERepair,
	}}

	_, err := fixture.Store.PersistRepairMessageWithParticipantsContext(
		t.Context(), fixture.Guard, repairParticipants(),
		func(participantIDs []int64) *store.MessagePersistData {
			return repairedMessageData(fixture, participantIDs, &replacement)
		},
	)
	require.ErrorContains(err, "provider-owned attachment content-hash collision")
	assert.Equal(before, readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID),
		"an unkeyed provider hash collision must roll back the complete repair transaction")
}

func TestUpsertAttachmentRecordStillUpdatesProviderOccurrenceBySourcePartKey(t *testing.T) {
	assert := assert.New(t)
	fixture := seedRepairStoreFixture(t)
	updated := store.AttachmentWrite{
		Filename: "provider-updated.bin", MIMEType: "application/octet-stream",
		StoragePath: "provider-updated.bin", ContentHash: "provider-updated-hash",
		Size: 101, SourceAttachmentID: "provider:attachment:1",
		SourcePartKey: "provider:part:1", Role: store.AttachmentRolePreview,
		RoleSource: store.AttachmentRoleSourceProviderExplicit,
	}

	require.NoError(t, fixture.Store.UpsertAttachmentRecord(
		t.Context(), fixture.TargetID, updated))
	after := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)
	providerAfter := attachmentBySourcePartKey(t, after, "provider:part:1")
	assert.Equal("provider-updated.bin", providerAfter.Filename.String)
	assert.Equal("provider-updated-hash", providerAfter.ContentHash.String)
	assert.Equal(int64(101), providerAfter.Size)
	assert.Equal("provider:attachment:1", providerAfter.SourceAttachmentID.String)
	assert.Equal(string(store.AttachmentRolePreview), providerAfter.Role)
	assert.Equal(string(store.AttachmentRoleSourceProviderExplicit), providerAfter.RoleSource)
}

func TestPersistRepairMessageRejectsIdentityGuardBeforeBuilder(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*store.MessageIdentityGuard)
	}{
		{name: "internal id", mutate: func(guard *store.MessageIdentityGuard) { guard.ID++ }},
		{name: "source id", mutate: func(guard *store.MessageIdentityGuard) { guard.SourceID++ }},
		{name: "source message id", mutate: func(guard *store.MessageIdentityGuard) { guard.SourceMessageID += "-wrong" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			fixture := seedRepairStoreFixture(t)
			beforeTarget := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)
			beforeSibling := readSiblingMessageSnapshot(t, fixture.Store, fixture.SiblingID)
			guard := fixture.Guard
			test.mutate(&guard)
			var built atomic.Bool
			replacement := []store.AttachmentWrite{}
			_, err := fixture.Store.PersistRepairMessageWithParticipantsContext(
				t.Context(), guard, repairParticipants(),
				func(participantIDs []int64) *store.MessagePersistData {
					built.Store(true)
					return repairedMessageData(fixture, participantIDs, &replacement)
				},
			)
			require.Error(t, err)
			assert.Contains(err.Error(), "identity guard")
			assert.False(built.Load(), "guard mismatch must reject before invoking the builder")
			assert.Equal(beforeTarget, readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID))
			assert.Equal(beforeSibling, readSiblingMessageSnapshot(t, fixture.Store, fixture.SiblingID))
		})
	}
}

func TestPersistRepairMessageRollsBackEveryMutationOnLateFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := seedRepairStoreFixture(t)
	beforeTarget := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)
	beforeSibling := readSiblingMessageSnapshot(t, fixture.Store, fixture.SiblingID)
	beforeFeed, err := fixture.Store.ListChangedMessages(
		t.Context(), store.ChangedMessagesFrom(fixture.Baseline.Add(time.Second)), 20)
	require.NoError(err)
	require.Empty(beforeFeed.Messages)
	replacement := []store.AttachmentWrite{
		{
			Filename: "valid-before-failure.txt", StoragePath: "valid-before-failure.txt",
			ContentHash: "valid-before-failure", Size: 7, SourcePartKey: "mime:valid",
			Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceRawMIMERepair,
		},
		{
			Filename: "invalid.txt", StoragePath: "invalid.txt", ContentHash: "invalid",
			Size: 8, SourcePartKey: "mime:invalid", Role: store.AttachmentRole("invalid-role"),
			RoleSource: store.AttachmentRoleSourceRawMIMERepair,
		},
	}

	_, err = fixture.Store.PersistRepairMessageWithParticipantsContext(
		t.Context(), fixture.Guard, repairParticipants(),
		func(participantIDs []int64) *store.MessagePersistData {
			data := repairedMessageData(fixture, participantIDs, &replacement)
			data.Conversation.SourceConversationID = "created-then-rolled-back"
			data.LabelRefs = append(data.LabelRefs, store.MessageLabelRef{
				SourceLabelID: "ROLLBACK", Info: store.LabelInfo{Name: "Rollback label", Type: "user"},
			})
			return data
		},
	)
	require.Error(err)
	assert.Contains(err.Error(), "invalid attachment role")
	assert.Equal(beforeTarget, readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID),
		"late failure must roll the complete target snapshot back")
	assert.Equal(beforeSibling, readSiblingMessageSnapshot(t, fixture.Store, fixture.SiblingID),
		"late failure must leave the complete sibling snapshot unchanged")
	afterFeed, feedErr := fixture.Store.ListChangedMessages(
		t.Context(), store.ChangedMessagesFrom(fixture.Baseline.Add(time.Second)), 20)
	require.NoError(feedErr)
	assert.Empty(afterFeed.Messages, "rolled-back work must not become visible in the change feed")
}

func TestPersistRepairMessageSerializesIdentityGuardRevalidation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := seedRepairStoreFixture(t)
	holder, err := fixture.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	held := true
	t.Cleanup(func() {
		if held {
			_ = holder.Rollback()
		}
	})
	_, err = holder.ExecContext(t.Context(), fixture.Store.Rebind(`
		UPDATE messages SET source_message_id = ? WHERE id = ?
	`), "repair-target-raced", fixture.TargetID)
	require.NoError(err)

	var built atomic.Bool
	result := make(chan error, 1)
	go func() {
		_, persistErr := fixture.Store.PersistRepairMessageWithParticipantsContext(
			t.Context(), fixture.Guard, repairParticipants(),
			func(participantIDs []int64) *store.MessagePersistData {
				built.Store(true)
				return repairedMessageData(fixture, participantIDs, nil)
			},
		)
		result <- persistErr
	}()

	if fixture.Store.IsPostgreSQL() {
		waitForPostgreSQLLockWait(t, fixture.Store,
			"%SELECT id, source_id, source_message_id%FROM messages%FOR UPDATE%")
	} else {
		require.Eventually(func() bool {
			return fixture.Store.DB().Stats().InUse >= 2 || len(result) > 0
		}, time.Second, time.Millisecond)
		select {
		case earlyErr := <-result:
			require.NoError(earlyErr, "repair returned before the SQLite writer committed")
		default:
		}
		assert.False(built.Load(), "SQLite writer serialization must precede the guard read")
	}

	require.NoError(holder.Commit())
	held = false
	select {
	case persistErr := <-result:
		require.Error(persistErr)
		assert.Contains(persistErr.Error(), "identity guard")
	case <-time.After(5 * time.Second):
		require.FailNow("repair did not finish after releasing identity writer")
	}
	assert.False(built.Load(), "guard must be re-read after acquiring serialization")
}

func TestPersistRepairMessageLocksParticipantDirectoryBeforeIdentityGuard(t *testing.T) {
	require := require.New(t)
	fixture := seedRepairStoreFixture(t)
	if !fixture.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL advisory and row locks are required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	blocker, err := fixture.Store.DB().BeginTx(ctx, nil)
	require.NoError(err)
	held := true
	t.Cleanup(func() {
		if held {
			_ = blocker.Rollback()
		}
	})
	_, err = blocker.ExecContext(ctx, `
		SELECT pg_advisory_xact_lock(
			hashtextextended('participant-directory-mutation', 0)
		)`)
	require.NoError(err)

	result := make(chan error, 1)
	go func() {
		_, persistErr := fixture.Store.PersistRepairMessageWithParticipantsContext(
			ctx, fixture.Guard, repairParticipants(),
			func(participantIDs []int64) *store.MessagePersistData {
				return repairedMessageData(fixture, participantIDs, nil)
			},
		)
		result <- persistErr
	}()
	waitForPostgreSQLLockWait(t, fixture.Store, "%pg_advisory_xact_lock%")

	var messageID int64
	require.NoError(blocker.QueryRowContext(ctx,
		`SELECT id FROM messages WHERE id = $1 FOR UPDATE`, fixture.TargetID,
	).Scan(&messageID), "repair must not lock its message while waiting for the participant directory")
	require.Equal(fixture.TargetID, messageID)
	require.NoError(blocker.Commit())
	held = false

	select {
	case persistErr := <-result:
		require.NoError(persistErr)
	case <-ctx.Done():
		require.FailNow("repair did not finish after the directory lock was released", ctx.Err())
	}
}

func TestPersistRepairMessageHonorsCanceledContext(t *testing.T) {
	fixture := seedRepairStoreFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var built atomic.Bool
	_, err := fixture.Store.PersistRepairMessageWithParticipantsContext(
		ctx, fixture.Guard, repairParticipants(),
		func(participantIDs []int64) *store.MessagePersistData {
			built.Store(true)
			return repairedMessageData(fixture, participantIDs, nil)
		},
	)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, built.Load())
}

func TestRecomputeConversationStats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("whatsapp", "+15550000001")
	require.NoError(err, "GetOrCreateSource")

	convID, err := st.EnsureConversationWithType(source.ID, "conv-1", "whatsapp_dm", "Test Chat")
	require.NoError(err, "EnsureConversationWithType")

	// Verify initial message_count is 0 (stats not maintained on insert).
	var initialCount int
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT message_count FROM conversations WHERE id = ?`), convID,
	).Scan(&initialCount), "initial message_count scan")
	assert.Equal(0, initialCount, "initial message_count")

	sentAt := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	msg1 := &store.Message{
		SourceID:        source.ID,
		SourceMessageID: "msg-1",
		ConversationID:  convID,
		MessageType:     "whatsapp",
		SentAt:          sql.NullTime{Time: sentAt, Valid: true},
		Snippet:         sql.NullString{String: "hello", Valid: true},
	}
	_, err = st.UpsertMessage(msg1)
	require.NoError(err, "UpsertMessage msg1")

	sentAt2 := sentAt.Add(time.Hour)
	msg2 := &store.Message{
		SourceID:        source.ID,
		SourceMessageID: "msg-2",
		ConversationID:  convID,
		MessageType:     "whatsapp",
		SentAt:          sql.NullTime{Time: sentAt2, Valid: true},
		Snippet:         sql.NullString{String: "world", Valid: true},
	}
	_, err = st.UpsertMessage(msg2)
	require.NoError(err, "UpsertMessage msg2")

	// msg3 has the SAME sent_at as msg2 but a different snippet.
	// After recompute, last_message_preview must come from msg3 (higher id),
	// exercising the `id DESC` tie-breaker in the SQL.
	msg3 := &store.Message{
		SourceID:        source.ID,
		SourceMessageID: "msg-3",
		ConversationID:  convID,
		MessageType:     "whatsapp",
		SentAt:          sql.NullTime{Time: sentAt2, Valid: true},
		Snippet:         sql.NullString{String: "tie-breaker", Valid: true},
	}
	_, err = st.UpsertMessage(msg3)
	require.NoError(err, "UpsertMessage msg3")

	// Add a conversation participant so participant_count is non-zero.
	participantID, err := st.EnsureParticipantByPhone("+15559876543", "Bob", "whatsapp")
	require.NoError(err, "EnsureParticipantByPhone")
	require.NoError(st.EnsureConversationParticipant(convID, participantID, "member"),
		"EnsureConversationParticipant")

	// Recompute and verify counts.
	require.NoError(st.RecomputeConversationStats(source.ID), "RecomputeConversationStats")

	var count int
	var participantCount int
	var lastMsgAt sql.NullTime
	var preview sql.NullString
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT message_count, participant_count, last_message_at, last_message_preview
		 FROM conversations WHERE id = ?`), convID,
	).Scan(&count, &participantCount, &lastMsgAt, &preview), "post-recompute scan")
	assert.Equal(3, count, "message_count")
	assert.Equal(1, participantCount, "participant_count")
	assert.True(lastMsgAt.Valid, "last_message_at should not be NULL")
	// msg2 and msg3 share the same sent_at; msg3 has the higher id, so its
	// snippet ("tie-breaker") must win via the `id DESC` tie-breaker.
	assert.True(preview.Valid, "preview valid")
	assert.Equal("tie-breaker", preview.String, "last_message_preview")

	// Idempotency: calling again should produce the same result.
	require.NoError(st.RecomputeConversationStats(source.ID), "RecomputeConversationStats (second call)")
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT message_count FROM conversations WHERE id = ?`), convID,
	).Scan(&count), "idempotency scan")
	assert.Equal(3, count, "idempotency message_count")
}

// TestEmbedGen_OrphanImpossibleAndCoverage pins the scan-and-fill
// embed_gen contract:
//   - a freshly-upserted message has embed_gen NULL (column default), so
//     CoverageCounts reports it as missing for any generation — the
//     scan-and-fill worker picks it up with no enqueue step (orphan rows
//     are impossible).
//   - SetEmbedGen stamps it covered; CoverageCounts then reports it
//     embedded.
//   - a subsequent UpsertMessage (ON CONFLICT DO UPDATE) clears embed_gen
//     when the embeddable subject text changes.
func TestEmbedGen_OrphanImpossibleAndCoverage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("gmail", "me@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(source.ID, "conv-1", "email_thread", "Subject")
	require.NoError(err, "EnsureConversationWithType")

	msg := &store.Message{
		SourceID:        source.ID,
		SourceMessageID: "m1",
		ConversationID:  convID,
		MessageType:     "email",
		Subject:         sql.NullString{String: "hello", Valid: true},
	}
	id, err := st.UpsertMessage(msg)
	require.NoError(err, "UpsertMessage")

	const gen = int64(7)
	ctx := t.Context()

	// New row: embed_gen NULL by default -> reported missing for any gen.
	var embedGen sql.NullInt64
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT embed_gen FROM messages WHERE id = ?`), id).Scan(&embedGen))
	assert.False(embedGen.Valid, "new message must have NULL embed_gen (no enqueue, no orphan)")

	live, embedded, _, missing, err := st.CoverageCounts(ctx, gen)
	require.NoError(err, "CoverageCounts (before stamp)")
	assert.Equal(int64(1), live, "one live message")
	assert.Equal(int64(0), embedded, "none embedded yet")
	assert.Equal(int64(1), missing, "the new message is missing")

	// Stamp it covered.
	require.NoError(st.SetEmbedGen(ctx, []int64{id}, gen), "SetEmbedGen")
	live, embedded, _, missing, err = st.CoverageCounts(ctx, gen)
	require.NoError(err, "CoverageCounts (after stamp)")
	assert.Equal(int64(1), live, "still one live message")
	assert.Equal(int64(1), embedded, "now embedded")
	assert.Equal(int64(0), missing, "nothing missing")

	// Re-upsert the same message with changed embedding input: embed_gen must
	// be cleared so the scan-and-fill worker re-embeds it.
	msg.Subject = sql.NullString{String: "hello (edited)", Valid: true}
	_, err = st.UpsertMessage(msg)
	require.NoError(err, "re-UpsertMessage")
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT embed_gen FROM messages WHERE id = ?`), id).Scan(&embedGen))
	assert.False(embedGen.Valid, "subject change must clear embed_gen")
}

func TestMigrateSourceMessageIDRepointsRepliesBeforeDeletingDuplicate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("teams", "user@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(source.ID, "team-1/channel-1", "channel", "General")
	require.NoError(err, "EnsureConversationWithType")

	legacyParentID := insertStoreTestMessage(t, st, source.ID, convID, "m1")
	scopedParentID := insertStoreTestMessage(t, st, source.ID, convID, "channel:team-1:channel-1:m1")
	childID := insertStoreTestMessage(t, st, source.ID, convID, "m2")
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages SET reply_to_message_id = ? WHERE id = ?`),
		legacyParentID, childID,
	)
	require.NoError(err, "seed reply")

	require.NoError(
		st.MigrateSourceMessageID(source.ID, convID, "m1", "channel:team-1:channel-1:m1"),
		"MigrateSourceMessageID",
	)

	var replyTo sql.NullInt64
	err = st.DB().QueryRow(
		st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`),
		childID,
	).Scan(&replyTo)
	require.NoError(err, "scan reply_to_message_id")
	require.True(replyTo.Valid, "reply_to_message_id should remain set")
	assert.Equal(scopedParentID, replyTo.Int64, "reply should point at scoped parent")

	var legacyCount int
	err = st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM messages WHERE id = ?`),
		legacyParentID,
	).Scan(&legacyCount)
	require.NoError(err, "legacy count")
	assert.Equal(0, legacyCount, "legacy duplicate should be deleted")
}

func TestListUnresolvedMessageRepliesReturnsOnlyUnlinkedProviderMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "guild-1")
	require.NoError(err)
	convID, err := st.EnsureConversationWithType(source.ID, "channel-1", "channel", "General")
	require.NoError(err)

	parentID := insertStoreTestMessage(t, st, source.ID, convID, "100")
	childID := insertStoreTestMessage(t, st, source.ID, convID, "101")
	linkedID := insertStoreTestMessage(t, st, source.ID, convID, "102")
	nonReplyID := insertStoreTestMessage(t, st, source.ID, convID, "103")
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET message_type = 'discord' WHERE source_id = ?`), source.ID)
	require.NoError(err)
	require.NoError(st.SetMessageMetadata(childID, sql.NullString{
		String: `{"referenced_message_id":"100","referenced_channel_id":"channel-1"}`, Valid: true,
	}))
	require.NoError(st.SetMessageMetadata(linkedID, sql.NullString{
		String: `{"referenced_message_id":"100"}`, Valid: true,
	}))
	require.NoError(st.SetMessageMetadata(nonReplyID, sql.NullString{
		String: `{"reaction_summaries":[{"emoji":"thumbsup","count":1}]}`, Valid: true,
	}))
	require.NoError(st.SetReplyTo(source.ID, "102", "100"))

	unresolved, err := st.ListUnresolvedMessageReplies(source.ID, "discord")
	require.NoError(err)
	require.Len(unresolved, 1)
	assert.Equal(childID, unresolved[0].MessageID)
	assert.Equal("101", unresolved[0].SourceMessageID)
	assert.JSONEq(`{"referenced_message_id":"100","referenced_channel_id":"channel-1"}`, unresolved[0].Metadata)
	assert.NotZero(parentID)
}

func TestListUnresolvedMessageRepliesAfterUsesBoundedKeysetPages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "guild-keyset")
	require.NoError(err)
	convID, err := st.EnsureConversationWithType(source.ID, "channel-keyset", "channel", "General")
	require.NoError(err)

	var ids []int64
	for _, sourceMessageID := range []string{"101", "102", "103", "104", "105"} {
		messageID := insertStoreTestMessage(t, st, source.ID, convID, sourceMessageID)
		if sourceMessageID != "102" {
			ids = append(ids, messageID)
		}
		metadata := `{"referenced_message_id":"100"}`
		if sourceMessageID == "102" {
			metadata = `{"reaction_summaries":[]}`
		}
		require.NoError(st.SetMessageMetadata(messageID, sql.NullString{
			String: metadata, Valid: true,
		}))
	}
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET message_type = 'discord' WHERE source_id = ?`), source.ID)
	require.NoError(err)

	first, err := st.ListUnresolvedMessageRepliesAfter(source.ID, "discord", 0, 2)
	require.NoError(err)
	require.Len(first, 2)
	wantFirst := ids[:2]
	gotFirst := []int64{first[0].MessageID, first[1].MessageID}
	assert.Equal(wantFirst, gotFirst)
	second, err := st.ListUnresolvedMessageRepliesAfter(source.ID, "discord", first[1].MessageID, 2)
	require.NoError(err)
	require.Len(second, 2)
	wantSecond := ids[2:]
	gotSecond := []int64{second[0].MessageID, second[1].MessageID}
	assert.Equal(wantSecond, gotSecond)
	last, err := st.ListUnresolvedMessageRepliesAfter(source.ID, "discord", second[1].MessageID, 2)
	require.NoError(err)
	assert.Empty(last)
}

func TestMigrateSourceMessageIDClearsTombstoneWhenRenamingLegacyRow(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("teams", "user@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(source.ID, "19:x@thread.v2", "direct_chat", "DM")
	require.NoError(err, "EnsureConversationWithType")
	_ = insertStoreTestMessage(t, st, source.ID, convID, "m1")
	require.NoError(st.MarkMessageDeleted(source.ID, "m1"), "MarkMessageDeleted")

	require.NoError(
		st.MigrateSourceMessageID(source.ID, convID, "m1", "chat:19:x@thread.v2:m1"),
		"MigrateSourceMessageID",
	)

	assertSourceMessageIDNotDeleted(t, st, source.ID, "chat:19:x@thread.v2:m1")
}

func TestMigrateSourceMessageIDClearsTombstoneOnExistingScopedRow(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("teams", "user@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(source.ID, "19:x@thread.v2", "direct_chat", "DM")
	require.NoError(err, "EnsureConversationWithType")
	_ = insertStoreTestMessage(t, st, source.ID, convID, "chat:19:x@thread.v2:m1")
	require.NoError(st.MarkMessageDeleted(source.ID, "chat:19:x@thread.v2:m1"), "MarkMessageDeleted")

	require.NoError(
		st.MigrateSourceMessageID(source.ID, convID, "m1", "chat:19:x@thread.v2:m1"),
		"MigrateSourceMessageID",
	)

	assertSourceMessageIDNotDeleted(t, st, source.ID, "chat:19:x@thread.v2:m1")
}

func TestMessageSourceIDsInSnowflakeInterval(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "123456789012345678")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "234567890123456789", "channel", "general",
	)
	require.NoError(err)

	for _, sourceMessageID := range []string{
		"99",
		"100",
		"101",
		"9223372036854775807",
		"9223372036854775808",
		"10000000000000000000",
		"1000000000000000000a",
		"09223372036854775808",
		"18446744073709551615",
		"18446744073709551616",
	} {
		insertStoreTestMessage(t, st, source.ID, conversationID, sourceMessageID)
	}

	otherConversationID, err := st.EnsureConversationWithType(
		source.ID, "234567890123456790", "channel", "random",
	)
	require.NoError(err)
	insertStoreTestMessage(t, st, source.ID, otherConversationID, "10000000000000000001")

	otherSource, err := st.GetOrCreateSource("discord", "999999999999999999")
	require.NoError(err)
	otherSourceConversationID, err := st.EnsureConversationWithType(
		otherSource.ID, "234567890123456789", "channel", "general",
	)
	require.NoError(err)
	insertStoreTestMessage(t, st, otherSource.ID, otherSourceConversationID, "10000000000000000002")

	got, err := st.MessageSourceIDsInSnowflakeInterval(
		source.ID, conversationID, "9223372036854775807", "18446744073709551615",
	)
	require.NoError(err)
	assert.Equal([]string{
		"9223372036854775808",
		"10000000000000000000",
		"18446744073709551615",
	}, got)

	adjacent, err := st.MessageSourceIDsInSnowflakeInterval(source.ID, conversationID, "99", "100")
	require.NoError(err)
	assert.Equal([]string{"100"}, adjacent)

	zeroPadded, err := st.MessageSourceIDsInSnowflakeInterval(
		source.ID, conversationID, "0009223372036854775807", "00018446744073709551615",
	)
	require.NoError(err)
	assert.Equal(got, zeroPadded)
}

func TestMessageSourceIDsInSnowflakeIntervalPageAndMaximum(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "paged-guild")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "paged-channel", "channel", "general")
	require.NoError(err)
	for _, sourceMessageID := range []string{"100", "101", "102", "103", "104"} {
		insertStoreTestMessage(t, st, source.ID, conversationID, sourceMessageID)
	}

	maximum, err := st.MaxMessageSourceIDInSnowflakeInterval(source.ID, conversationID, "99", "104")
	require.NoError(err)
	assert.Equal("104", maximum)
	first, err := st.MessageSourceIDsInSnowflakeIntervalPage(
		source.ID, conversationID, "99", "104", "", 2,
	)
	require.NoError(err)
	assert.Equal([]string{"104", "103"}, first)
	second, err := st.MessageSourceIDsInSnowflakeIntervalPage(
		source.ID, conversationID, "99", "104", first[len(first)-1], 2,
	)
	require.NoError(err)
	assert.Equal([]string{"102", "101"}, second)
	last, err := st.MessageSourceIDsInSnowflakeIntervalPage(
		source.ID, conversationID, "99", "104", second[len(second)-1], 2,
	)
	require.NoError(err)
	assert.Equal([]string{"100"}, last)
}

func TestMessageSourceIDsInSnowflakeIntervalRejectsUnsafeBounds(t *testing.T) {
	st := testutil.NewTestStore(t)

	for _, tc := range []struct {
		name  string
		lower string
		upper string
	}{
		{name: "empty lower", lower: "", upper: "100"},
		{name: "empty upper", lower: "99", upper: ""},
		{name: "malformed lower", lower: "9x", upper: "100"},
		{name: "malformed upper", lower: "99", upper: "10_0"},
		{name: "reversed", lower: "101", upper: "100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			_, err := st.MessageSourceIDsInSnowflakeInterval(1, 1, tc.lower, tc.upper)
			require.Error(err)
		})
	}
}

func TestClearMessageDeletedFromSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "123456789012345678")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "234567890123456789", "channel", "general",
	)
	require.NoError(err)
	insertStoreTestMessage(t, st, source.ID, conversationID, "345678901234567890")
	require.NoError(st.MarkMessageDeleted(source.ID, "345678901234567890"))

	otherSource, err := st.GetOrCreateSource("discord", "999999999999999999")
	require.NoError(err)
	otherConversationID, err := st.EnsureConversationWithType(
		otherSource.ID, "888888888888888888", "channel", "other",
	)
	require.NoError(err)
	insertStoreTestMessage(t, st, otherSource.ID, otherConversationID, "345678901234567890")
	require.NoError(st.MarkMessageDeleted(otherSource.ID, "345678901234567890"))

	require.NoError(st.ClearMessageDeletedFromSource(source.ID, "345678901234567890"))
	assertSourceMessageIDNotDeleted(t, st, source.ID, "345678901234567890")

	var otherDeletedAt sql.NullTime
	err = st.DB().QueryRow(
		st.Rebind(`SELECT deleted_from_source_at FROM messages WHERE source_id = ? AND source_message_id = ?`),
		otherSource.ID, "345678901234567890",
	).Scan(&otherDeletedAt)
	require.NoError(err)
	assert.True(otherDeletedAt.Valid)
}

func TestReconcileSourceMessageSnapshotIsSourceScopedAndGenerationFenced(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("gmail", "primary@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "primary-thread", "email_thread", "Primary",
	)
	require.NoError(err)
	insertStoreTestMessage(t, st, source.ID, conversationID, "missing")
	insertStoreTestMessage(t, st, source.ID, conversationID, "present")

	otherSource, err := st.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err)
	otherConversationID, err := st.EnsureConversationWithType(
		otherSource.ID, "other-thread", "email_thread", "Other",
	)
	require.NoError(err)
	insertStoreTestMessage(t, st, otherSource.ID, otherConversationID, "missing")

	syncID, err := st.StartSync(source.ID, "full")
	require.NoError(err)
	scoped := st.ScopedToSync(source.ID, syncID)
	reconciled, err := scoped.ReconcileSourceMessageSnapshot(
		t.Context(), source.ID, map[string]struct{}{"present": {}},
	)
	require.NoError(err)
	assert.Equal(int64(1), reconciled)
	assertMessageDeletedFromSource(t, st, source.ID, "missing", true)
	assertMessageDeletedFromSource(t, st, source.ID, "present", false)
	assertMessageDeletedFromSource(t, st, otherSource.ID, "missing", false)

	staleSyncID, err := st.StartSync(source.ID, "full")
	require.NoError(err)
	stale := st.ScopedToSync(source.ID, staleSyncID)
	_, err = st.StartSync(source.ID, "full")
	require.NoError(err)
	_, err = stale.ReconcileSourceMessageSnapshot(t.Context(), source.ID, map[string]struct{}{})
	require.ErrorIs(err, store.ErrSyncRunSuperseded)
}

func TestMarkMessagesDeletedFromReaderIsAtomicOnLateReadFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "atomic-delete-guild")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "atomic-channel", "channel", "general")
	require.NoError(err)
	for _, sourceMessageID := range []string{"101", "102"} {
		insertStoreTestMessage(t, st, source.ID, conversationID, sourceMessageID)
	}
	reader := io.MultiReader(strings.NewReader("101\n102\n"), failingMessageIDReader{})

	err = st.MarkMessagesDeletedFromReader(source.ID, reader, 1)
	require.ErrorContains(err, "synthetic staged-ID read failure")
	var deleted int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages
		WHERE source_id = ? AND deleted_from_source_at IS NOT NULL
	`), source.ID).Scan(&deleted))
	assert.Zero(deleted, "late reader failure rolls back earlier bounded batches")

	require.NoError(st.MarkMessagesDeletedFromReader(source.ID, strings.NewReader("101\n102\n"), 1))
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages
		WHERE source_id = ? AND deleted_from_source_at IS NOT NULL
	`), source.ID).Scan(&deleted))
	assert.Equal(2, deleted)
}

func insertStoreTestMessage(t *testing.T, st *store.Store, sourceID, convID int64, sourceMessageID string) int64 {
	t.Helper()
	msg := &store.Message{
		SourceID:        sourceID,
		SourceMessageID: sourceMessageID,
		ConversationID:  convID,
		MessageType:     "teams",
		SentAt:          sql.NullTime{Time: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC), Valid: true},
		Snippet:         sql.NullString{String: sourceMessageID, Valid: true},
	}
	id, err := st.UpsertMessage(msg)
	require.NoError(t, err, "UpsertMessage "+sourceMessageID)
	return id
}

func assertSourceMessageIDNotDeleted(t *testing.T, st *store.Store, sourceID int64, sourceMessageID string) {
	t.Helper()
	var deletedAt sql.NullTime
	err := st.DB().QueryRow(
		st.Rebind(`SELECT deleted_from_source_at FROM messages WHERE source_id = ? AND source_message_id = ?`),
		sourceID, sourceMessageID,
	).Scan(&deletedAt)
	require.NoError(t, err, "scan deleted_from_source_at")
	assert.False(t, deletedAt.Valid, "deleted_from_source_at should be cleared")
}

func assertMessageDeletedFromSource(
	t *testing.T, st *store.Store, sourceID int64, sourceMessageID string, want bool,
) {
	t.Helper()
	var deletedAt sql.NullTime
	err := st.DB().QueryRow(
		st.Rebind(`SELECT deleted_from_source_at FROM messages WHERE source_id = ? AND source_message_id = ?`),
		sourceID, sourceMessageID,
	).Scan(&deletedAt)
	require.NoError(t, err, "scan deleted_from_source_at")
	assert.Equal(t, want, deletedAt.Valid, "deleted_from_source_at validity")
}

func TestEnsureParticipantByPhone_IdentifierType(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	// Create participant via WhatsApp
	id1, err := st.EnsureParticipantByPhone("+15551234567", "Alice", "whatsapp")
	require.NoError(err, "EnsureParticipantByPhone(whatsapp)")
	require.NotZero(id1, "expected non-zero participant ID")

	// Same phone via iMessage — should return the same participant ID
	id2, err := st.EnsureParticipantByPhone("+15551234567", "Alice", "imessage")
	require.NoError(err, "EnsureParticipantByPhone(imessage)")
	assert.Equal(id1, id2, "imessage call should return same participant ID as whatsapp")

	// Both participant_identifiers rows should exist
	var count int
	err = st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM participant_identifiers WHERE participant_id = ?`),
		id1,
	).Scan(&count)
	require.NoError(err, "count participant_identifiers")
	assert.Equal(2, count, "participant_identifiers count")

	// Verify each identifier type is present
	for _, identType := range []string{"whatsapp", "imessage"} {
		var exists int
		err = st.DB().QueryRow(
			st.Rebind(`SELECT COUNT(*) FROM participant_identifiers
			 WHERE participant_id = ? AND identifier_type = ?`),
			id1, identType,
		).Scan(&exists)
		require.NoError(err, "check identifier_type %q", identType)
		assert.Equal(1, exists, "identifier_type %q", identType)
	}
}

func TestUpdateParticipantDisplayNameByEmail(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	// Create an unnamed email participant (e.g. inserted by iMessage import
	// for an Apple ID handle).
	var pid int64
	require.NoError(st.DB().QueryRow(
		st.Rebind(`INSERT INTO participants (email_address) VALUES (?) RETURNING id`),
		"alice@example.com",
	).Scan(&pid), "insert participant")

	// Backfilling on an empty display_name succeeds.
	updated, err := st.UpdateParticipantDisplayNameByEmail("alice@example.com", "Alice Example")
	require.NoError(err, "UpdateParticipantDisplayNameByEmail")
	require.True(updated, "expected backfill to update existing participant")

	got := readDisplayName(t, st, pid)
	assert.Equal("Alice Example", got, "display_name")

	// Lookup is case-insensitive on the email.
	updatedMixed, err := st.UpdateParticipantDisplayNameByEmail("ALICE@example.com", "Should Not Overwrite")
	require.NoError(err, "UpdateParticipantDisplayNameByEmail (case)")
	assert.False(updatedMixed, "second update should not modify a non-empty display_name")
	assert.Equal("Alice Example", readDisplayName(t, st, pid), "display_name should not be overwritten")

	// Empty inputs are no-ops.
	updated, err = st.UpdateParticipantDisplayNameByEmail("", "X")
	require.NoError(err, "empty email err")
	assert.False(updated, "empty email updated")
	updated, err = st.UpdateParticipantDisplayNameByEmail("x@y.com", "")
	require.NoError(err, "empty name err")
	assert.False(updated, "empty name updated")

	// Unknown email is a no-op (does not create rows).
	updated, err = st.UpdateParticipantDisplayNameByEmail("nobody@example.com", "Nobody")
	require.NoError(err, "unknown email err")
	assert.False(updated, "unknown email updated")
}

func TestUpdateImessageParticipantDisplayNameByPhone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	// Case 1: legacy iMessage participant with display_name = phone_number.
	// Should be overwritten by the contact name.
	legacyID, err := st.EnsureParticipantByPhone("+15551111111", "+15551111111", "imessage")
	require.NoError(err, "seed legacy")

	// Case 2: iMessage participant already named by another source. Real
	// name must be preserved.
	namedID, err := st.EnsureParticipantByPhone("+15552222222", "Bob From Gmail", "imessage")
	require.NoError(err, "seed named")

	// Case 3: WhatsApp-only participant with display_name = phone_number.
	// Not iMessage, must NOT be touched (no imessage identifier exists).
	otherID, err := st.EnsureParticipantByPhone("+15553333333", "+15553333333", "whatsapp")
	require.NoError(err, "seed other")

	// Apply contact-name backfill.
	updated, err := st.UpdateImessageParticipantDisplayNameByPhone("+15551111111", "Alice Real")
	require.NoError(err, "backfill legacy")
	assert.True(updated, "legacy placeholder should be replaced")
	assert.Equal("Alice Real", readDisplayName(t, st, legacyID), "legacy display_name")

	updated, err = st.UpdateImessageParticipantDisplayNameByPhone("+15552222222", "Should Not Win")
	require.NoError(err, "backfill named")
	assert.False(updated, "real name from another source should be preserved")
	assert.Equal("Bob From Gmail", readDisplayName(t, st, namedID), "named display_name")

	updated, err = st.UpdateImessageParticipantDisplayNameByPhone("+15553333333", "Not Allowed")
	require.NoError(err, "backfill other")
	assert.False(updated, "non-iMessage participant should not be touched")
	assert.Equal("+15553333333", readDisplayName(t, st, otherID), "non-iMessage display_name")

	// Empty inputs are no-ops.
	updated, err = st.UpdateImessageParticipantDisplayNameByPhone("", "X")
	require.NoError(err, "empty phone err")
	assert.False(updated, "empty phone updated")
	updated, err = st.UpdateImessageParticipantDisplayNameByPhone("+15551111111", "")
	require.NoError(err, "empty name err")
	assert.False(updated, "empty name updated")
}

func TestRetitleImessageChats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	src, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(err, "source")

	otherSrc, err := st.GetOrCreateSource("whatsapp", "+15550000000")
	require.NoError(err, "other source")

	// Named iMessage participant whose phone is the current title of a 1:1.
	namedID, err := st.EnsureParticipantByPhone("+15551111111", "Alice Real", "imessage")
	require.NoError(err, "seed alice")

	// Email-backed iMessage participants did not always get an iMessage
	// participant_identifiers row, but the apple_messages conversation is
	// still enough context to safely refresh the raw email title.
	emailID, err := st.EnsureParticipant("alice@example.com", "Alice Email", "example.com")
	require.NoError(err, "seed alice email")

	// iMessage participant whose name is still the phone (poisoned). Must
	// not be used as a title.
	poisonedID, err := st.EnsureParticipantByPhone("+15552222222", "+15552222222", "imessage")
	require.NoError(err, "seed poisoned")

	// Non-iMessage participant whose phone is a conversation title — must
	// not be touched even if a real name exists elsewhere.
	whatsappID, err := st.EnsureParticipantByPhone("+15553333333", "Carol", "whatsapp")
	require.NoError(err, "seed carol")

	// 1:1 with named participant — title is the phone, should be replaced.
	convNamedID, err := st.EnsureConversationWithType(src.ID, "imsg-1", "direct_chat", "+15551111111")
	require.NoError(err, "conv named")
	require.NoError(st.EnsureConversationParticipant(convNamedID, namedID, "member"), "link named")

	// 1:1 with email participant — title is the raw email, should be replaced.
	convEmailID, err := st.EnsureConversationWithType(src.ID, "imsg-email-1", "direct_chat", "alice@example.com")
	require.NoError(err, "conv email")
	require.NoError(st.EnsureConversationParticipant(convEmailID, emailID, "member"), "link email")

	// 1:1 with poisoned participant — title equals phone but participant
	// has no real name yet. Must remain unchanged.
	convPoisonedID, err := st.EnsureConversationWithType(src.ID, "imsg-2", "direct_chat", "+15552222222")
	require.NoError(err, "conv poisoned")
	require.NoError(st.EnsureConversationParticipant(convPoisonedID, poisonedID, "member"), "link poisoned")

	// Non-iMessage 1:1 — title is a phone, but the source isn't apple_messages.
	convOtherID, err := st.EnsureConversationWithType(otherSrc.ID, "wa-1", "direct_chat", "+15553333333")
	require.NoError(err, "conv other")
	require.NoError(st.EnsureConversationParticipant(convOtherID, whatsappID, "member"), "link other")

	// Group chat whose title was generated from raw participant handles
	// before contacts were backfilled. It should be regenerated with names.
	bobID, err := st.EnsureParticipantByPhone("+15554444444", "Bob Real", "imessage")
	require.NoError(err, "seed bob")
	carolID, err := st.EnsureParticipantByPhone("+15555555555", "Carol Real", "imessage")
	require.NoError(err, "seed carol")
	daveID, err := st.EnsureParticipantByPhone("+15556666666", "Dave Real", "imessage")
	require.NoError(err, "seed dave")
	convGroupID, err := st.EnsureConversationWithType(
		src.ID, "imsg-group-1", "group_chat",
		"+15551111111, +15554444444, +15555555555 +1 more",
	)
	require.NoError(err, "conv group")
	for _, pid := range []int64{namedID, bobID, carolID, daveID} {
		require.NoError(st.EnsureConversationParticipant(convGroupID, pid, "member"),
			"link group participant %d", pid)
	}

	// Named group chats must not be overwritten, even when the participant
	// list would allow a generated title.
	convNamedGroupID, err := st.EnsureConversationWithType(
		src.ID, "imsg-group-2", "group_chat", "Road trip",
	)
	require.NoError(err, "conv named group")
	for _, pid := range []int64{namedID, bobID, carolID} {
		require.NoError(st.EnsureConversationParticipant(convNamedGroupID, pid, "member"),
			"link named group participant %d", pid)
	}

	n, err := st.RetitleImessageChats()
	require.NoError(err, "RetitleImessageChats")
	assert.Equal(int64(3), n, "rows updated")

	assert.Equal("Alice Real", readConvTitle(t, st, convNamedID), "named conv title")
	assert.Equal("Alice Email", readConvTitle(t, st, convEmailID), "email conv title")
	assert.Equal("+15552222222", readConvTitle(t, st, convPoisonedID), "poisoned conv title (unchanged)")
	assert.Equal("+15553333333", readConvTitle(t, st, convOtherID), "non-imessage conv title (unchanged)")
	assert.Equal("Alice Real, Bob Real, Carol Real +1 more",
		readConvTitle(t, st, convGroupID), "group conv title (refreshed generated title)")
	assert.Equal("Road trip", readConvTitle(t, st, convNamedGroupID), "named group conv title (unchanged)")

	// Idempotent: running again is a no-op.
	n2, err := st.RetitleImessageChats()
	require.NoError(err, "idempotent rerun err")
	assert.Equal(int64(0), n2, "idempotent rerun rows")
}

func readConvTitle(t *testing.T, st *store.Store, id int64) string {
	t.Helper()
	var title sql.NullString
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT title FROM conversations WHERE id = ?`), id,
	).Scan(&title), "scan title")
	return title.String
}

func readDisplayName(t *testing.T, st *store.Store, pid int64) string {
	t.Helper()
	var name sql.NullString
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT display_name FROM participants WHERE id = ?`), pid,
	).Scan(&name), "scan display_name")
	return name.String
}

func TestCountMessagesPerMailbox(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("imap", "user@example.com")
	require.NoError(err, "GetOrCreateSource")

	convID, err := st.EnsureConversation(source.ID, "conv-1", "Test Chat")
	require.NoError(err, "EnsureConversation")

	// Create labels for each folder/mbox
	labelMap, err := st.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{
		"INBOX":  {Name: "INBOX", Type: "system"},
		"Sent":   {Name: "Sent", Type: "system"},
		"Drafts": {Name: "Drafts", Type: "system"},
	})
	require.NoError(err, "EnsureLabelsBatch")

	// Insert messages
	msgs := []*store.Message{
		{SourceID: source.ID, SourceMessageID: "INBOX|100", ConversationID: convID, MessageType: "email"},
		{SourceID: source.ID, SourceMessageID: "INBOX|200", ConversationID: convID, MessageType: "email"},
		{SourceID: source.ID, SourceMessageID: "Sent|300", ConversationID: convID, MessageType: "email"},
		{SourceID: source.ID, SourceMessageID: "Drafts|400", ConversationID: convID, MessageType: "email"},
	}

	msgIDs := make([]int64, len(msgs))
	for i, msg := range msgs {
		msgIDs[i], err = st.UpsertMessage(msg)
		require.NoError(err, "UpsertMessage")
	}

	// Associate messages with labels (mimicking folder membership)
	require.NoError(st.LinkMessageLabel(msgIDs[0], labelMap["INBOX"]))
	require.NoError(st.LinkMessageLabel(msgIDs[1], labelMap["INBOX"]))
	require.NoError(st.LinkMessageLabel(msgIDs[2], labelMap["Sent"]))
	require.NoError(st.LinkMessageLabel(msgIDs[3], labelMap["Drafts"]))

	counts, err := st.CountMessagesPerMailbox(source.ID)
	require.NoError(err, "CountMessagesPerMailbox")
	assert.Equal(int64(2), counts["INBOX"], "INBOX count")
	assert.Equal(int64(1), counts["Sent"], "Sent count")
	assert.Equal(int64(1), counts["Drafts"], "Drafts count")
}
