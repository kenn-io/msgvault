package imazingcsv

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImporterMessagesConversationsAndIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice & Bob", "2024-06-01 12:00:00", "2024-06-01 12:00:05", "2024-06-01 12:01:00", "iMessage", "Outgoing", "", "", "Delivered", "", "Greeting", "hello from me", "", ""},
		{"Alice & Bob", "2024-06-01 12:02:00", "", "", "SMS", "Incoming", "+1 (555) 000-0002", "Alice", "", "", "", "hello from Alice", "", ""},
		{"Alice & Bob", "2024-06-01T12:03:00Z", "", "2024-06-01T12:04:00Z", "RCS", "Incoming", "BOB@EXAMPLE.TEST", "Bob", "Read", "", "", "hello from Bob", "", ""},
	})

	summary, err := NewImporter(st, Options{
		Owner: "+1 (555) 000-0001", Timezone: "UTC",
	}).ImportPath(context.Background(), exportDir)
	require.NoError(err)
	assert.Equal(Summary{Files: 1, Conversations: 1, Messages: 3, Participants: 3}, summary)

	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	require.True(source.SyncConfig.Valid)
	assert.JSONEq(`{"ambiguous_time_policy":"earlier","timezone":"UTC"}`, source.SyncConfig.String)

	var conversationID int64
	var conversationType, title string
	var messageCount, participantCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT id, conversation_type, title, message_count, participant_count
		FROM conversations WHERE source_id = ?`), source.ID).Scan(
		&conversationID, &conversationType, &title, &messageCount, &participantCount,
	))
	assert.Equal("group_chat", conversationType)
	assert.Equal("Alice & Bob", title)
	assert.Equal(3, messageCount)
	assert.Equal(3, participantCount)

	rows, err := st.DB().Query(st.Rebind(`
		SELECT COALESCE(phone_number, ''), COALESCE(email_address, ''), COALESCE(display_name, '')
		FROM participants
		WHERE id IN (SELECT participant_id FROM conversation_participants WHERE conversation_id = ?)
		ORDER BY COALESCE(phone_number, email_address)`), conversationID)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var participants []string
	for rows.Next() {
		var phone, email, name string
		require.NoError(rows.Scan(&phone, &email, &name))
		participants = append(participants, phone+"|"+email+"|"+name)
	}
	require.NoError(rows.Err())
	sort.Strings(participants)
	assert.Equal([]string{
		"+15550000001||",
		"+15550000002||Alice",
		"|bob@example.test|Bob",
	}, participants)

	var messageID int64
	var deliveredAt sql.NullTime
	var isDelivered, isRead bool
	var readAt, metadata sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT m.id, m.delivered_at, m.is_delivered, m.is_read,
		       CAST(m.read_at AS TEXT), CAST(m.metadata AS TEXT)
		FROM messages m
		JOIN message_bodies b ON b.message_id = m.id
		WHERE m.source_id = ? AND b.body_text = ?`), source.ID, "hello from me").Scan(
		&messageID, &deliveredAt, &isDelivered, &isRead, &readAt, &metadata,
	))
	assert.True(deliveredAt.Valid)
	assert.True(isDelivered)
	assert.True(isRead)
	assert.False(readAt.Valid)
	require.True(metadata.Valid)
	assert.JSONEq(`{
		"format":"imazing_csv",
		"status":"Delivered",
		"delivered_date":"2024-06-01 12:00:05",
		"read_date":"2024-06-01 12:01:00",
		"subject":"Greeting",
		"text":"hello from me",
		"file":"messages.csv",
		"record":2
	}`, metadata.String)

	raw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	assert.JSONEq(`{"Chat Session":"Alice & Bob","Message Date":"2024-06-01 12:00:00","Delivered Date":"2024-06-01 12:00:05","Read Date":"2024-06-01 12:01:00","Service":"iMessage","Type":"Outgoing","Sender ID":"","Sender Name":"","Status":"Delivered","Replying to":"","Subject":"Greeting","Text":"hello from me","Attachment":"","Attachment type":""}`, string(raw))

	var rawFormat string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT raw_format FROM message_raw WHERE message_id = ?`), messageID).Scan(&rawFormat))
	assert.Equal(RawFormat, rawFormat)

	var readReceiptImpliesDelivered bool
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT m.is_delivered FROM messages m
		JOIN message_bodies b ON b.message_id = m.id
		WHERE m.source_id = ? AND b.body_text = ?`),
		source.ID, "hello from Bob").Scan(&readReceiptImpliesDelivered))
	assert.True(readReceiptImpliesDelivered)
}

func TestImporterDoesNotDuplicateSoleUnnamedParticipantFromTitle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Mom", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "hello", "", ""},
		{"Mom", "2024-06-01 12:01:00", "", "", "iMessage", "Incoming", "+15550000002", "", "", "", "", "reply", "", ""},
	})

	summary, err := NewImporter(st, Options{
		Owner: "+15550000001", Timezone: "UTC",
	}).ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Equal(2, summary.Participants)

	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	var conversationType string
	var participantCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT conversation_type, participant_count
		FROM conversations WHERE source_id = ?`), source.ID).Scan(
		&conversationType, &participantCount,
	))
	assert.Equal("direct_chat", conversationType)
	assert.Equal(2, participantCount)
}

func TestImporterTitleDoesNotInventParticipants(t *testing.T) {
	for _, tc := range []struct {
		title       string
		senders     []string
		wantMembers int
		wantType    string
	}{
		{"Family", []string{"Alice", "Bob"}, 3, "group_chat"},
		{"Example & Sample Pizza", []string{"Example & Sample Pizza"}, 2, "direct_chat"},
		{"Family", nil, 1, "direct_chat"},
		{"Alice & Bob & Carol", []string{"Alice", "Bob"}, 4, "group_chat"},
	} {
		t.Run(tc.title, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			rows := [][]string{
				{tc.title, "2024-06-01 12:00:00", "", "", "SMS", "Outgoing", "", "", "", "", "", "hello", "", ""},
			}
			for i, name := range tc.senders {
				rows = append(rows, []string{tc.title, "2024-06-01 12:01:00", "", "", "SMS", "Incoming",
					[]string{"+15550000002", "+15550000003"}[i], name, "", "", "", "reply", "", ""})
			}
			exportDir := newTestExport(t, rows)
			_, err := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"}).ImportPath(t.Context(), exportDir)
			require.NoError(err)
			var members int
			var kind string
			require.NoError(st.DB().QueryRow(`SELECT conversation_type,
				(SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = conversations.id)
				FROM conversations`).Scan(&kind, &members))
			assert.Equal(tc.wantMembers, members)
			assert.Equal(tc.wantType, kind)
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants`).Scan(&members))
			assert.Equal(tc.wantMembers, members, "no unused title identities reach People")
		})
	}
}

