package query

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestDuckDBTextSnapshotRevisionUsesCommittedCacheMarker(t *testing.T) {
	b := NewTestDataBuilder(t)
	sourceID := b.AddSourceWithType("owner@chat.test", "imessage")
	participantID := b.AddPhoneParticipant("+15550000011", "Alice")
	messageID := b.AddMessage(MessageOpt{MessageType: "imessage", SourceID: sourceID, SenderID: &participantID})
	b.AddFrom(messageID, participantID, "Alice")

	analyticsDir, cleanup := b.Build()
	t.Cleanup(cleanup)
	engine, err := NewDuckDBEngine(analyticsDir, "", nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })

	filter := TextFilter{
		SourceID:       &sourceID,
		ParticipantIDs: []int64{participantID},
		Pagination:     Pagination{Limit: 1, Offset: 100},
		SortField:      TextSortByLastMessage,
		SortDirection:  SortDesc,
	}
	first, err := engine.TextSnapshotRevision(t.Context(), TextSnapshotScope{Filter: filter})
	require.NoError(t, err)
	assert.NotEmpty(t, first)

	state, err := ReadCacheSyncState(analyticsDir)
	require.NoError(t, err)
	state.PublishedAt = state.PublishedAt.Add(time.Second)
	data, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(CacheStatePath(analyticsDir), data, 0o600))

	second, err := engine.TextSnapshotRevision(t.Context(), TextSnapshotScope{Filter: filter})
	require.NoError(t, err)
	assert.NotEqual(t, first, second)

	filter.Pagination = Pagination{Limit: 500, Offset: 0}
	sameLogicalRequest, err := engine.TextSnapshotRevision(t.Context(), TextSnapshotScope{Filter: filter})
	require.NoError(t, err)
	assert.Equal(t, second, sameLogicalRequest)
}

