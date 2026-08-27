package query

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestSQLiteTextSnapshotRevisionConversationsTracksScopedMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
	require.NoError(err)

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
	require.NoError(err)
	assert.NotEmpty(first)

	filter.Pagination = Pagination{Limit: 500, Offset: 0}
	samePageSet, err := engine.TextSnapshotRevision(t.Context(), TextSnapshotScope{Filter: filter})
	require.NoError(err)
	assert.Equal(first, samePageSet)

	_, err = tdb.DB.Exec(`
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title)
			VALUES (703, 8, 'chat-703', 'direct_chat', 'Unrelated');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
			VALUES (803, 703, 8, 'message-803', 'imessage', '2026-08-20 12:00:00', 'unrelated', 12);
		INSERT INTO conversation_participants (conversation_id, participant_id) VALUES (703, 12);
	`)
	require.NoError(err)
	outOfScope, err := engine.TextSnapshotRevision(t.Context(), TextSnapshotScope{Filter: filter})
	require.NoError(err)
	assert.Equal(first, outOfScope)

	_, err = tdb.DB.Exec(`
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title)
			VALUES (704, 7, 'chat-704', 'direct_chat', 'Zulu');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
			VALUES (804, 704, 7, 'message-804', 'imessage', '2026-08-20 13:00:00', 'matching', 11);
		INSERT INTO conversation_participants (conversation_id, participant_id) VALUES (704, 11);
	`)
	require.NoError(err)
	added, err := engine.TextSnapshotRevision(t.Context(), TextSnapshotScope{Filter: filter})
	require.NoError(err)
	assert.NotEqual(first, added)

	_, err = tdb.DB.Exec(`UPDATE conversations SET title = 'Aardvark' WHERE id = 704`)
	require.NoError(err)
	reordered, err := engine.TextSnapshotRevision(t.Context(), TextSnapshotScope{Filter: filter})
	require.NoError(err)
	assert.NotEqual(added, reordered)

	filter.SortDirection = SortDesc
	differentSort, err := engine.TextSnapshotRevision(t.Context(), TextSnapshotScope{Filter: filter})
	require.NoError(err)
	assert.NotEqual(reordered, differentSort)
}

func TestSQLiteTextSnapshotRevisionMessagesTracksMembershipWithoutBodies(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
	require.NoError(err)

	engine := NewSQLiteEngine(tdb.DB)
	conversationID := int64(701)
	filter := TextFilter{
		Pagination:    Pagination{Limit: 1, Offset: 1},
		SortDirection: SortAsc,
	}
	scope := TextSnapshotScope{ConversationID: &conversationID, Filter: filter}
	first, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.NotEmpty(first)

	scope.Filter.Pagination = Pagination{Limit: 500, Offset: 0}
	samePageSet, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.Equal(first, samePageSet)

	_, err = tdb.DB.Exec(`UPDATE message_bodies SET body_text = 'changed full body' WHERE message_id = 801`)
	require.NoError(err)
	bodyOnly, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.Equal(first, bodyOnly)

	_, err = tdb.DB.Exec(`
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
			VALUES (802, 701, 7, 'message-802', 'whatsapp', '2026-08-20 11:00:00', 'second', 11)
	`)
	require.NoError(err)
	added, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.NotEqual(first, added)

	_, err = tdb.DB.Exec(`DELETE FROM messages WHERE id = 802`)
	require.NoError(err)
	deleted, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.Equal(first, deleted)

	_, err = tdb.DB.Exec(`
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
			VALUES (802, 701, 7, 'message-802', 'whatsapp', '2026-08-20 11:00:00', 'second', 11)
	`)
	require.NoError(err)
	reinserted, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.Equal(added, reinserted)

	_, err = tdb.DB.Exec(`UPDATE messages SET conversation_id = 702 WHERE id = 802`)
	require.NoError(err)
	moved, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.Equal(first, moved)

	scope.Filter.ContactName = "different logical request"
	differentFilter, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.NotEqual(moved, differentFilter)
}