func TestImporterRerunAndRosterGrowthKeepStableRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	rows := [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "hello", "", ""},
	}
	exportDir := newTestExport(t, rows)
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	firstSummary, err := importer.ImportPath(context.Background(), exportDir)
	require.NoError(err)
	assert.Equal(1, firstSummary.Messages)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	firstMessages := sourceMessageIDs(t, st, source.ID)
	firstConversation := singleConversationID(t, st, source.ID)
	var firstMessageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`),
		source.ID, firstMessages[0]).Scan(&firstMessageID))
	_, err = st.DB().Exec(st.Rebind(`
		UPDATE messages SET is_read = FALSE, read_at = '2026-09-14 12:00:00'
		WHERE id = ?`), firstMessageID)
	require.NoError(err)

	_, err = importer.ImportPath(context.Background(), exportDir)
	require.NoError(err)
	assert.Equal(firstMessages, sourceMessageIDs(t, st, source.ID))
	assert.Equal(firstConversation, singleConversationID(t, st, source.ID))
	var isRead bool
	var readAt sql.NullTime
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT is_read, read_at FROM messages WHERE id = ?`), firstMessageID).Scan(&isRead, &readAt))
	assert.False(isRead)
	assert.True(readAt.Valid)

	rows = append(rows,
		[]string{"Alice", "2024-06-01 12:01:00", "", "", "iMessage", "Incoming", "+15550000002", "Alice", "", "", "", "reply", "", ""},
		rows[0],
	)
	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), rows)
	secondSummary, err := importer.ImportPath(context.Background(), exportDir)
	require.NoError(err)
	assert.Equal(3, secondSummary.Messages)
	secondMessages := sourceMessageIDs(t, st, source.ID)
	require.Len(secondMessages, 3)
	assert.Equal(firstMessages[0], secondMessages[0])
	assert.Equal(firstConversation, singleConversationID(t, st, source.ID))

	var conversationParticipants int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = ?`),
		firstConversation).Scan(&conversationParticipants))
	assert.Equal(2, conversationParticipants)
}

func TestImporterContractedExportPreservesHistoricalConversationRosterAndType(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	first := []string{"Team", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Alice", "", "", "", "hello", "", ""}
	second := []string{"Team", "2024-06-01 12:01:00", "", "", "iMessage", "Incoming", "+15550000003", "Bob", "", "", "", "hi", "", ""}
	exportDir := newTestExport(t, [][]string{first})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	conversationID := singleConversationID(t, st, source.ID)
	var conversationType string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT conversation_type FROM conversations WHERE id = ?`), conversationID).Scan(&conversationType))
	require.Equal("direct_chat", conversationType)

	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{first, second})
	_, err = importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	var initialParticipantCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = ?`),
		conversationID).Scan(&initialParticipantCount))
	require.Greater(initialParticipantCount, 2)
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT conversation_type FROM conversations WHERE id = ?`), conversationID).Scan(&conversationType))
	require.Equal("group_chat", conversationType)

	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{first})
	_, err = importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)

	var participantCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT conversation_type,
		       (SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = conversations.id)
		FROM conversations WHERE id = ?`), conversationID).Scan(&conversationType, &participantCount))
	assert.Equal("group_chat", conversationType)
	assert.Equal(initialParticipantCount, participantCount)
}

func TestImporterContractedExportOnlyPreservesAffectedConversation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	teamAlice := []string{"Team", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Alice", "", "", "", "team alice", "", ""}
	teamBob := []string{"Team", "2024-06-01 12:01:00", "", "", "iMessage", "Incoming", "+15550000003", "Bob", "", "", "", "team bob", "", ""}
	otherCarol := []string{"Other", "2024-06-01 12:02:00", "", "", "iMessage", "Incoming", "+15550000004", "Carol", "", "", "", "other carol", "", ""}
	exportDir := newTestExport(t, [][]string{teamAlice, teamBob, otherCarol})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	var teamID, otherID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?`),
		source.ID, conversationKey("Team")).Scan(&teamID))
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?`),
		source.ID, conversationKey("Other")).Scan(&otherID))
	// Simulate stale derived conversation state in the unaffected conversation.
	// A complete rerun must repair it even while another conversation is
	// contracted and therefore preserves its historical roster.
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO conversation_participants (conversation_id, participant_id)
		SELECT ?, participant_id FROM conversation_participants
		WHERE conversation_id = ? AND participant_id NOT IN (
			SELECT participant_id FROM conversation_participants WHERE conversation_id = ?
		) LIMIT 1`), otherID, teamID, otherID)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(
		`UPDATE conversations SET conversation_type = 'group_chat' WHERE id = ?`), otherID)
	require.NoError(err)

	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{teamAlice, otherCarol})
	_, err = importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)

	var teamType, otherType string
	var teamParticipants, otherParticipants int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT conversation_type,
		       (SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = conversations.id)
		FROM conversations WHERE source_id = ? AND source_conversation_id = ?`),
		source.ID, conversationKey("Team")).Scan(&teamType, &teamParticipants))
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT conversation_type,
		       (SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = conversations.id)
		FROM conversations WHERE source_id = ? AND source_conversation_id = ?`),
		source.ID, conversationKey("Other")).Scan(&otherType, &otherParticipants))
	assert.Equal("group_chat", teamType)
	assert.GreaterOrEqual(teamParticipants, 3)
	assert.Equal("direct_chat", otherType)
	assert.Equal(2, otherParticipants)
}

func TestImporterInsertingIdenticalDuplicateRowKeepsExistingRowsStable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	duplicate := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "ping", "", ""}
	exportDir := newTestExport(t, [][]string{duplicate, duplicate})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	first, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	require.Equal(2, first.Messages)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	firstIDs := sourceMessageIDs(t, st, source.ID)
	require.Len(firstIDs, 2)
	labelID, err := st.EnsureLabel(source.ID, "imazing-duplicate-label", "Duplicates", "user")
	require.NoError(err)
	firstMessageIDs := make([]int64, 0, len(firstIDs))
	for _, sourceMessageID := range firstIDs {
		var messageID int64
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`),
			source.ID, sourceMessageID).Scan(&messageID))
		_, err := st.ReconcileMessageLabels(messageID, []int64{labelID}, false)
		require.NoError(err)
		firstMessageIDs = append(firstMessageIDs, messageID)
	}

	// A later export contains one more identical row before the existing
	// ones, shifting the physical record numbers of every later row.
	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"),
		[][]string{duplicate, duplicate, duplicate})
	second, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	require.Equal(3, second.Messages)
	secondIDs := sourceMessageIDs(t, st, source.ID)
	require.Len(secondIDs, 3)
	assert.Equal(firstIDs, secondIDs[:len(firstIDs)], "existing duplicate rows keep their source IDs")

	for _, messageID := range firstMessageIDs {
		var labelCount int
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT COUNT(*) FROM message_labels WHERE message_id = ? AND label_id = ?`),
			messageID, labelID).Scan(&labelCount))
		assert.Equal(1, labelCount, "existing message %d keeps its label", messageID)
	}
	var messageCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&messageCount))
	assert.Equal(3, messageCount, "exactly one new duplicate row is added")
}

func TestImporterInsertedDuplicateRowBeforePreservesExistingReplyEvidenceAndLabels(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	// The plain and replying rows share every identity-bearing field, so they
	// form one duplicate occurrence group; only the reply evidence differs.
	parent := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Alice", "", "", "", "original body", "", ""}
	plain := []string{"Alice", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "my reply", "", ""}
	replying := []string{"Alice", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "Replying to:\nAlice (+15550000002)\n2024-06-01 12:00:00\noriginal body", "", "my reply", "", ""}
	exportDir := newTestExport(t, [][]string{parent, plain, replying})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	first, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	require.Equal(1, first.RepliesLinked)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	parentID := messageIDByBody(t, st, "original body")
	var firstReplyID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT id FROM messages WHERE source_id = ? AND reply_to_message_id = ?`),
		source.ID, parentID).Scan(&firstReplyID))
	labelID, err := st.EnsureLabel(source.ID, "imazing-reply-label", "Replies", "user")
	require.NoError(err)
	_, err = st.ReconcileMessageLabels(firstReplyID, []int64{labelID}, false)
	require.NoError(err)

	// A later export inserts one more plain copy before the existing rows,
	// shifting the physical record numbers of every later row.
	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"),
		[][]string{parent, plain, plain, replying})
	second, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	require.Equal(1, second.RepliesLinked)

	var messageCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&messageCount))
	assert.Equal(4, messageCount, "exactly one new duplicate row is added")
	var replyTo sql.NullInt64
	var replyMetadata sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT reply_to_message_id, CAST(metadata AS TEXT) FROM messages WHERE id = ?`),
		firstReplyID).Scan(&replyTo, &replyMetadata))
	assert.True(replyTo.Valid, "the archived reply keeps its link")
	assert.Equal(parentID, replyTo.Int64, "the archived reply keeps its link")
	require.True(replyMetadata.Valid)
	assert.Contains(replyMetadata.String, "original body", "the archived reply keeps its evidence metadata")
	var labelCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_labels WHERE message_id = ? AND label_id = ?`),
		firstReplyID, labelID).Scan(&labelCount))
	assert.Equal(1, labelCount, "the archived reply keeps its label")
	var linkedCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages WHERE source_id = ? AND reply_to_message_id = ?`),
		source.ID, parentID).Scan(&linkedCount))
	assert.Equal(1, linkedCount, "no duplicate reply row is created")
}

