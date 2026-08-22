package query

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestSQLiteTextSnapshotRevisionConversationsTracksScopedMetadata(t *testing.T) {
	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	_, err := tdb.DB.Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES
			(7, 'imessage', 'owner@chat.test'),
			(8, 'imessage', 'owner@other.test');
		INSERT INTO participants (id, phone_number, display_name) VALUES
			(11, '+15550000011', 'Alice'),
			(12, '+15550000012', 'Bob');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title) VALUES
			(701, 7, 'chat-701', 'direct_chat', 'Alpha'),
			(702, 8, 'chat-702', 'direct_chat', 'Outside');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id) VALUES
			(801, 701, 7, 'message-801', 'imessage', '2026-08-20 10:00:00', 'first', 11),
			(802, 702, 8, 'message-802', 'imessage', '2026-08-20 11:00:00', 'outside', 12);
		INSERT INTO conversation_participants (conversation_id, participant_id) VALUES
			(701, 11), (702, 12);
	`)
	require.NoError(t, err)

	engine := NewSQLiteEngine(tdb.DB)
	sourceID := int64(7)
	filter := TextFilter{
		SourceID:       &sourceID,
		ParticipantIDs: []int64{11},
		Pagination:     Pagination{Limit: 1, Offset: 1},
		SortField:      TextSortByName,
		SortDirection:  SortAsc,
	}
	first, err := engine.TextSnapshotRevision(t.Context(), TextSnapshotScope{Filter: filter})
	require.NoError(t, err)
	assert.NotEmpty(t, first)

	filter.Pagination = Pagination{Limit: 500, Offset: 0}
	samePageSet, err := engine.TextSnapshotRevision(t.Context(), TextSnapshotScope{Filter: filter})
	require.NoError(t, err)
	assert.Equal(t, first, samePageSet)

	_, err = tdb.DB.Exec(`
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title)
			VALUES (703, 8, 'chat-703', 'direct_chat', 'Unrelated');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
			VALUES (803, 703, 8, 'message-803', 'imessage', '2026-08-20 12:00:00', 'unrelated', 12);
		INSERT INTO conversation_participants (conversation_id, participant_id) VALUES (703, 12);
	`)
	require.NoError(t, err)
	outOfScope, err := engine.TextSnapshotRevision(t.Context(), TextSnapshotScope{Filter: filter})
	require.NoError(t, err)
	assert.Equal(t, first, outOfScope)

	_, err = tdb.DB.Exec(`
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title)
			VALUES (704, 7, 'chat-704', 'direct_chat', 'Zulu');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
			VALUES (804, 704, 7, 'message-804', 'imessage', '2026-08-20 13:00:00', 'matching', 11);
		INSERT INTO conversation_participants (conversation_id, participant_id) VALUES (704, 11);
	`)
	require.NoError(t, err)
	added, err := engine.TextSnapshotRevision(t.Context(), TextSnapshotScope{Filter: filter})
	require.NoError(t, err)
	assert.NotEqual(t, first, added)

	_, err = tdb.DB.Exec(`UPDATE conversations SET title = 'Aardvark' WHERE id = 704`)
	require.NoError(t, err)
	reordered, err := engine.TextSnapshotRevision(t.Context(), TextSnapshotScope{Filter: filter})
	require.NoError(t, err)
	assert.NotEqual(t, added, reordered)

	filter.SortDirection = SortDesc
	differentSort, err := engine.TextSnapshotRevision(t.Context(), TextSnapshotScope{Filter: filter})
	require.NoError(t, err)
	assert.NotEqual(t, reordered, differentSort)
}

func TestSQLiteTextSnapshotRevisionMessagesTracksMembershipWithoutBodies(t *testing.T) {
	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	_, err := tdb.DB.Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES (7, 'whatsapp', 'owner@chat.test');
		INSERT INTO participants (id, phone_number, display_name) VALUES (11, '+15550000011', 'Alice');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title) VALUES
			(701, 7, 'chat-701', 'direct_chat', 'Alpha'),
			(702, 7, 'chat-702', 'direct_chat', 'Beta');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id) VALUES
			(801, 701, 7, 'message-801', 'whatsapp', '2026-08-20 10:00:00', 'first', 11);
		INSERT INTO message_bodies (message_id, body_text) VALUES (801, 'full first body');
	`)
	require.NoError(t, err)

	engine := NewSQLiteEngine(tdb.DB)
	conversationID := int64(701)
	filter := TextFilter{
		Pagination:    Pagination{Limit: 1, Offset: 1},
		SortDirection: SortAsc,
	}
	scope := TextSnapshotScope{ConversationID: &conversationID, Filter: filter}
	first, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(t, err)
	assert.NotEmpty(t, first)

	scope.Filter.Pagination = Pagination{Limit: 500, Offset: 0}
	samePageSet, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(t, err)
	assert.Equal(t, first, samePageSet)

	_, err = tdb.DB.Exec(`UPDATE message_bodies SET body_text = 'changed full body' WHERE message_id = 801`)
	require.NoError(t, err)
	bodyOnly, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(t, err)
	assert.Equal(t, first, bodyOnly)

	_, err = tdb.DB.Exec(`
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
			VALUES (802, 701, 7, 'message-802', 'whatsapp', '2026-08-20 11:00:00', 'second', 11)
	`)
	require.NoError(t, err)
	added, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(t, err)
	assert.NotEqual(t, first, added)

	_, err = tdb.DB.Exec(`DELETE FROM messages WHERE id = 802`)
	require.NoError(t, err)
	deleted, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(t, err)
	assert.Equal(t, first, deleted)

	_, err = tdb.DB.Exec(`
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
			VALUES (802, 701, 7, 'message-802', 'whatsapp', '2026-08-20 11:00:00', 'second', 11)
	`)
	require.NoError(t, err)
	reinserted, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(t, err)
	assert.Equal(t, added, reinserted)

	_, err = tdb.DB.Exec(`UPDATE messages SET conversation_id = 702 WHERE id = 802`)
	require.NoError(t, err)
	moved, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(t, err)
	assert.Equal(t, first, moved)

	scope.Filter.ContactName = "different logical request"
	differentFilter, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(t, err)
	assert.NotEqual(t, moved, differentFilter)
}