func TestSQLiteTextSnapshotReadsReturnPageAndFullRevisionTogether(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	_, err := tdb.DB.Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES (7, 'imessage', 'owner@chat.test');
		INSERT INTO participants (id, phone_number, display_name) VALUES (11, '+15550000011', 'Alice');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title)
			VALUES
				(701, 7, 'chat-701', 'direct_chat', 'Alpha'),
				(702, 7, 'chat-702', 'direct_chat', 'Beta');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, subject, snippet, sender_id)
			VALUES
				(801, 701, 7, 'message-801', 'imessage', '2026-08-20 10:00:00', 'First', 'first', 11),
				(802, 701, 7, 'message-802', 'imessage', '2026-08-20 11:00:00', 'Second', 'second', 11),
				(803, 701, 7, 'message-803', 'imessage', '2026-08-20 12:00:00', 'Third', 'third', 11),
				(804, 702, 7, 'message-804', 'imessage', '2026-08-20 13:00:00', 'Fourth', 'fourth', 11);
	`)
	require.NoError(err)

	engine := NewSQLiteEngine(tdb.DB)
	conversationFilter := TextFilter{
		Pagination:    Pagination{Limit: 1, Offset: 1},
		SortField:     TextSortByName,
		SortDirection: SortAsc,
	}
	conversations, conversationRevision, err := engine.ListConversationsSnapshot(
		t.Context(), conversationFilter,
	)
	require.NoError(err)
	require.Len(conversations, 1)
	assert.Equal(int64(702), conversations[0].ConversationID)
	assert.NotEmpty(conversationRevision)

	conversationFilter.Pagination = Pagination{Limit: 500}
	fullConversationRevision, err := engine.TextSnapshotRevision(
		t.Context(), TextSnapshotScope{Filter: conversationFilter},
	)
	require.NoError(err)
	assert.Equal(conversationRevision, fullConversationRevision)

	messageFilter := TextFilter{
		Pagination:    Pagination{Limit: 2, Offset: 1},
		SortDirection: SortAsc,
	}
	messages, messageRevision, err := engine.ListConversationMessagesSnapshot(
		t.Context(), 701, messageFilter,
	)
	require.NoError(err)
	require.Len(messages, 2)
	assert.Equal([]int64{802, 803}, []int64{messages[0].ID, messages[1].ID})
	assert.NotEmpty(messageRevision)

	messageFilter.Pagination = Pagination{Limit: 500}
	conversationID := int64(701)
	fullMessageRevision, err := engine.TextSnapshotRevision(t.Context(), TextSnapshotScope{
		ConversationID: &conversationID,
		Filter:         messageFilter,
	})
	require.NoError(err)
	assert.Equal(messageRevision, fullMessageRevision)
}

// TestSQLiteListConversationsParticipantMembership ensures the participant
// filter uses the conversation roster rather than the latest message sender.
// In particular, an owner-authored latest message must not hide a conversation
// that contains a selected cluster member.
func TestSQLiteListConversationsParticipantMembership(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
			(704, 8, 'other-source', 'direct_chat', 'other source'),
			(705, 7, 'sender-fallback', 'direct_chat', 'sender fallback'),
			(706, 7, 'recipient-fallback', 'direct_chat', 'recipient fallback'),
			(707, 7, 'unrelated-fallback', 'direct_chat', 'unrelated fallback');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id) VALUES
			(701, 701, 7, 'received', 'imessage', '2024-01-01 10:00:00', 'received', 11),
			(702, 702, 7, 'owner-earlier', 'imessage', '2024-01-01 10:00:00', 'owner earlier', 14),
			(703, 702, 7, 'owner-latest', 'imessage', '2024-01-01 11:00:00', 'owner latest', 15),
			(704, 703, 7, 'same-name', 'imessage', '2024-01-01 10:00:00', 'same name', 16),
			(705, 704, 8, 'other-source', 'imessage', '2024-01-01 10:00:00', 'other source', 11),
			(706, 705, 7, 'sender-match', 'imessage', '2024-01-01 10:00:00', 'sender match', 11),
			(707, 705, 7, 'sender-other', 'imessage', '2024-01-01 11:00:00', 'sender other', 15),
			(708, 706, 7, 'recipient-match', 'imessage', '2024-01-01 10:00:00', 'recipient match', 15),
			(709, 706, 7, 'recipient-other', 'imessage', '2024-01-01 11:00:00', 'recipient other', 15),
			(710, 707, 7, 'unrelated-sender', 'imessage', '2024-01-01 10:00:00', 'unrelated sender', 16),
			(711, 707, 7, 'unrelated-recipient', 'imessage', '2024-01-01 11:00:00', 'unrelated recipient', 15);
		INSERT INTO conversation_participants (conversation_id, participant_id) VALUES
			(701, 11), (702, 14), (702, 15), (703, 16), (704, 11);
		INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES
			(708, 14, 'to'), (711, 16, 'to');
	`)
	require.NoError(err)

	sourceID := int64(7)
	rows, err := NewSQLiteEngine(tdb.DB).ListConversations(context.Background(), TextFilter{
		SourceID:       &sourceID,
		ParticipantIDs: []int64{11, 14},
	})
	require.NoError(err)
	require.Len(rows, 4)

	conversationIDs := make([]int64, len(rows))
	byID := make(map[int64]ConversationRow, len(rows))
	for i, row := range rows {
		conversationIDs[i] = row.ConversationID
		byID[row.ConversationID] = row
	}
	assert.ElementsMatch([]int64{701, 702, 705, 706}, conversationIDs)
	assert.Equal(int64(2), byID[702].MessageCount)
	assert.Equal("owner latest", byID[702].LastPreview)
	assert.Equal(int64(2), byID[705].MessageCount)
	assert.Equal(int64(2), byID[706].MessageCount)
}