func TestImporterContractedDuplicateGroupKeepsUnmatchedArchivedEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	// The four rows share every identity-bearing field, so they form one
	// duplicate occurrence group; only delivery and read evidence differ.
	delivered := []string{"Alice", "2024-06-01 12:00:00", "2024-06-01 12:00:05", "", "iMessage", "Outgoing", "", "", "Delivered", "", "", "ping", "", ""}
	read := []string{"Alice", "2024-06-01 12:00:00", "", "2024-06-01 12:00:10", "iMessage", "Outgoing", "", "", "Read", "", "", "ping", "", ""}
	plain := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "ping", "", ""}
	redelivered := []string{"Alice", "2024-06-01 12:00:00", "2024-06-01 12:00:07", "", "iMessage", "Outgoing", "", "", "Delivered", "", "", "ping", "", ""}
	exportDir := newTestExport(t, [][]string{delivered, read, plain})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	first, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	require.Equal(3, first.Messages)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	firstEvidence := archivedDuplicateEvidence(t, st, source.ID)
	require.Len(firstEvidence, 3)
	archivedDelivered := findArchivedEvidence(t, firstEvidence, "Delivered", "2024-06-01 12:00:05")
	archivedRead := findArchivedEvidence(t, firstEvidence, "Read", "")
	archivedPlain := findArchivedEvidence(t, firstEvidence, "", "")
	firstIDs := archivedSourceMessageIDs(firstEvidence)

	labelDelivered, err := st.EnsureLabel(source.ID, "dup-delivered", "Dup Delivered", "user")
	require.NoError(err)
	labelRead, err := st.EnsureLabel(source.ID, "dup-read", "Dup Read", "user")
	require.NoError(err)
	labelPlain, err := st.EnsureLabel(source.ID, "dup-plain", "Dup Plain", "user")
	require.NoError(err)
	for _, labeled := range []struct {
		messageID int64
		labelID   int64
	}{
		{archivedDelivered.messageID, labelDelivered},
		{archivedRead.messageID, labelRead},
		{archivedPlain.messageID, labelPlain},
	} {
		_, err := st.ReconcileMessageLabels(labeled.messageID, []int64{labeled.labelID}, false)
		require.NoError(err)
	}

	// A later export drops the read row and reorders the surviving rows, so
	// fewer rows survive than a previous export archived. Every surviving
	// row must rematch its archived occurrence by evidence, and the
	// occurrence with no surviving row must stay untouched instead of being
	// remapped onto a surviving row.
	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{plain, delivered})
	contracted, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	require.Equal(2, contracted.Messages)

	contractedEvidence := archivedDuplicateEvidence(t, st, source.ID)
	require.Len(contractedEvidence, 3, "the unmatched archived occurrence stays archived")
	assert.ElementsMatch(firstIDs, archivedSourceMessageIDs(contractedEvidence))
	contractedPlain := findArchivedEvidence(t, contractedEvidence, "", "")
	assert.Equal(archivedPlain.messageID, contractedPlain.messageID)
	assert.Equal(2, contractedPlain.record, "the surviving plain row rematches its archived occurrence")
	contractedDelivered := findArchivedEvidence(t, contractedEvidence, "Delivered", "2024-06-01 12:00:05")
	assert.Equal(archivedDelivered.messageID, contractedDelivered.messageID)
	assert.Equal(3, contractedDelivered.record, "the reordered delivered row rematches its archived occurrence")
	contractedRead := findArchivedEvidence(t, contractedEvidence, "Read", "")
	assert.Equal(archivedRead.messageID, contractedRead.messageID)
	assert.Equal(archivedRead.metadataRaw, contractedRead.metadataRaw,
		"the archived occurrence without a surviving row keeps its stored evidence")
	assert.Equal(1, messageLabelCount(t, st, archivedPlain.messageID, labelPlain))
	assert.Equal(1, messageLabelCount(t, st, archivedDelivered.messageID, labelDelivered))
	assert.Equal(1, messageLabelCount(t, st, archivedRead.messageID, labelRead))

	// Re-expanding the export rematches the previously unmatched occurrence
	// with its old evidence and allocates exactly one fresh occurrence for
	// the new evidence-distinct row.
	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"),
		[][]string{plain, delivered, read, redelivered})
	expanded, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	require.Equal(4, expanded.Messages)

	expandedEvidence := archivedDuplicateEvidence(t, st, source.ID)
	require.Len(expandedEvidence, 4)
	expandedIDs := archivedSourceMessageIDs(expandedEvidence)
	assert.Subset(expandedIDs, firstIDs)
	expandedRead := findArchivedEvidence(t, expandedEvidence, "Read", "")
	assert.Equal(archivedRead.messageID, expandedRead.messageID)
	assert.Equal(4, expandedRead.record, "the re-expanded read row rematches its archived occurrence")
	expandedRedelivered := findArchivedEvidence(t, expandedEvidence, "Delivered", "2024-06-01 12:00:07")
	assert.NotContains(firstIDs, expandedRedelivered.sourceMessageID,
		"the new duplicate row allocates one fresh occurrence")
	assert.Equal(5, expandedRedelivered.record)
	assert.Equal(1, messageLabelCount(t, st, expandedRead.messageID, labelRead))
}

func TestImporterWildcardDuplicateClaimsOnlyRemainingArchivedOccurrence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	delivered := []string{"Alice", "2024-06-01 12:00:00", "2024-06-01 12:00:05", "", "iMessage", "Outgoing", "", "", "Delivered", "", "", "ping", "", ""}
	read := []string{"Alice", "2024-06-01 12:00:00", "", "2024-06-01 12:00:10", "iMessage", "Outgoing", "", "", "Read", "", "", "ping", "", ""}
	plain := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "ping", "", ""}
	exportDir := newTestExport(t, [][]string{delivered, read})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	archived := archivedDuplicateEvidence(t, st, source.ID)
	readMessage := findArchivedEvidence(t, archived, "Read", "")
	labelID, err := st.EnsureLabel(source.ID, "wildcard-read", "Wildcard read", "user")
	require.NoError(err)
	_, err = st.ReconcileMessageLabels(readMessage.messageID, []int64{labelID}, false)
	require.NoError(err)

	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{delivered, plain})
	_, err = importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)

	var messageCount int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&messageCount))
	assert.Equal(2, messageCount, "the wildcard row reuses the sole remaining archived occurrence")
	assert.Equal(1, messageLabelCount(t, st, readMessage.messageID, labelID))
}

func TestImporterWildcardDuplicateDoesNotClaimAmbiguousArchivedOccurrence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	delivered := []string{"Alice", "2024-06-01 12:00:00", "2024-06-01 12:00:05", "", "iMessage", "Outgoing", "", "", "Delivered", "", "", "ping", "", ""}
	read := []string{"Alice", "2024-06-01 12:00:00", "", "2024-06-01 12:00:10", "iMessage", "Outgoing", "", "", "Read", "", "", "ping", "", ""}
	plain := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "ping", "", ""}
	exportDir := newTestExport(t, [][]string{delivered, read})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{plain})
	_, err = importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)

	evidence := archivedDuplicateEvidence(t, st, source.ID)
	assert.Len(evidence, 3, "an ambiguous wildcard receives a fresh occurrence")
	findArchivedEvidence(t, evidence, "Delivered", "2024-06-01 12:00:05")
	findArchivedEvidence(t, evidence, "Read", "")
	findArchivedEvidence(t, evidence, "", "")
}

func TestImporterWildcardDuplicatePreservesForcedMatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	delivered := []string{"Alice", "2024-06-01 12:00:00", "2024-06-01 12:00:05", "", "iMessage", "Outgoing", "", "", "Delivered", "", "", "ping", "", ""}
	read := []string{"Alice", "2024-06-01 12:00:00", "", "2024-06-01 12:00:10", "iMessage", "Outgoing", "", "", "Read", "", "", "ping", "", ""}
	exportDir := newTestExport(t, [][]string{delivered, read})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})
	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	before := messageRowIDs(t, st, source.ID)
	require.Len(before, 2)
	archived := archivedDuplicateEvidence(t, st, source.ID)
	deliveredID := findArchivedEvidence(t, archived, "Delivered", "2024-06-01 12:00:05").messageID
	readID := findArchivedEvidence(t, archived, "Read", "").messageID
	labelID, err := st.EnsureLabel(source.ID, "saved-message", "Saved message", "user")
	require.NoError(err)
	_, err = st.ReconcileMessageLabels(readID, []int64{labelID}, false)
	require.NoError(err)

	// Neither row is an exact match. The delivery time still identifies one
	// archived row, leaving the other occurrence for the wildcard row.
	delivered[8] = ""
	read[3], read[8] = "", ""
	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{delivered, read})
	_, err = importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Equal(before, messageRowIDs(t, st, source.ID), "receipt omission must reuse both archived messages")
	updated := archivedDuplicateEvidence(t, st, source.ID)
	assert.Equal(deliveredID, findArchivedEvidence(t, updated, "", "2024-06-01 12:00:05").messageID)
	wildcardID := findArchivedEvidence(t, updated, "", "").messageID
	assert.Equal(readID, wildcardID)
	assert.Equal(1, messageLabelCount(t, st, wildcardID, labelID))
}

func TestImporterCompetingDuplicateRows(t *testing.T) {
	for _, tt := range []struct {
		name          string
		identical     bool
		wildcardFirst bool
	}{
		{name: "identical rows", identical: true},
		{name: "wildcard first", identical: true, wildcardFirst: true},
		{name: "conflicting evidence"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			delivered := []string{"Alice", "2024-06-01 12:00:00", "2024-06-01 12:00:05", "", "iMessage", "Outgoing", "", "", "Delivered", "", "", "ping", "", ""}
			read := []string{"Alice", "2024-06-01 12:00:00", "", "2024-06-01 12:00:10", "iMessage", "Outgoing", "", "", "Read", "", "", "ping", "", ""}
			exportDir := newTestExport(t, [][]string{delivered, read})
			importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})
			_, err := importer.ImportPath(t.Context(), exportDir)
			require.NoError(err)
			source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
			require.NoError(err)
			archived := archivedDuplicateEvidence(t, st, source.ID)
			require.Len(archived, 2)
			labelID, err := st.EnsureLabel(source.ID, "saved-message", "Saved message", "user")
			require.NoError(err)
			_, err = st.ReconcileMessageLabels(archived[1].messageID, []int64{labelID}, false)
			require.NoError(err)

			// Neither competitor is exact; both can match only the delivered
			// occurrence. The wildcard can match either archived occurrence.
			delivered[8] = ""
			competitor := slices.Clone(delivered)
			if !tt.identical {
				competitor[2], competitor[8] = "", "Delivered"
			}
			read[3], read[8] = "", ""
			rows := [][]string{delivered, competitor, read}
			if tt.wildcardFirst {
				rows = [][]string{read, delivered, competitor}
			}
			writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), rows)
			_, err = importer.ImportPath(t.Context(), exportDir)
			require.NoError(err)
			updated := archivedDuplicateEvidence(t, st, source.ID)
			if tt.identical {
				require.Len(updated, 3, "reuse both archived messages and add only the surplus copy")
				assert.Equal(archived[0].messageID, updated[0].messageID)
				assert.Empty(updated[0].status, "the first archived message was updated")
				assert.Equal("2024-06-01 12:00:05", updated[0].deliveredDate)
				wildcard := findArchivedEvidence(t, updated, "", "")
				assert.Equal(archived[1].messageID, wildcard.messageID)
				assert.Equal(1, messageLabelCount(t, st, wildcard.messageID, labelID))
			} else {
				assert.Len(updated, 5, "different evidence must not arbitrarily claim the same archived message")
				assert.Equal(archived[0], updated[0])
				assert.Equal(archived[1], updated[1])
			}
			_, err = importer.ImportPath(t.Context(), exportDir)
			require.NoError(err)
			assert.Equal(archivedSourceMessageIDs(updated), sourceMessageIDs(t, st, source.ID),
				"rerunning the export must keep the same messages")
		})
	}
}

