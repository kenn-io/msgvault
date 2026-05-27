package textimport_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textimport"
)

func TestNormalizeAddressClassifiesPhoneAndRaw(t *testing.T) {
	require := requirepkg.New(t)
	phone := textimport.NormalizeAddress("+1 (555) 123-4567")
	require.Equal(textimport.AddressPhone, phone.Kind, "phone normalization = %#v", phone)
	require.Equal("+15551234567", phone.Value, "phone normalization = %#v", phone)
	short := textimport.NormalizeAddress("12345")
	require.Equal(textimport.AddressRaw, short.Kind, "short code normalization = %#v", short)
	require.Equal("12345", short.Value, "short code normalization = %#v", short)
}

// TestIntegration exercises the full text message import pipeline:
// store methods, participant deduplication across sources,
// conversation stats recomputation, and TextEngine queries.
func TestIntegration(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()

	// Create a temporary on-disk DB (store.Open does MkdirAll, WAL, etc.)
	dbPath := filepath.Join(t.TempDir(), "integration.db")
	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = s.Close() })

	require.NoError(s.InitSchema(), "init schema")

	// --- Sources ---
	src1, err := s.GetOrCreateSource("whatsapp", "whatsapp:+15550000001")
	require.NoError(err, "GetOrCreateSource(whatsapp)")
	src2, err := s.GetOrCreateSource("apple_messages", "apple_messages:+15550000001")
	require.NoError(err, "GetOrCreateSource(apple_messages)")

	// --- Participant deduplication across sources ---
	// Both sources reference the same phone +15551234567.
	// EnsureParticipantByPhone deduplicates by phone, so both calls should
	// return the same participant ID.
	participantID1, err := s.EnsureParticipantByPhone("+15551234567", "Alice", "whatsapp")
	require.NoError(err, "EnsureParticipantByPhone(src1)")
	participantID2, err := s.EnsureParticipantByPhone("+15551234567", "Alice", "imessage")
	require.NoError(err, "EnsureParticipantByPhone(src2)")
	assert.Equal(participantID1, participantID2, "same phone across sources: participant IDs differ")
	phoneParticipantID := participantID1

	// --- Conversations ---
	conv1ID, err := s.EnsureConversationWithType(src1.ID, "wa-conv-1", "whatsapp", "WhatsApp Chat")
	require.NoError(err, "EnsureConversationWithType(src1)")
	conv2ID, err := s.EnsureConversationWithType(src2.ID, "am-conv-1", "imessage", "iMessage Chat")
	require.NoError(err, "EnsureConversationWithType(src2)")

	// Link participant to both conversations.
	require.NoError(s.EnsureConversationParticipant(conv1ID, phoneParticipantID, "member"), "EnsureConversationParticipant(conv1)")
	require.NoError(s.EnsureConversationParticipant(conv2ID, phoneParticipantID, "member"), "EnsureConversationParticipant(conv2)")

	// --- Messages for source 1 (whatsapp) ---
	baseTime := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	whatsappMsgs := []struct {
		srcMsgID string
		snippet  string
		sentAt   time.Time
		fromMe   bool
	}{
		{"wa-1", "Hello from WhatsApp", baseTime, false},
		{"wa-2", "Reply on WhatsApp", baseTime.Add(time.Minute), true},
		{"wa-3", "Third WhatsApp message", baseTime.Add(2 * time.Minute), false},
	}
	for _, m := range whatsappMsgs {
		msg := &store.Message{
			SourceID:        src1.ID,
			SourceMessageID: m.srcMsgID,
			ConversationID:  conv1ID,
			MessageType:     "whatsapp",
			Snippet:         sql.NullString{String: m.snippet, Valid: true},
			SentAt:          sql.NullTime{Time: m.sentAt, Valid: true},
			IsFromMe:        m.fromMe,
			SizeEstimate:    int64(len(m.snippet)),
			SenderID:        sql.NullInt64{Int64: phoneParticipantID, Valid: true},
		}
		msgID, err := s.UpsertMessage(msg)
		require.NoError(err, "UpsertMessage(%s)", m.srcMsgID)
		bodyText := sql.NullString{String: m.snippet, Valid: true}
		require.NoError(s.UpsertMessageBody(msgID, bodyText, sql.NullString{}), "UpsertMessageBody(%s)", m.srcMsgID)
		// Add participant as message recipient for TextAggregate to pick up
		require.NoError(s.ReplaceMessageRecipients(
			msgID, "from",
			[]int64{phoneParticipantID}, []string{"Alice"},
		), "ReplaceMessageRecipients(%s)", m.srcMsgID)
	}

	// --- Messages for source 2 (apple_messages) ---
	imessageMsgs := []struct {
		srcMsgID string
		snippet  string
		sentAt   time.Time
	}{
		{"am-1", "Hi from iMessage", baseTime.Add(time.Hour)},
		{"am-2", "iMessage follow-up", baseTime.Add(time.Hour + time.Minute)},
	}
	for _, m := range imessageMsgs {
		msg := &store.Message{
			SourceID:        src2.ID,
			SourceMessageID: m.srcMsgID,
			ConversationID:  conv2ID,
			MessageType:     "imessage",
			Snippet:         sql.NullString{String: m.snippet, Valid: true},
			SentAt:          sql.NullTime{Time: m.sentAt, Valid: true},
			SizeEstimate:    int64(len(m.snippet)),
			SenderID:        sql.NullInt64{Int64: phoneParticipantID, Valid: true},
		}
		msgID, err := s.UpsertMessage(msg)
		require.NoError(err, "UpsertMessage(%s)", m.srcMsgID)
		bodyText := sql.NullString{String: m.snippet, Valid: true}
		require.NoError(s.UpsertMessageBody(msgID, bodyText, sql.NullString{}), "UpsertMessageBody(%s)", m.srcMsgID)
		require.NoError(s.ReplaceMessageRecipients(
			msgID, "from",
			[]int64{phoneParticipantID}, []string{"Alice"},
		), "ReplaceMessageRecipients(%s)", m.srcMsgID)
	}

	// --- Same-timestamp message for preview tie-breaker test ---
	// Inserted after wa-3 with the SAME sent_at; should have higher ID.
	// ListConversations should pick this as last_preview (highest ID wins).
	{
		sameTimestamp := baseTime.Add(2 * time.Minute) // same as wa-3
		msg := &store.Message{
			SourceID:        src1.ID,
			SourceMessageID: "wa-4-tiebreaker",
			ConversationID:  conv1ID,
			MessageType:     "whatsapp",
			Snippet:         sql.NullString{String: "tiebreaker preview", Valid: true},
			SentAt:          sql.NullTime{Time: sameTimestamp, Valid: true},
			SizeEstimate:    18,
			SenderID:        sql.NullInt64{Int64: phoneParticipantID, Valid: true},
		}
		msgID, err := s.UpsertMessage(msg)
		require.NoError(err, "UpsertMessage(wa-4-tiebreaker)")
		require.NoError(s.UpsertMessageBody(msgID,
			sql.NullString{String: "tiebreaker preview", Valid: true},
			sql.NullString{}), "UpsertMessageBody(wa-4-tiebreaker)")
		require.NoError(s.ReplaceMessageRecipients(msgID, "from",
			[]int64{phoneParticipantID}, []string{"Alice"}), "ReplaceMessageRecipients(wa-4-tiebreaker)")
	}

	// --- Message with NULL sender_id (backward-compatibility) ---
	// Some older imports only have message_recipients "from" rows, not sender_id.
	// Verify that TextAggregate still picks these up via the COALESCE fallback.
	{
		msg := &store.Message{
			SourceID:        src2.ID,
			SourceMessageID: "am-null-sender",
			ConversationID:  conv2ID,
			MessageType:     "imessage",
			Snippet:         sql.NullString{String: "Null sender msg", Valid: true},
			SentAt:          sql.NullTime{Time: baseTime.Add(2 * time.Hour), Valid: true},
			SizeEstimate:    15,
			SenderID:        sql.NullInt64{}, // NULL
		}
		msgID, err := s.UpsertMessage(msg)
		require.NoError(err, "UpsertMessage(am-null-sender)")
		bodyText := sql.NullString{String: "Null sender msg", Valid: true}
		require.NoError(s.UpsertMessageBody(msgID, bodyText, sql.NullString{}), "UpsertMessageBody(am-null-sender)")
		require.NoError(s.ReplaceMessageRecipients(
			msgID, "from",
			[]int64{phoneParticipantID}, []string{"Alice"},
		), "ReplaceMessageRecipients(am-null-sender)")
	}

	// --- Labels ---
	labelID, err := s.EnsureLabel(src1.ID, "important", "Important", "user")
	require.NoError(err, "EnsureLabel")
	// Fetch the first WhatsApp message ID to link a label.
	var wa1MsgID int64
	require.NoError(s.DB().QueryRow(
		`SELECT id FROM messages WHERE source_message_id = ?`, "wa-1",
	).Scan(&wa1MsgID), "lookup wa-1 message")
	require.NoError(s.LinkMessageLabel(wa1MsgID, labelID), "LinkMessageLabel")

	// Verify label is linked.
	var labelCount int
	require.NoError(s.DB().QueryRow(
		`SELECT COUNT(*) FROM message_labels WHERE message_id = ?`, wa1MsgID,
	).Scan(&labelCount), "count labels")
	assert.Equal(1, labelCount, "label count for wa-1")

	// --- Recompute conversation stats ---
	require.NoError(s.RecomputeConversationStats(src1.ID), "RecomputeConversationStats(src1)")
	require.NoError(s.RecomputeConversationStats(src2.ID), "RecomputeConversationStats(src2)")

	// Verify conversation stats for conv1.
	var msgCount int64
	require.NoError(s.DB().QueryRow(
		`SELECT message_count FROM conversations WHERE id = ?`, conv1ID,
	).Scan(&msgCount), "read conv1 stats")
	assert.Equal(int64(4), msgCount, "conv1 message_count")

	// --- TextEngine queries ---
	eng := query.NewSQLiteEngine(s.DB())
	var te query.TextEngine = eng

	// ListConversations — should return both conversations.
	convRows, err := te.ListConversations(ctx, query.TextFilter{})
	require.NoError(err, "ListConversations")
	assert.Len(convRows, 2, "ListConversations: got %d rows, want 2", len(convRows))
	convByID := make(map[int64]query.ConversationRow)
	for _, row := range convRows {
		convByID[row.ConversationID] = row
	}
	wantConv1LastAt := baseTime.Add(2 * time.Minute)
	wantConv2LastAt := baseTime.Add(2 * time.Hour)
	if row, ok := convByID[conv1ID]; assert.True(ok, "conv1 not found in ListConversations results") {
		assert.Equal(int64(4), row.MessageCount, "conv1 MessageCount")
		assert.False(row.LastMessageAt.IsZero(), "conv1 LastMessageAt is zero")
		if !row.LastMessageAt.IsZero() {
			assert.True(row.LastMessageAt.Equal(wantConv1LastAt), "conv1 LastMessageAt: got %v, want %v", row.LastMessageAt, wantConv1LastAt)
		}
		// Preview tie-breaker: wa-3 and wa-4-tiebreaker share the same
		// timestamp; the higher-ID message should win.
		assert.Equal("tiebreaker preview", row.LastPreview, "conv1 LastPreview")
	}
	if row, ok := convByID[conv2ID]; assert.True(ok, "conv2 not found in ListConversations results") {
		assert.Equal(int64(3), row.MessageCount, "conv2 MessageCount")
		assert.False(row.LastMessageAt.IsZero(), "conv2 LastMessageAt is zero")
		if !row.LastMessageAt.IsZero() {
			assert.True(row.LastMessageAt.Equal(wantConv2LastAt), "conv2 LastMessageAt: got %v, want %v", row.LastMessageAt, wantConv2LastAt)
		}
	}

	// TextAggregate by contacts — groups by phone number.
	// All 7 messages have +15551234567 as the from participant
	// (6 via sender_id, 1 via message_recipients fallback with NULL sender_id).
	aggRows, err := te.TextAggregate(ctx, query.TextViewContacts, query.TextAggregateOptions{Limit: 100})
	require.NoError(err, "TextAggregate(TextViewContacts)")
	require.NotEmpty(aggRows, "TextAggregate(TextViewContacts): want at least 1 row")
	foundPhone := false
	for _, row := range aggRows {
		if row.Key == "+15551234567" {
			foundPhone = true
			assert.Equal(int64(7), row.Count, "contact +15551234567 count")
		}
	}
	assert.True(foundPhone, "TextAggregate: phone +15551234567 not found in results")

	// ListConversationMessages — returns messages for conv1 in chronological order.
	messages, err := te.ListConversationMessages(ctx, conv1ID, query.TextFilter{
		SortDirection: query.SortAsc,
	})
	require.NoError(err, "ListConversationMessages(conv1)")
	assert.Len(messages, 4, "ListConversationMessages(conv1)")
	// Verify chronological order (ascending by sent_at).
	for i := 1; i < len(messages); i++ {
		assert.False(messages[i].SentAt.Before(messages[i-1].SentAt), "messages not in chronological order at index %d", i)
	}
	// Verify message type is correct.
	for _, msg := range messages {
		assert.Equal("whatsapp", msg.MessageType, "expected message_type=whatsapp")
	}

	// GetTextStats — should count all 7 text messages.
	stats, err := te.GetTextStats(ctx, query.TextStatsOptions{})
	require.NoError(err, "GetTextStats")
	assert.Equal(int64(7), stats.MessageCount, "GetTextStats.MessageCount")
	// Should see 2 accounts (sources).
	assert.Equal(int64(2), stats.AccountCount, "GetTextStats.AccountCount")
	// LabelCount: 1 label linked to at least one text message.
	assert.Equal(int64(1), stats.LabelCount, "GetTextStats.LabelCount")

	// GetTextStats filtered by source 1 only.
	statsS1, err := te.GetTextStats(ctx, query.TextStatsOptions{SourceID: &src1.ID})
	require.NoError(err, "GetTextStats(src1)")
	assert.Equal(int64(4), statsS1.MessageCount, "GetTextStats(src1).MessageCount")
}