func TestSQLiteListConversationMessagesWithoutBodies(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
	require.NoError(err)

	rows, err := NewSQLiteEngine(tdb.DB).ListConversationMessages(t.Context(), 701, TextFilter{})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal("safe snippet", rows[0].Snippet)
	assert.Empty(rows[0].BodyText)
}

func TestSQLiteListConversationMessagesSearchesFullBodyWithinConversation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	_, err := tdb.DB.Exec(`
		CREATE VIRTUAL TABLE messages_fts USING fts5(
			message_id UNINDEXED, subject, body, from_addr, to_addr, cc_addr
		);
		INSERT INTO sources (id, source_type, identifier) VALUES (7, 'whatsapp', 'chat');
		INSERT INTO participants (id, display_name) VALUES (11, 'Alice');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title) VALUES
			(701, 7, 'chat-701', 'direct_chat', 'Selected'),
			(702, 7, 'chat-702', 'direct_chat', 'Outside');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id) VALUES
			(801, 701, 7, 'message-801', 'whatsapp', '2026-08-20 10:00:00', 'short preview', 11),
			(802, 701, 7, 'message-802', 'whatsapp', '2026-08-20 11:00:00', 'another preview', 11),
			(803, 702, 7, 'message-803', 'whatsapp', '2026-08-20 12:00:00', 'outside preview', 11);
		INSERT INTO messages_fts (rowid, message_id, subject, body) VALUES
			(801, 801, '', 'opening text followed much later by hiddenneedle'),
			(802, 802, '', 'body without the search term'),
			(803, 803, '', 'hiddenneedle in another conversation');
	`)
	require.NoError(err)

	rows, err := NewSQLiteEngine(tdb.DB).ListConversationMessages(t.Context(), 701, TextFilter{
		SearchQuery: "hiddenneedle",
	})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(int64(801), rows[0].ID)
	assert.Equal("short preview", rows[0].Snippet)
	assert.Empty(rows[0].BodyText)
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