// One physical row re-exported with the same instant in iMazing's other
// accepted timestamp spelling must keep its archived message and source ID:
// the occurrence identity hashes the parsed instant, not the raw date text.
func TestImporterEquivalentDateSpellingKeepsOneMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	row := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "hello", "", ""}
	exportDir := newTestExport(t, [][]string{row})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	firstIDs := sourceMessageIDs(t, st, source.ID)
	require.Len(firstIDs, 1)
	firstRowIDs := messageRowIDs(t, st, source.ID)
	require.Len(firstRowIDs, 1)
	labelID, err := st.EnsureLabel(source.ID, "imazing-date-label", "Date spelling stable", "user")
	require.NoError(err)
	_, err = st.ReconcileMessageLabels(firstRowIDs[0], []int64{labelID}, false)
	require.NoError(err)

	// The RFC3339 spelling parses to the same UTC instant as the wall-clock
	// form under the UTC import timezone, and iMazing emits both spellings.
	row[1] = "2024-06-01T12:00:00Z"
	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{row})
	_, err = importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)

	assert.Equal(firstIDs, sourceMessageIDs(t, st, source.ID), "the re-spelled row keeps its source ID")
	assert.Equal(firstRowIDs, messageRowIDs(t, st, source.ID),
		"the re-spelled row updates the archived message in place")
	var messageCount int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&messageCount))
	assert.Equal(1, messageCount, "the re-spelled row must not add a duplicate")
	assert.Equal(1, messageLabelCount(t, st, firstRowIDs[0], labelID))
	latest, err := st.GetLatestSync(source.ID)
	require.NoError(err)
	assert.Equal(int64(0), latest.MessagesAdded, "the re-spelled row is an update, not an addition")
	assert.Equal(int64(1), latest.MessagesUpdated, "the re-spelled row is an update, not an addition")
}

// A unique occurrence whose mutable content changed between exports must
// update the archived message in place: the occurrence identity excludes
// subject, text, and attachment, so the edited row rematches its archived
// occurrence and keeps its source ID and user-applied label.
func TestImporterEditedContentUpdatesUniqueOccurrenceInPlace(t *testing.T) {
	row := func(subject, text, attachment string) []string {
		return []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", subject, text, attachment, ""}
	}
	tests := []struct {
		name   string
		first  []string
		edited []string
	}{
		{"subject edit", row("", "hello", ""), row("Edited subject", "hello", "")},
		{"text edit", row("", "hello", ""), row("", "hello edited", "")},
		{"attachment edit", row("", "photo", "old.bin"), row("", "photo", "nested/new.bin")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			exportDir := newTestExport(t, [][]string{tt.first})
			require.NoError(os.MkdirAll(filepath.Join(exportDir, "attachments", "nested"), 0o700))
			require.NoError(os.WriteFile(filepath.Join(exportDir, "attachments", "old.bin"), []byte("old bytes"), 0o600))
			require.NoError(os.WriteFile(filepath.Join(exportDir, "attachments", "nested", "new.bin"), []byte("new bytes"), 0o600))
			importer := NewImporter(st, Options{
				Owner: "+15550000001", Timezone: "UTC", AttachmentsDir: t.TempDir(),
			})

			_, err := importer.ImportPath(t.Context(), exportDir)
			require.NoError(err)
			source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
			require.NoError(err)
			firstIDs := sourceMessageIDs(t, st, source.ID)
			require.Len(firstIDs, 1)
			firstMessageID := messageIDByBody(t, st, tt.first[11])
			labelID, err := st.EnsureLabel(source.ID, "imazing-edit-label", "Edit stable", "user")
			require.NoError(err)
			_, err = st.ReconcileMessageLabels(firstMessageID, []int64{labelID}, false)
			require.NoError(err)

			writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{tt.edited})
			_, err = importer.ImportPath(t.Context(), exportDir)
			require.NoError(err)

			assert.Equal(firstIDs, sourceMessageIDs(t, st, source.ID), "the edited row keeps its source ID")
			assert.Equal(firstMessageID, messageIDByBody(t, st, tt.edited[11]),
				"the edited content updates the archived message in place")
			var messageCount int
			require.NoError(st.DB().QueryRow(st.Rebind(
				`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&messageCount))
			assert.Equal(1, messageCount, "the edited row must not add a duplicate")
			assert.Equal(1, messageLabelCount(t, st, firstMessageID, labelID),
				"the edited row keeps its user-applied label")
		})
	}
}

// Two rows that share chat, instant, direction, sender, and service form one
// duplicate group whose occurrences are told apart by content evidence. A
// later export that swaps their physical order must rematch each archived
// occurrence to its own row so both keep their message row, source ID, and
// user-applied label.
func TestImporterReorderedSameCoreRowsKeepTheirIDsByContentEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	firstRow := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "first body", "", ""}
	secondRow := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "second body", "", ""}
	exportDir := newTestExport(t, [][]string{firstRow, secondRow})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	firstMessageID := messageIDByBody(t, st, "first body")
	firstSourceID := sourceMessageIDOfMessage(t, st, firstMessageID)
	secondMessageID := messageIDByBody(t, st, "second body")
	secondSourceID := sourceMessageIDOfMessage(t, st, secondMessageID)
	require.NotEqual(firstSourceID, secondSourceID)
	labelID, err := st.EnsureLabel(source.ID, "imazing-order-label", "Order stable", "user")
	require.NoError(err)
	_, err = st.ReconcileMessageLabels(firstMessageID, []int64{labelID}, false)
	require.NoError(err)

	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{secondRow, firstRow})
	summary, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	require.Equal(2, summary.Messages)

	assert.Equal(firstMessageID, messageIDByBody(t, st, "first body"),
		"the reordered row keeps its archived message")
	assert.Equal(firstSourceID, sourceMessageIDOfMessage(t, st, firstMessageID),
		"the reordered row keeps its source ID")
	assert.Equal(secondMessageID, messageIDByBody(t, st, "second body"),
		"the reordered row keeps its archived message")
	assert.Equal(secondSourceID, sourceMessageIDOfMessage(t, st, secondMessageID),
		"the reordered row keeps its source ID")
	assert.Equal(1, messageLabelCount(t, st, firstMessageID, labelID))
}

func TestImporterReorderedDuplicatesKeepIDsWhenProviderEvidenceDisappears(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	first := []string{"Alice", "2024-06-01 12:00:00", "2024-06-01 12:00:05", "", "iMessage", "Outgoing", "", "", "Delivered", "", "", "first body", "", ""}
	second := []string{"Alice", "2024-06-01 12:00:00", "", "2024-06-01 12:00:10", "iMessage", "Outgoing", "", "", "Read", "", "", "second body", "", ""}
	exportDir := newTestExport(t, [][]string{first, second})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	firstMessageID := messageIDByBody(t, st, "first body")
	secondMessageID := messageIDByBody(t, st, "second body")
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	labelID, err := st.EnsureLabel(source.ID, "imazing-evidence-label", "Evidence stable", "user")
	require.NoError(err)
	_, err = st.ReconcileMessageLabels(firstMessageID, []int64{labelID}, false)
	require.NoError(err)

	for _, row := range [][]string{first, second} {
		row[2], row[3], row[8] = "", "", ""
	}
	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{second, first})
	_, err = importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)

	assert.Equal(firstMessageID, messageIDByBody(t, st, "first body"))
	assert.Equal(secondMessageID, messageIDByBody(t, st, "second body"))
	assert.Equal(1, messageLabelCount(t, st, firstMessageID, labelID))
	for _, messageID := range []int64{firstMessageID, secondMessageID} {
		var metadata string
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT CAST(metadata AS TEXT) FROM messages WHERE id = ?`), messageID).Scan(&metadata))
		assert.NotContains(metadata, `"status"`)
		assert.NotContains(metadata, `"delivered_date"`)
		assert.NotContains(metadata, `"read_date"`)
	}
}