func TestDuckDBTextSnapshotRevisionTracksHybridSQLiteTimeline(t *testing.T) {
	b := NewTestDataBuilder(t)
	sourceID := b.AddSourceWithType("owner@chat.test", "whatsapp")
	participantID := b.AddPhoneParticipant("+15550000011", "Alice")
	b.AddMessage(MessageOpt{
		MessageType:    "whatsapp",
		SourceID:       sourceID,
		SenderID:       &participantID,
		ConversationID: 701,
	})
	analyticsDir, cleanup := b.Build()
	t.Cleanup(cleanup)

	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	_, err := tdb.DB.Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES (7, 'whatsapp', 'owner@chat.test');
		INSERT INTO participants (id, phone_number, display_name) VALUES (11, '+15550000011', 'Alice');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title)
			VALUES (701, 7, 'chat-701', 'direct_chat', 'Alpha');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
			VALUES (801, 701, 7, 'message-801', 'whatsapp', '2026-08-20 10:00:00', 'first', 11);
	`)
	require.NoError(t, err)

	engine, err := NewDuckDBEngine(analyticsDir, "", tdb.DB)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })

	conversationID := int64(701)
	scope := TextSnapshotScope{
		ConversationID: &conversationID,
		Filter: TextFilter{
			SortDirection: SortAsc,
			Pagination:    Pagination{Limit: 25},
		},
	}
	before, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(t, err)

	_, err = tdb.DB.Exec(`
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
			VALUES (802, 701, 7, 'message-802', 'whatsapp', '2026-08-20 11:00:00', 'second', 11)
	`)
	require.NoError(t, err)

	rows, err := engine.ListConversationMessages(t.Context(), conversationID, scope.Filter)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, []int64{801, 802}, []int64{rows[0].ID, rows[1].ID})

	after, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(t, err)
	assert.NotEqual(t, before, after)
}

func TestDuckDBListConversationMessagesUsesIDTiebreakerForEqualTimestamps(t *testing.T) {
	b := NewTestDataBuilder(t)
	sourceID := b.AddSourceWithType("owner@chat.test", "whatsapp")
	participantID := b.AddPhoneParticipant("+15550000011", "Alice")
	for range 3 {
		messageID := b.AddMessage(MessageOpt{
			MessageType:    "whatsapp",
			SourceID:       sourceID,
			SenderID:       &participantID,
			ConversationID: 701,
			SentAt:         time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC),
		})
		b.AddFrom(messageID, participantID, "Alice")
	}
	engine := b.BuildEngine()

	for _, tc := range []struct {
		name      string
		direction SortDirection
		want      []int64
	}{
		{name: "ascending", direction: SortAsc, want: []int64{1, 2, 3}},
		{name: "descending", direction: SortDesc, want: []int64{3, 2, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make([]int64, 0, 3)
			for offset := range 3 {
				rows, err := engine.ListConversationMessages(t.Context(), 701, TextFilter{
					SortDirection: tc.direction,
					Pagination:    Pagination{Limit: 1, Offset: offset},
				})
				require.NoError(t, err)
				require.Len(t, rows, 1)
				got = append(got, rows[0].ID)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// buildTextContactsEngine builds a DuckDB engine over a small Parquet dataset
// shaped like real text data: an iMessage source, phone/email participants, and
// messages whose sender is recorded on messages.sender_id (iMessage/SMS shape)
// or only on a message_recipients row of type 'from' (Messenger shape). It also
// includes an email message that must be excluded from text aggregates.
func buildTextContactsEngine(t *testing.T) *DuckDBEngine {
	t.Helper()
	b := NewTestDataBuilder(t)

	smsSrc := b.AddSourceWithType("me@imessage.local", "imessage")
	emailSrc := b.AddSourceWithType("me@gmail.com", "gmail")

	alice := b.AddPhoneParticipant("+14155550001", "Alice")
	bob := b.AddPhoneParticipant("+14155550002", "Bob")
	emailSender := b.AddParticipant("carol@example.com", "example.com", "Carol")

	// Two iMessages from Alice, sender on messages.sender_id (direct shape).
	m1 := b.AddMessage(MessageOpt{MessageType: "imessage", SourceID: smsSrc, SenderID: &alice})
	m2 := b.AddMessage(MessageOpt{MessageType: "imessage", SourceID: smsSrc, SenderID: &alice})
	b.AddFrom(m1, alice, "Alice")
	b.AddFrom(m2, alice, "Alice")

	// One SMS from Bob, sender recorded ONLY via the 'from' recipient row
	// (no messages.sender_id) — exercises the COALESCE fallback.
	m3 := b.AddMessage(MessageOpt{MessageType: "sms", SourceID: smsSrc})
	b.AddFrom(m3, bob, "Bob")

	// One email — must NOT appear in the text contacts aggregate.
	m4 := b.AddMessage(MessageOpt{MessageType: "email", SourceID: emailSrc, SenderID: &emailSender})
	b.AddFrom(m4, emailSender, "Carol")

	return b.BuildEngine()
}

// TestDuckDBTextAggregate_Contacts guards the DuckDB text Contacts aggregate.
// A prior implementation embedded a correlated scalar subquery in the JOIN ON
// clause, which DuckDB could not optimize over a large messages dataset; this
// asserts the view returns the expected non-email contacts.
func TestDuckDBTextAggregate_Contacts(t *testing.T) {
	engine := buildTextContactsEngine(t)
	ctx := context.Background()

	tests := []struct {
		name     string
		view     TextViewType
		wantKeys map[string]int64 // key -> message count
		absent   string           // key that must not appear (the email sender)
	}{
		{
			name: "contacts (phone/email key)",
			view: TextViewContacts,
			wantKeys: map[string]int64{
				"+14155550001": 2, // Alice, via sender_id
				"+14155550002": 1, // Bob, via 'from' recipient fallback
			},
			absent: "carol@example.com",
		},
		{
			name: "contact names (display name key)",
			view: TextViewContactNames,
			wantKeys: map[string]int64{
				"Alice": 2,
				"Bob":   1,
			},
			absent: "Carol",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			rows, err := engine.TextAggregate(ctx, tc.view, TextAggregateOptions{})
			require.NoError(err)
			require.NotEmpty(rows, "contacts aggregate must not be empty")

			got := make(map[string]int64, len(rows))
			for _, r := range rows {
				got[r.Key] = r.Count
			}
			for key, count := range tc.wantKeys {
				assert.Equal(count, got[key], "count for %q", key)
			}
			_, present := got[tc.absent]
			assert.False(present, "email sender %q must be excluded from text aggregate", tc.absent)
		})
	}
}

// TestDuckDBListConversationsParticipantMembership ensures the participant
// filter uses the conversation roster rather than the latest message sender.
// In particular, an owner-authored latest message must not hide a conversation
// that contains a selected cluster member.
func TestDuckDBListConversationsParticipantMembership(t *testing.T) {
	b := NewTestDataBuilder(t)
	sourceID := b.AddSourceWithType("owner@chat.test", "imessage")
	otherSourceID := b.AddSourceWithType("owner@other.test", "imessage")

	aliceID := b.AddPhoneParticipant("+15550000011", "Alice Cluster")
	aliceAliasID := b.AddPhoneParticipant("+15550000014", "Alice Alias")
	ownerID := b.AddPhoneParticipant("+15550000015", "Owner")
	sameNameID := b.AddPhoneParticipant("+15550000016", "Alice Cluster")

	const (
		receivedConversationID = int64(701)
		ownerConversationID    = int64(702)
		sameNameConversationID = int64(703)
		otherSourceConvID      = int64(704)
	)

	b.AddMessage(MessageOpt{SourceID: sourceID, ConversationID: receivedConversationID, MessageType: "imessage", SenderID: &aliceID})
	b.AddConversationParticipant(receivedConversationID, aliceID)

	b.AddMessage(MessageOpt{SourceID: sourceID, ConversationID: ownerConversationID, MessageType: "imessage", SenderID: &aliceAliasID})
	b.AddMessage(MessageOpt{SourceID: sourceID, ConversationID: ownerConversationID, MessageType: "imessage", SenderID: &ownerID})
	b.AddConversationParticipant(ownerConversationID, aliceAliasID)
	b.AddConversationParticipant(ownerConversationID, ownerID)

	b.AddMessage(MessageOpt{SourceID: sourceID, ConversationID: sameNameConversationID, MessageType: "imessage", SenderID: &sameNameID})
	b.AddConversationParticipant(sameNameConversationID, sameNameID)

	b.AddMessage(MessageOpt{SourceID: otherSourceID, ConversationID: otherSourceConvID, MessageType: "imessage", SenderID: &aliceID})
	b.AddConversationParticipant(otherSourceConvID, aliceID)

	engine := b.BuildEngine()
	rows, err := engine.ListConversations(context.Background(), TextFilter{
		SourceID:       &sourceID,
		ParticipantIDs: []int64{aliceID, aliceAliasID},
	})
	require.NoError(t, err)
	require.Len(t, rows, 2)

	conversationIDs := make([]int64, len(rows))
	for i, row := range rows {
		conversationIDs[i] = row.ConversationID
	}
	assert.ElementsMatch(t, []int64{receivedConversationID, ownerConversationID}, conversationIDs)
}