// TestSQLiteListConversationsParticipantMembership ensures the participant
// filter uses the conversation roster rather than the latest message sender.
// In particular, an owner-authored latest message must not hide a conversation
// that contains a selected cluster member.
func TestSQLiteListConversationsParticipantMembership(t *testing.T) {
	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	_, err := tdb.DB.Exec(`
		INSERT INTO sources (id, source_type, identifier, display_name) VALUES
			(7, 'imessage', 'owner@chat.test', 'Owner Chat'),
			(8, 'imessage', 'owner@other.test', 'Other Chat');
		INSERT INTO participants (id, phone_number, display_name) VALUES
			(11, '+15550000011', 'Alice Cluster'),
			(14, '+15550000014', 'Alice Alias'),
			(15, '+15550000015', 'Owner'),
			(16, '+15550000016', 'Alice Cluster');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title) VALUES
			(701, 7, 'received', 'direct_chat', 'received'),
			(702, 7, 'owner-latest', 'direct_chat', 'owner latest'),
			(703, 7, 'same-name', 'direct_chat', 'same name'),
			(704, 8, 'other-source', 'direct_chat', 'other source');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id) VALUES
			(701, 701, 7, 'received', 'imessage', '2024-01-01 10:00:00', 'received', 11),
			(702, 702, 7, 'owner-earlier', 'imessage', '2024-01-01 10:00:00', 'owner earlier', 14),
			(703, 702, 7, 'owner-latest', 'imessage', '2024-01-01 11:00:00', 'owner latest', 15),
			(704, 703, 7, 'same-name', 'imessage', '2024-01-01 10:00:00', 'same name', 16),
			(705, 704, 8, 'other-source', 'imessage', '2024-01-01 10:00:00', 'other source', 11);
		INSERT INTO conversation_participants (conversation_id, participant_id) VALUES
			(701, 11), (702, 14), (702, 15), (703, 16), (704, 11);
	`)
	require.NoError(t, err)

	sourceID := int64(7)
	rows, err := NewSQLiteEngine(tdb.DB).ListConversations(context.Background(), TextFilter{
		SourceID:       &sourceID,
		ParticipantIDs: []int64{11, 14},
	})
	require.NoError(t, err)
	require.Len(t, rows, 2)

	conversationIDs := make([]int64, len(rows))
	byID := make(map[int64]ConversationRow, len(rows))
	for i, row := range rows {
		conversationIDs[i] = row.ConversationID
		byID[row.ConversationID] = row
	}
	assert.ElementsMatch(t, []int64{701, 702}, conversationIDs)
	assert.Equal(t, int64(2), byID[702].MessageCount)
	assert.Equal(t, "owner latest", byID[702].LastPreview)
}

func TestSQLiteListConversationMessagesWithoutBodies(t *testing.T) {
	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	_, err := tdb.DB.Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES (7, 'whatsapp', 'chat');
		INSERT INTO participants (id, display_name) VALUES (11, 'Alice');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title)
			VALUES (701, 7, 'chat-701', 'direct_chat', 'Metadata only');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
			VALUES (801, 701, 7, 'message-801', 'whatsapp', '2026-08-20 10:00:00', 'safe snippet', 11);
		DROP TABLE message_bodies;
	`)
	require.NoError(t, err)

	rows, err := NewSQLiteEngine(tdb.DB).ListConversationMessages(t.Context(), 701, TextFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "safe snippet", rows[0].Snippet)
	assert.Empty(t, rows[0].BodyText)
}

func TestSQLiteListConversationMessagesUsesIDTiebreakerForEqualTimestamps(t *testing.T) {
	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	_, err := tdb.DB.Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES (7, 'whatsapp', 'chat');
		INSERT INTO participants (id, display_name) VALUES (11, 'Alice');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title)
			VALUES (701, 7, 'chat-701', 'direct_chat', 'Equal timestamps');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id) VALUES
			(803, 701, 7, 'message-803', 'whatsapp', '2026-08-20 10:00:00', 'third', 11),
			(801, 701, 7, 'message-801', 'whatsapp', '2026-08-20 10:00:00', 'first', 11),
			(802, 701, 7, 'message-802', 'whatsapp', '2026-08-20 10:00:00', 'second', 11);
	`)
	require.NoError(t, err)

	engine := NewSQLiteEngine(tdb.DB)
	for _, tc := range []struct {
		name      string
		direction SortDirection
		want      []int64
	}{
		{name: "ascending", direction: SortAsc, want: []int64{801, 802, 803}},
		{name: "descending", direction: SortDesc, want: []int64{803, 802, 801}},
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