func TestImporterEditedDuplicateDoesNotOverwriteEvidenceDistinctArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	first := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "first body", "", ""}
	second := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "second body", "", ""}
	exportDir := newTestExport(t, [][]string{first, second})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	firstMessageID := messageIDByBody(t, st, "first body")
	secondMessageID := messageIDByBody(t, st, "second body")
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	labelID, err := st.EnsureLabel(source.ID, "imazing-unmatched-label", "Unmatched stable", "user")
	require.NoError(err)
	_, err = st.ReconcileMessageLabels(firstMessageID, []int64{labelID}, false)
	require.NoError(err)

	edited := append([]string(nil), second...)
	edited[11] = "edited second body"
	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{edited})
	_, err = importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)

	assert.Equal(firstMessageID, messageIDByBody(t, st, "first body"))
	assert.Equal(secondMessageID, messageIDByBody(t, st, "second body"))
	assert.Equal(1, messageLabelCount(t, st, firstMessageID, labelID))
	editedMessageID := messageIDByBody(t, st, "edited second body")
	assert.NotEqual(firstMessageID, editedMessageID)
	assert.NotEqual(secondMessageID, editedMessageID)
}

func TestImporterFailedAttachmentRunStillRecomputesConversationStats(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "first message", "", ""},
		{"Alice", "2024-06-01 12:01:00", "", "", "iMessage", "Incoming", "+15550000002", "Alice", "", "", "", "second message", "huge.bin", ""},
	})
	require.NoError(os.Mkdir(filepath.Join(exportDir, "attachments"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(exportDir, "attachments", "huge.bin"), []byte("too large to store"), 0o600))

	// A storage write failure occurs after both messages have committed.
	attachmentStore := filepath.Join(t.TempDir(), "file")
	require.NoError(os.WriteFile(attachmentStore, []byte("not a directory"), 0o600))
	_, err := NewImporter(st, Options{
		Owner: "+15550000001", Timezone: "UTC", AttachmentsDir: attachmentStore,
	}).ImportPath(t.Context(), exportDir)
	require.Error(err)

	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	var messageCount, participantCount int
	var lastMessageAt sql.NullTime
	var lastMessagePreview sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT message_count, participant_count, last_message_at, last_message_preview
		FROM conversations WHERE source_id = ?`), source.ID).Scan(
		&messageCount, &participantCount, &lastMessageAt, &lastMessagePreview,
	))
	require.Equal(2, messageCount, "committed messages must be reflected in conversation stats")
	require.Equal(2, participantCount)
	require.True(lastMessageAt.Valid)
	require.Equal(time.Date(2024, 6, 1, 12, 1, 0, 0, time.UTC), lastMessageAt.Time.UTC())
	require.True(lastMessagePreview.Valid)
	require.Equal("second message", lastMessagePreview.String)
}

func TestImporterPartialPersistenceFailureRecomputesConversationStats(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	first := []string{"First", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "old preview", "", ""}
	second := []string{"Second", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "second preview", "", ""}
	exportDir := newTestExport(t, [][]string{first, second})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	secondID := messageIDByBody(t, st, "second preview")
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET metadata = ? WHERE id = ?`), "[]", secondID)
	require.NoError(err)

	first[11] = "new preview"
	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{first, second})
	_, err = importer.ImportPath(t.Context(), exportDir)
	require.ErrorContains(err, "decode existing metadata")

	var preview sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT last_message_preview FROM conversations
		WHERE source_id = ? AND source_conversation_id = ?`),
		source.ID, conversationKey("First")).Scan(&preview))
	require.True(preview.Valid)
	require.Equal("new preview", preview.String)
}

func TestImporterRecordsSyncRunCountersOnImportAndRerun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "hello", "", ""},
		{"Alice", "2024-06-01 12:01:00", "", "", "SMS", "Incoming", "+15550000002", "Alice", "", "", "", "reply", "", ""},
	})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)

	first, err := st.GetLatestSync(source.ID)
	require.NoError(err)
	require.Equal("completed", first.Status, "first import run status")
	assert.Equal(int64(2), first.MessagesProcessed, "first run processed counter")
	assert.Equal(int64(2), first.MessagesAdded, "first run added counter")
	assert.Equal(int64(0), first.MessagesUpdated, "first run updated counter")

	_, err = importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	second, err := st.GetLatestSync(source.ID)
	require.NoError(err)
	require.Equal("completed", second.Status, "rerun status")
	require.NotEqual(first.ID, second.ID, "rerun must record its own sync run")
	assert.Equal(int64(2), second.MessagesProcessed, "rerun processed counter")
	assert.Equal(int64(0), second.MessagesAdded, "rerun added counter")
	assert.Equal(int64(2), second.MessagesUpdated, "rerun updated counter")

	var messageCount int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&messageCount))
	assert.Equal(2, messageCount, "rerun must not duplicate messages")
}

func TestImporterRejectsTimezoneChangeAndRecordsFailedSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "SMS", "Incoming", "", "Alice", "", "", "", "hello", "", ""},
	})

	_, err := NewImporter(st, Options{Owner: "owner@example.test", Timezone: "UTC"}).
		ImportPath(context.Background(), exportDir)
	require.NoError(err)
	_, err = NewImporter(st, Options{Owner: "OWNER@EXAMPLE.TEST", Timezone: "Europe/Berlin"}).
		ImportPath(context.Background(), exportDir)
	require.ErrorContains(err, "timezone")

	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "owner@example.test")
	require.NoError(err)
	var completed, failed int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT
			SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END)
		FROM sync_runs WHERE source_id = ?`), source.ID).Scan(&completed, &failed))
	assert.Equal(1, completed)
	assert.Equal(1, failed)
}

// Go resolves exactly the spelling "Local" to the host's time.Local, so an
// explicit Local timezone would decode offset-free timestamps differently on
// every host while persisting the opaque name in the source sync config. The
// importer must reject it before creating a source or starting a sync, while
// concrete IANA zones and the stable UTC alias stay accepted.
func TestImporterRejectsHostDependentLocalTimezone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	assert.NoError(validateTimezone("UTC"))
	assert.NoError(validateTimezone("Europe/Berlin"))
	require.ErrorContains(validateTimezone("Local"), "host-dependent Local")

	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "hello", "", ""},
	})

	_, err := NewImporter(st, Options{Owner: "+15550000001", Timezone: "Local"}).
		ImportPath(context.Background(), exportDir)
	require.ErrorContains(err, "host-dependent Local")

	var sources, syncs, messages int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM sources WHERE source_type = ?`), SourceType).Scan(&sources))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM sync_runs`).Scan(&syncs))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Zero(sources, "a rejected timezone must not create a source")
	assert.Zero(syncs, "a rejected timezone must not start a sync")
	assert.Zero(messages, "a rejected timezone must not persist messages")
}

func TestImporterRejectsIncomingRowWithoutSenderEvidence(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "SMS", "Incoming", "", "", "", "", "", "hello", "", ""},
	})

	_, err := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"}).
		ImportPath(context.Background(), exportDir)
	require.ErrorContains(err, "incoming sender")
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	var messageCount int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&messageCount))
	require.Zero(messageCount)
}

func TestImporterStoresAttachmentsAndPreservesThenReplacesOccurrence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "photo", "nested/photo.bin", "image/jpeg"},
	})
	sourceAttachments := filepath.Join(exportDir, "attachments", "nested")
	require.NoError(os.MkdirAll(sourceAttachments, 0o700))
	sourcePath := filepath.Join(sourceAttachments, "photo.bin")
	require.NoError(os.WriteFile(sourcePath, []byte("first"), 0o600))
	attachmentStore := t.TempDir()
	importer := NewImporter(st, Options{
		Owner: "+15550000001", Timezone: "UTC", AttachmentsDir: attachmentStore,
	})

	first, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Equal(1, first.AttachmentsStored)
	assert.Zero(first.AttachmentsMissing)
	attachment := singleImportedAttachment(t, st)
	assert.Equal("photo.bin", attachment.filename)
	assert.Equal("image/jpeg", attachment.mimeType)
	assert.Equal("standalone", attachment.role)
	assert.Equal("importer_semantics", attachment.roleSource)
	assert.Equal("stored", attachment.state)
	require.NotEmpty(attachment.storagePath)
	storedBytes, err := os.ReadFile(filepath.Join(attachmentStore, attachment.storagePath))
	require.NoError(err)
	assert.Equal([]byte("first"), storedBytes)

	require.NoError(os.Remove(sourcePath))
	missing, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Zero(missing.AttachmentsStored)
	assert.Equal(1, missing.AttachmentsMissing)
	preserved := singleImportedAttachment(t, st)
	assert.Equal(attachment.id, preserved.id)
	assert.Equal(attachment.contentHash, preserved.contentHash)
	assert.Equal(attachment.storagePath, preserved.storagePath)
	assert.Equal("stored", preserved.state)

	require.NoError(os.WriteFile(sourcePath, []byte("changed"), 0o600))
	changed, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Equal(1, changed.AttachmentsStored)
	replaced := singleImportedAttachment(t, st)
	assert.Equal(attachment.id, replaced.id)
	assert.NotEqual(attachment.contentHash, replaced.contentHash)
	assert.NotEqual(attachment.storagePath, replaced.storagePath)
	storedBytes, err = os.ReadFile(filepath.Join(attachmentStore, replaced.storagePath))
	require.NoError(err)
	assert.Equal([]byte("changed"), storedBytes)
}

func TestImporterClassifiesAttachmentMediaTypes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "photo", "IMG_0001.jpg", "image/jpeg"},
		{"Alice", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "clip", "IMG_0002.mov", "video/quicktime"},
		{"Alice", "2024-06-01 12:02:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "voice note", "voice.m4a", "audio/mp4"},
		{"Alice", "2024-06-01 12:03:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "see contract", "docs/contract.pdf", "application/pdf"},
		{"Alice", "2024-06-01 12:04:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "mystery file", "payload.zzz", ""},
	})
	require.NoError(os.MkdirAll(filepath.Join(exportDir, "attachments", "docs"), 0o700))
	for _, name := range []string{
		"IMG_0001.jpg", "IMG_0002.mov", "voice.m4a", filepath.Join("docs", "contract.pdf"), "payload.zzz",
	} {
		require.NoError(os.WriteFile(filepath.Join(exportDir, "attachments", name), []byte("attachment bytes"), 0o600))
	}

	summary, err := NewImporter(st, Options{
		Owner: "+15550000001", Timezone: "UTC", AttachmentsDir: t.TempDir(),
	}).ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Equal(5, summary.AttachmentsStored)

	// Sort in Go rather than SQL: PostgreSQL and SQLite collate filenames
	// differently (contract.pdf before or after uppercase IMG names), so the
	// assertion order must not depend on the backend's collation.
	rows, err := st.DB().Query(`
		SELECT filename, mime_type, media_type FROM attachments`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var got []string
	for rows.Next() {
		var filename, mimeType, mediaType string
		require.NoError(rows.Scan(&filename, &mimeType, &mediaType))
		got = append(got, filename+"|"+mimeType+"|"+mediaType)
	}
	require.NoError(rows.Err())
	sort.Strings(got)
	assert.Equal([]string{
		"IMG_0001.jpg|image/jpeg|image",
		"IMG_0002.mov|video/quicktime|video",
		"contract.pdf|application/pdf|document",
		"payload.zzz|application/octet-stream|document",
		"voice.m4a|audio/mp4|audio",
	}, got)
}

func TestImporterRerunPreservesUserAppliedLabels(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "hello", "", ""},
	})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT id FROM messages WHERE source_id = ?`), source.ID).Scan(&messageID))
	labelID, err := st.EnsureLabel(source.ID, "imazing-user-label", "Keep on rerun", "user")
	require.NoError(err)
	_, err = st.ReconcileMessageLabels(messageID, []int64{labelID}, false)
	require.NoError(err)

	_, err = importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)

	var labelCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_labels WHERE message_id = ? AND label_id = ?`),
		messageID, labelID).Scan(&labelCount))
	assert.Equal(1, labelCount, "user-applied labels must survive a rerun that supplies no label IDs")
}

func TestImporterAttachmentFailureStillRecordsFailedOccurrence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "see contract", "docs/contract.pdf", "application/pdf"},
	})
	require.NoError(os.MkdirAll(filepath.Join(exportDir, "attachments", "docs"), 0o700))
	require.NoError(os.WriteFile(
		filepath.Join(exportDir, "attachments", "docs", "contract.pdf"),
		[]byte("far too large for the cap"), 0o600))

	// Storing the resolved file fails after the message already committed and
	// claims an attachment, so the failed occurrence must remain visible.
	attachmentStore := filepath.Join(t.TempDir(), "file")
	require.NoError(os.WriteFile(attachmentStore, []byte("not a directory"), 0o600))
	_, err := NewImporter(st, Options{
		Owner: "+15550000001", Timezone: "UTC", AttachmentsDir: attachmentStore,
	}).ImportPath(t.Context(), exportDir)
	require.Error(err)

	occurrence := singleImportedAttachment(t, st)
	assert.Equal("contract.pdf", occurrence.filename)
	assert.Equal("application/pdf", occurrence.mimeType)
	assert.Equal("failed", occurrence.state)
	assert.Empty(occurrence.storagePath)
	var skipReason string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COALESCE(attachment_skip_reason, '') FROM attachments WHERE id = ?`),
		occurrence.id).Scan(&skipReason))
	assert.Equal("fetch_failure", skipReason)
	var hasAttachments bool
	var attachmentCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT has_attachments, attachment_count FROM messages WHERE id = ?`),
		occurrence.messageID).Scan(&hasAttachments, &attachmentCount))
	assert.True(hasAttachments, "the message keeps its attachment claim")
	assert.Equal(1, attachmentCount)
}

func TestImporterAmbiguousAttachmentRemainsMissing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "see contract", "contract.pdf", "application/pdf"},
	})
	for _, dir := range []string{"one", "two"} {
		require.NoError(os.MkdirAll(filepath.Join(exportDir, "attachments", dir), 0o700))
		require.NoError(os.WriteFile(
			filepath.Join(exportDir, "attachments", dir, "contract.pdf"),
			[]byte("ambiguous basename"), 0o600))
	}

	// Ambiguity leaves a missing occurrence without blocking repeated imports.
	importer := NewImporter(st, Options{
		Owner: "+15550000001", Timezone: "UTC", AttachmentsDir: t.TempDir(),
	})
	for range 2 {
		summary, err := importer.ImportPath(t.Context(), exportDir)
		require.NoError(err)
		assert.Equal(1, summary.AttachmentsMissing)
	}

	occurrence := singleImportedAttachment(t, st)
	assert.Equal("contract.pdf", occurrence.filename)
	assert.Equal("application/pdf", occurrence.mimeType)
	assert.Equal("failed", occurrence.state)
	assert.Empty(occurrence.storagePath)
	var skipReason string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COALESCE(attachment_skip_reason, '') FROM attachments WHERE id = ?`),
		occurrence.id).Scan(&skipReason))
	assert.Equal("fetch_failure", skipReason)
}

func TestImporterSkipsAttachmentOverConfiguredLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000002", "", "", "", "", "large", "large.bin", ""},
	})
	require.NoError(os.Mkdir(filepath.Join(exportDir, "attachments"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(exportDir, "attachments", "large.bin"), []byte("too large"), 0o600))
	contacts := filepath.Join(t.TempDir(), "contacts.vcf")
	require.NoError(os.WriteFile(contacts, []byte("BEGIN:VCARD\nVERSION:3.0\nFN:Example Contact\nTEL:+15550000002\nEND:VCARD\n"), 0o600))

	importer := NewImporter(st, Options{
		Owner: "+15550000001", Timezone: "UTC", AttachmentsDir: t.TempDir(), MaxAttachmentBytes: 2,
		ContactsPath: contacts,
	})
	for range 2 {
		summary, err := importer.ImportPath(t.Context(), exportDir)
		require.NoError(err)
		assert.Equal(1, summary.ContactsTotal, "contact enrichment runs after a skipped attachment")
		assert.Equal(1, summary.AttachmentsSkipped)
	}
	occurrence := singleImportedAttachment(t, st)
	assert.Equal("skipped", occurrence.state)
	assert.Empty(occurrence.storagePath)
	var skipReason, displayName, syncStatus string
	require.NoError(st.DB().QueryRow(`SELECT attachment_skip_reason FROM attachments`).Scan(&skipReason))
	assert.Equal("size_cap", skipReason)
	require.NoError(st.DB().QueryRow(`SELECT display_name FROM participants WHERE phone_number = '+15550000002'`).Scan(&displayName))
	assert.Equal("Example Contact", displayName)
	require.NoError(st.DB().QueryRow(`SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&syncStatus))
	assert.Equal("completed", syncStatus)
}

func TestImporterDeduplicatesSharedAttachmentBytes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "first", "first.bin", ""},
		{"Alice", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "second", "second.bin", ""},
	})
	require.NoError(os.Mkdir(filepath.Join(exportDir, "attachments"), 0o700))
	for _, name := range []string{"first.bin", "second.bin"} {
		require.NoError(os.WriteFile(filepath.Join(exportDir, "attachments", name), []byte("shared"), 0o600))
	}
	summary, err := NewImporter(st, Options{
		Owner: "+15550000001", Timezone: "UTC", AttachmentsDir: t.TempDir(),
	}).ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Equal(2, summary.AttachmentsStored)
	var occurrences, blobs int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*), COUNT(DISTINCT storage_path) FROM attachments
	`).Scan(&occurrences, &blobs))
	assert.Equal(2, occurrences)
	assert.Equal(1, blobs)
}

// Attachment rows are independent: a failure on one row must not stop later
// messages from receiving their occurrences, because every message was
// already persisted with its attachment claim. The run records a typed
// failed occurrence for each affected message, still stores later valid
// attachments, and reports every failure in one aggregate error.
func TestImporterAttachmentFailuresContinueToLaterRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "escape", "../secret.bin", ""},
		{"Alice", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "huge", "huge.bin", ""},
		{"Alice", "2024-06-01 12:02:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "late", "late.bin", ""},
	})
	require.NoError(os.Mkdir(filepath.Join(exportDir, "attachments"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(exportDir, "attachments", "huge.bin"), []byte("way too large"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(exportDir, "attachments", "late.bin"), []byte("late"), 0o600))

	_, err := NewImporter(st, Options{
		Owner: "+15550000001", Timezone: "UTC", AttachmentsDir: t.TempDir(), MaxAttachmentBytes: 4,
	}).ImportPath(t.Context(), exportDir)
	require.Error(err)
	assert.Contains(err.Error(), "messages.csv record 2: unsafe iMazing attachment reference")
	assert.NotContains(err.Error(), "record 3", "the oversized attachment is skipped")
	assert.NotContains(err.Error(), "record 4", "the valid later row must not fail")

	var messageCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount))
	assert.Equal(3, messageCount, "every message stays committed")

	rows, err := st.DB().Query(`
		SELECT filename, COALESCE(attachment_state, ''), COALESCE(storage_path, '')
		FROM attachments`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	occurrences := make(map[string][2]string)
	for rows.Next() {
		var filename string
		var stateAndPath [2]string
		require.NoError(rows.Scan(&filename, &stateAndPath[0], &stateAndPath[1]))
		occurrences[filename] = stateAndPath
	}
	require.NoError(rows.Err())
	require.Len(occurrences, 3, "every referenced attachment has an occurrence")
	assert.Equal([2]string{"failed", ""}, occurrences["secret.bin"], "the unsafe reference records a failed occurrence")
	assert.Equal([2]string{"skipped", ""}, occurrences["huge.bin"], "the oversized reference records a skipped occurrence")
	require.Equal("stored", occurrences["late.bin"][0], "the valid later attachment is stored")
	assert.NotEmpty(occurrences["late.bin"][1], "the valid later attachment has archived bytes")
}

// A lower size cap must not discard bytes stored by a previous run.
func TestImporterSkippedAttachmentPreservesStoredOccurrences(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "early", "early.bin", ""},
		{"Alice", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "tiny", "tiny.txt", ""},
		{"Alice", "2024-06-01 12:02:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "late", "late.bin", ""},
	})
	require.NoError(os.Mkdir(filepath.Join(exportDir, "attachments"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(exportDir, "attachments", "early.bin"), []byte("early-bytes"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(exportDir, "attachments", "tiny.txt"), []byte("x"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(exportDir, "attachments", "late.bin"), []byte("late-bytes"), 0o600))
	attachmentStore := t.TempDir()
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC", AttachmentsDir: attachmentStore})

	first, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	require.Equal(3, first.AttachmentsStored)
	attachmentByID := make(map[int64]importedAttachment)
	for _, attachment := range importedAttachments(t, st) {
		attachmentByID[attachment.id] = attachment
	}
	require.Len(attachmentByID, 3)

	// The lowered cap skips the early and late rows while the tiny row stores.
	second, err := NewImporter(st, Options{
		Owner: "+15550000001", Timezone: "UTC", AttachmentsDir: attachmentStore, MaxAttachmentBytes: 4,
	}).ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Equal(1, second.AttachmentsStored)
	assert.Equal(2, second.AttachmentsSkipped)

	after := importedAttachments(t, st)
	require.Len(after, 3)
	for _, attachment := range after {
		previous := attachmentByID[attachment.id]
		require.Equal(previous.id, attachment.id)
		switch attachment.filename {
		case "early.bin", "late.bin":
			assert.Equal("stored", attachment.state, "%s keeps its stored occurrence", attachment.filename)
			assert.Equal(previous.contentHash, attachment.contentHash)
			assert.Equal(previous.storagePath, attachment.storagePath)
			storedBytes, err := os.ReadFile(filepath.Join(attachmentStore, attachment.storagePath))
			require.NoError(err, "%s archived bytes stay available", attachment.filename)
			if attachment.filename == "early.bin" {
				assert.Equal([]byte("early-bytes"), storedBytes)
			} else {
				assert.Equal([]byte("late-bytes"), storedBytes)
			}
		case "tiny.txt":
			assert.Equal("stored", attachment.state)
			assert.Equal(previous.contentHash, attachment.contentHash)
		}
	}
}

func TestImportAttachmentsContinuesIndependentRowsAndCounters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	messageIDs := f.CreateMessages(4)
	attachmentsDir := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(attachmentsDir, "huge.bin"), []byte("huge!"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(attachmentsDir, "ok.bin"), []byte("ok"), 0o600))
	plans := []*plannedMessage{
		{row: Row{File: "messages.csv", Record: 2, Attachment: "../escape.bin"}, messageID: messageIDs[0]},
		{row: Row{File: "messages.csv", Record: 3, Attachment: "absent.bin"}, messageID: messageIDs[1]},
		{row: Row{File: "messages.csv", Record: 4, Attachment: "huge.bin"}, messageID: messageIDs[2]},
		{row: Row{File: "messages.csv", Record: 5, Attachment: "ok.bin"}, messageID: messageIDs[3]},
	}

	stored, missing, skipped, err := importAttachments(t.Context(), f.Store, Layout{AttachmentsDir: attachmentsDir},
		Options{AttachmentsDir: t.TempDir(), MaxAttachmentBytes: 2}, plans)

	require.Error(err)
	assert.Contains(err.Error(), "messages.csv record 2: unsafe iMazing attachment reference")
	assert.NotContains(err.Error(), "record 4", "the oversized attachment is skipped")
	assert.NotContains(err.Error(), "record 3", "a missing reference is not an error")
	assert.NotContains(err.Error(), "record 5", "the valid row must not fail")
	assert.Equal(1, stored, "only the valid row counts as stored")
	assert.Equal(1, missing, "only the missing row counts as missing")
	assert.Equal(1, skipped, "only the oversized row counts as skipped")

	var occurrences int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&occurrences))
	assert.Equal(4, occurrences, "every row leaves an occurrence")
	var storedOnes int
	require.NoError(f.Store.DB().QueryRow(`
		SELECT COUNT(*) FROM attachments WHERE COALESCE(attachment_state, '') = 'stored'`).Scan(&storedOnes))
	assert.Equal(1, storedOnes)
}

func TestImportAttachmentsStopsPromptlyOnCanceledContext(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("canceled-attachment")
	attachmentsDir := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(attachmentsDir, "ok.bin"), []byte("ok"), 0o600))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stored, missing, skipped, err := importAttachments(ctx, f.Store, Layout{AttachmentsDir: attachmentsDir},
		Options{AttachmentsDir: t.TempDir()},
		[]*plannedMessage{{row: Row{File: "messages.csv", Record: 2, Attachment: "ok.bin"}, messageID: messageID}})

	require.ErrorIs(err, context.Canceled)
	require.Zero(stored)
	require.Zero(missing)
	require.Zero(skipped)
	var occurrences int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&occurrences))
	require.Zero(occurrences, "a canceled run must not store or record attachments")
}

func TestImporterFillsInitiallyMissingAttachment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "photo", "photo.bin", ""},
	})
	attachmentStore := t.TempDir()
	importer := NewImporter(st, Options{
		Owner: "+15550000001", Timezone: "UTC", AttachmentsDir: attachmentStore,
	})
	missing, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Equal(1, missing.AttachmentsMissing)
	placeholder := singleImportedAttachment(t, st)
	assert.Empty(placeholder.storagePath)
	assert.Equal("failed", placeholder.state)

	require.NoError(os.Mkdir(filepath.Join(exportDir, "attachments"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(exportDir, "attachments", "photo.bin"), []byte("found"), 0o600))
	found, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Equal(1, found.AttachmentsStored)
	stored := singleImportedAttachment(t, st)
	assert.Equal(placeholder.id, stored.id)
	assert.NotEmpty(stored.storagePath)
	assert.Equal("stored", stored.state)
}

func TestImporterReconcilesEditedAttachmentReference(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	row := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "photo", "old.bin", ""}
	exportDir := newTestExport(t, [][]string{row})
	require.NoError(os.Mkdir(filepath.Join(exportDir, "attachments"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(exportDir, "attachments", "old.bin"), []byte("old"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(exportDir, "attachments", "new.bin"), []byte("new"), 0o600))
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC", AttachmentsDir: t.TempDir()})

	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	old := singleImportedAttachment(t, st)
	require.Equal("old.bin", old.filename)

	row[12] = "new.bin"
	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{row})
	_, err = importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	attachments := importedAttachments(t, st)
	require.Len(attachments, 1)
	assert.Equal("new.bin", attachments[0].filename)
	var hasAttachments bool
	var attachmentCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT has_attachments, attachment_count FROM messages WHERE id = ?`),
		attachments[0].messageID).Scan(&hasAttachments, &attachmentCount))
	assert.True(hasAttachments)
	assert.Equal(1, attachmentCount)
}

func TestImporterEditedMissingAttachmentDoesNotPreserveOldReferenceBytes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	row := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "photo", "old.bin", ""}
	exportDir := newTestExport(t, [][]string{row})
	require.NoError(os.Mkdir(filepath.Join(exportDir, "attachments"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(exportDir, "attachments", "old.bin"), []byte("old"), 0o600))
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC", AttachmentsDir: t.TempDir()})

	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	require.NotEmpty(singleImportedAttachment(t, st).storagePath)

	row[12] = "missing.bin"
	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{row})
	summary, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Equal(1, summary.AttachmentsMissing)
	attachment := singleImportedAttachment(t, st)
	assert.Equal("missing.bin", attachment.filename)
	assert.Empty(attachment.storagePath)
	assert.Empty(attachment.contentHash)
	assert.Equal("failed", attachment.state)
}

func TestImporterReconcilesClearedAttachmentReference(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	row := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "photo", "old.bin", ""}
	exportDir := newTestExport(t, [][]string{row})
	require.NoError(os.Mkdir(filepath.Join(exportDir, "attachments"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(exportDir, "attachments", "old.bin"), []byte("old"), 0o600))
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC", AttachmentsDir: t.TempDir()})

	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	messageID := singleImportedAttachment(t, st).messageID

	row[12] = ""
	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{row})
	_, err = importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Empty(importedAttachments(t, st))
	var hasAttachments bool
	var attachmentCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT has_attachments, attachment_count FROM messages WHERE id = ?`),
		messageID).Scan(&hasAttachments, &attachmentCount))
	assert.False(hasAttachments)
	assert.Zero(attachmentCount)
}

func TestImporterContactsFillOnlyEmptyNames(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Phone Friend & CSV Friend", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000002", "", "", "", "", "phone", "", ""},
		{"Phone Friend & CSV Friend", "2024-06-01 12:01:00", "", "", "iMessage", "Incoming", "friend@example.test", "CSV Friend", "", "", "", "email", "", ""},
	})
	contacts := filepath.Join(t.TempDir(), "contacts.vcf")
	require.NoError(os.WriteFile(contacts, []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Contact Phone\r\nTEL:+15550000002\r\nEND:VCARD\r\nBEGIN:VCARD\r\nVERSION:3.0\r\nFN:Contact Email\r\nEMAIL:friend@example.test\r\nEND:VCARD\r\n"), 0o600))

	summary, err := NewImporter(st, Options{
		Owner: "+15550000001", Timezone: "UTC", ContactsPath: contacts,
	}).ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Equal(1, summary.ContactsMatched)
	assert.Equal(2, summary.ContactsTotal)

	rows, err := st.DB().Query(`
		SELECT COALESCE(phone_number, ''), COALESCE(email_address, ''), COALESCE(display_name, '')
		FROM participants WHERE phone_number = '+15550000002' OR email_address = 'friend@example.test'
		ORDER BY COALESCE(phone_number, '') DESC
	`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var names []string
	for rows.Next() {
		var phone, email, name string
		require.NoError(rows.Scan(&phone, &email, &name))
		names = append(names, name)
	}
	require.NoError(rows.Err())
	assert.Equal([]string{"Contact Phone", "CSV Friend"}, names)

	var title string
	require.NoError(st.DB().QueryRow(`SELECT title FROM conversations`).Scan(&title))
	assert.Equal("Phone Friend & CSV Friend", title)
}

func TestImporterCountsMatchedContactsOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Shared Contact", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000002", "", "", "", "", "phone", "", ""},
		{"Shared Contact", "2024-06-01 12:01:00", "", "", "iMessage", "Incoming", "friend@example.test", "", "", "", "", "email", "", ""},
	})
	contacts := filepath.Join(t.TempDir(), "contacts.vcf")
	require.NoError(os.WriteFile(contacts, []byte(
		"BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Shared Contact\r\nTEL:+15550000002\r\nEMAIL:friend@example.test\r\nEND:VCARD\r\n"+
			"BEGIN:VCARD\r\nVERSION:3.0\r\nFN:\r\nTEL:+15550000003\r\nEND:VCARD\r\n",
	), 0o600))

	summary, err := NewImporter(st, Options{
		Owner: "+15550000001", Timezone: "UTC", ContactsPath: contacts,
	}).ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Equal(1, summary.ContactsMatched)
	assert.Equal(1, summary.ContactsTotal)
}

type importedAttachment struct {
	id          int64
	messageID   int64
	size        int64
	filename    string
	mimeType    string
	storagePath string
	contentHash string
	role        string
	roleSource  string
	state       string
}

func importedAttachments(t *testing.T, st *store.Store) []importedAttachment {
	t.Helper()
	rows, err := st.DB().Query(`
		SELECT id, message_id, COALESCE(size, 0), filename, mime_type, storage_path, content_hash,
		       attachment_role, role_source, COALESCE(attachment_state, '')
		FROM attachments`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var result []importedAttachment
	for rows.Next() {
		var attachment importedAttachment
		require.NoError(t, rows.Scan(
			&attachment.id, &attachment.messageID, &attachment.size, &attachment.filename,
			&attachment.mimeType, &attachment.storagePath, &attachment.contentHash,
			&attachment.role, &attachment.roleSource, &attachment.state,
		))
		result = append(result, attachment)
	}
	require.NoError(t, rows.Err())
	return result
}

func singleImportedAttachment(t *testing.T, st *store.Store) importedAttachment {
	t.Helper()
	var attachment importedAttachment
	require.NoError(t, st.DB().QueryRow(`
		SELECT id, message_id, COALESCE(size, 0), filename, mime_type, storage_path, content_hash,
		       attachment_role, role_source, COALESCE(attachment_state, '')
		FROM attachments
	`).Scan(
		&attachment.id, &attachment.messageID, &attachment.size, &attachment.filename,
		&attachment.mimeType, &attachment.storagePath, &attachment.contentHash,
		&attachment.role, &attachment.roleSource, &attachment.state,
	))
	return attachment
}

func newTestExport(t *testing.T, rows [][]string) string {
	t.Helper()
	root := t.TempDir()
	csvDir := filepath.Join(root, "csv")
	require.NoError(t, os.Mkdir(csvDir, 0o700))
	writeTestCSV(t, filepath.Join(csvDir, "messages.csv"), rows)
	return root
}

func writeTestCSV(t *testing.T, path string, rows [][]string) {
	t.Helper()
	file, err := os.Create(path)
	require.NoError(t, err)
	writer := csv.NewWriter(file)
	require.NoError(t, writer.Write([]string{
		"Chat Session", "Message Date", "Delivered Date", "Read Date", "Service", "Type",
		"Sender ID", "Sender Name", "Status", "Replying to", "Subject", "Text", "Attachment", "Attachment type",
	}))
	for _, row := range rows {
		require.NoError(t, writer.Write(row))
	}
	writer.Flush()
	require.NoError(t, writer.Error())
	require.NoError(t, file.Close())
}

func sourceMessageIDs(t *testing.T, st *store.Store, sourceID int64) []string {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`
		SELECT source_message_id FROM messages WHERE source_id = ? ORDER BY sent_at, id`), sourceID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var result []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		result = append(result, id)
	}
	require.NoError(t, rows.Err())
	return result
}

func messageRowIDs(t *testing.T, st *store.Store, sourceID int64) []int64 {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(
		`SELECT id FROM messages WHERE source_id = ? ORDER BY sent_at, id`), sourceID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var result []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		result = append(result, id)
	}
	require.NoError(t, rows.Err())
	return result
}

func sourceMessageIDOfMessage(t *testing.T, st *store.Store, messageID int64) string {
	t.Helper()
	var id string
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT source_message_id FROM messages WHERE id = ?`), messageID).Scan(&id))
	return id
}

func singleConversationID(t *testing.T, st *store.Store, sourceID int64) int64 {
	t.Helper()
	var id int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT id FROM conversations WHERE source_id = ?`), sourceID).Scan(&id))
	return id
}

// archivedDuplicateEvidence loads every archived message of one source with
// the evidence fields the duplicate occurrence reconciliation matches on.
type archivedEvidence struct {
	sourceMessageID string
	messageID       int64
	status          string
	deliveredDate   string
	record          int
	metadataRaw     string
}

func archivedDuplicateEvidence(t *testing.T, st *store.Store, sourceID int64) []archivedEvidence {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`
		SELECT source_message_id, id, CAST(metadata AS TEXT)
		FROM messages WHERE source_id = ? ORDER BY id`), sourceID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var result []archivedEvidence
	for rows.Next() {
		var evidence archivedEvidence
		var metadata sql.NullString
		require.NoError(t, rows.Scan(&evidence.sourceMessageID, &evidence.messageID, &metadata))
		require.True(t, metadata.Valid)
		evidence.metadataRaw = metadata.String
		var decoded struct {
			Status    string `json:"status"`
			Delivered string `json:"delivered_date"`
			Record    int    `json:"record"`
		}
		require.NoError(t, json.Unmarshal([]byte(metadata.String), &decoded))
		evidence.status = decoded.Status
		evidence.deliveredDate = decoded.Delivered
		evidence.record = decoded.Record
		result = append(result, evidence)
	}
	require.NoError(t, rows.Err())
	return result
}

func findArchivedEvidence(t *testing.T, evidence []archivedEvidence, status, deliveredDate string) archivedEvidence {
	t.Helper()
	var match archivedEvidence
	matches := 0
	for _, candidate := range evidence {
		if candidate.status != status || candidate.deliveredDate != deliveredDate {
			continue
		}
		match, matches = candidate, matches+1
	}
	require.Equal(t, 1, matches,
		"expected exactly one archived duplicate with status %q and delivered date %q", status, deliveredDate)
	return match
}

func archivedSourceMessageIDs(evidence []archivedEvidence) []string {
	ids := make([]string, 0, len(evidence))
	for _, candidate := range evidence {
		ids = append(ids, candidate.sourceMessageID)
	}
	return ids
}

func messageLabelCount(t *testing.T, st *store.Store, messageID, labelID int64) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_labels WHERE message_id = ? AND label_id = ?`),
		messageID, labelID).Scan(&count))
	return count
}
