package query_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSQLiteTimelineSnapshotProbeUsesCoveringIndex(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	s := f.Store
	rows, err := s.DB().Query("EXPLAIN QUERY PLAN "+query.SQLiteTimelineSnapshotProbeForTest, 1, 1, 1)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(rows.Scan(&id, &parent, &unused, &detail))
		plan.WriteString(detail + "\n")
	}
	require.NoError(rows.Err())
	assert.Contains(plan.String(), "COVERING INDEX idx_messages_conversation_content_changed")
	assert.Equal(1, strings.Count(plan.String(), "COVERING INDEX idx_messages_conversation_content_changed"))
	assert.NotContains(plan.String(), "SCAN m")
}

func TestSQLiteTimelineSnapshotRevisionFencesRecentChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("recent-message")
	for _, tc := range []struct {
		offset string
		recent bool
	}{{"+0 seconds", true}, {"-0.001 seconds", true}, {"-0.003 seconds", false}} {
		var recent bool
		err := f.Store.DB().QueryRow(`UPDATE messages SET content_changed_at = strftime('%Y-%m-%d %H:%M:%f', 'now', ?) WHERE id = ? RETURNING (SELECT recent FROM (`+query.SQLiteTimelineSnapshotProbeForTest+`))`, tc.offset, id, f.ConvID, f.ConvID, f.ConvID).Scan(&recent)
		require.NoError(err)
		assert.Equal(tc.recent, recent, tc.offset)
	}
	// A future stamp keeps the fence active even if the test runner pauses
	// between the two reads.
	_, err := f.Store.DB().Exec(`UPDATE messages SET message_type = 'imessage', content_changed_at = strftime('%Y-%m-%d %H:%M:%f', 'now', '+1 second') WHERE id = ?`, id)
	require.NoError(err)
	engine := query.NewSQLiteEngine(f.Store.DB())
	scope := query.TextSnapshotScope{ConversationID: &f.ConvID}
	first, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	second, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.NotEqual(first, second, "each read at or ahead of the current clock must get a new revision")

	_, err = f.Store.DB().Exec(`UPDATE messages SET content_changed_at = strftime('%Y-%m-%d %H:%M:%f', 'now', '-1 second') WHERE id = ?`, id)
	require.NoError(err)
	first, err = engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	second, err = engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.Equal(first, second)
}

func TestSQLiteTimelineSnapshotRevisionTracksMessageChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want []int64
	}{
		{"insert", `INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at)
			VALUES (803, 701, 7, 'message-803', 'imessage', '2020-01-01 12:00:00')`, []int64{801, 802, 803}},
		{"hard delete", `DELETE FROM messages WHERE id = 802`, []int64{801}},
		{"sent_at edit", `UPDATE messages SET sent_at = '2020-01-01 09:00:00' WHERE id = 802`, []int64{802, 801}},
		{"snippet edit", `UPDATE messages SET snippet = 'edited' WHERE id = 802`, []int64{801, 802}},
		{"move out", `UPDATE messages SET conversation_id = 702 WHERE id = 802`, []int64{801}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			s := f.Store
			_, err := s.DB().Exec(`
				INSERT INTO sources (id, source_type, identifier) VALUES (7, 'imessage', 'owner@example.com');
				INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type) VALUES
					(701, 7, 'chat-701', 'direct_chat'), (702, 7, 'chat-702', 'direct_chat');
				INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, content_changed_at) VALUES
					(801, 701, 7, 'message-801', 'imessage', '2020-01-01 10:00:00', 'first', '2020-01-01 00:00:00'),
					(802, 701, 7, 'message-802', 'imessage', '2020-01-01 11:00:00', 'second', '2020-01-01 00:00:00');
			`)
			require.NoError(err)
			engine := query.NewSQLiteEngine(s.DB())
			conversationID := int64(701)
			filter := query.TextFilter{Pagination: query.Pagination{Limit: 1}, SortDirection: query.SortAsc}
			page, before, err := engine.ListConversationMessagesSnapshot(t.Context(), conversationID, filter)
			require.NoError(err)
			require.Len(page, 1)
			assert.Equal(int64(801), page[0].ID)
			filter.Pagination.Offset = 1
			page, next, err := engine.ListConversationMessagesSnapshot(t.Context(), conversationID, filter)
			require.NoError(err)
			require.Len(page, 1)
			assert.Equal(int64(802), page[0].ID)
			assert.Equal(before, next)

			_, err = s.DB().Exec(tc.sql)
			require.NoError(err)
			_, err = s.DB().Exec(`UPDATE messages SET content_changed_at = '2021-01-01 00:00:00' WHERE content_changed_at > '2022-01-01'`)
			require.NoError(err)
			filter.Pagination = query.Pagination{}
			page, after, err := engine.ListConversationMessagesSnapshot(t.Context(), conversationID, filter)
			require.NoError(err)
			assert.NotEqual(before, after)
			var ids []int64
			for _, message := range page {
				ids = append(ids, message.ID)
			}
			assert.Equal(tc.want, ids)
			if tc.name == "snippet edit" {
				assert.Equal("edited", page[1].Snippet)
			}
			scope := query.TextSnapshotScope{ConversationID: &conversationID, Filter: filter}
			revision, err := engine.TextSnapshotRevision(t.Context(), scope)
			require.NoError(err)
			assert.Equal(after, revision)
			scope.Filter.ContactName = "different request"
			differentFilter, err := engine.TextSnapshotRevision(t.Context(), scope)
			require.NoError(err)
			assert.NotEqual(after, differentFilter)
		})
	}
}

func TestSQLiteTimelineSnapshotRevisionTracksDisplayedChanges(t *testing.T) {
	for _, change := range []string{"sender name", "conversation title", "body edit"} {
		t.Run(change, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			s := f.Store
			_, err := s.DB().Exec(`
				INSERT INTO sources (id, source_type, identifier) VALUES (7, 'imessage', 'owner@example.com');
				INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title, updated_at)
					VALUES (701, 7, 'chat-701', 'direct_chat', 'Original', '2020-01-01 00:00:00');
				INSERT INTO participants (id, email_address, display_name) VALUES (11, 'sender@example.com', 'Original');
				INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, sender_id, snippet, content_changed_at) VALUES
					(801, 701, 7, 'message-801', 'imessage', '2020-01-01 10:00:00', 11, 'first', '2020-01-01 00:00:00.000'),
					(802, 701, 7, 'message-802', 'imessage', '2020-01-01 11:00:00', 11, 'second', '2020-01-01 00:00:00.000');
				INSERT INTO message_bodies (message_id, body_text) VALUES (802, 'original body');
				UPDATE messages SET content_changed_at = '2020-01-01 00:00:00.000' WHERE id = 802;
			`)
			require.NoError(err)
			engine := query.NewSQLiteEngine(s.DB())
			filter := query.TextFilter{SortDirection: query.SortAsc}
			page, before, err := engine.ListConversationMessagesSnapshot(t.Context(), 701, filter)
			require.NoError(err)
			require.Len(page, 2)
			assert.Equal("Original", page[0].FromName)
			assert.Equal("Original", page[0].ConversationTitle)
			var maxBefore string
			require.NoError(s.DB().QueryRow(`SELECT MAX(content_changed_at) FROM messages WHERE conversation_id = 701`).Scan(&maxBefore))

			switch change {
			case "sender name":
				require.NoError(s.RepairParticipantDisplayNames([]store.ParticipantDisplayNameRepair{{ParticipantID: 11, DisplayName: "Updated"}}))
			case "conversation title":
				require.NoError(s.SetConversationTitle(7, 701, "Updated"))
				// Title updates can share SQLite's one-second updated_at value.
				_, err = s.DB().Exec(`UPDATE conversations SET updated_at = '2020-01-01 00:00:00' WHERE id = 701`)
				require.NoError(err)
			case "body edit":
				_, err = s.DB().Exec(`UPDATE message_bodies SET body_text = 'updated body' WHERE message_id = 802`)
				require.NoError(err)
			}
			page, after, err := engine.ListConversationMessagesSnapshot(t.Context(), 701, filter)
			require.NoError(err)
			require.Len(page, 2)
			assert.NotEqual(before, after)
			var maxAfter string
			require.NoError(s.DB().QueryRow(`SELECT MAX(content_changed_at) FROM messages WHERE conversation_id = 701`).Scan(&maxAfter))
			if change != "body edit" {
				assert.Equal(maxBefore, maxAfter)
			}
			switch change {
			case "sender name":
				assert.Equal("Updated", page[0].FromName)
			case "conversation title":
				assert.Equal("Updated", page[0].ConversationTitle)
			case "body edit":
				var body string
				require.NoError(s.DB().QueryRow(`SELECT body_text FROM message_bodies WHERE message_id = 802`).Scan(&body))
				assert.Equal("updated body", body)
			}
		})
	}
}

func TestSQLiteTextSnapshotRevisionConversationsTracksScopedMetadata(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	db := f.Store.DB()
	_, err := db.Exec(`
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

	engine := query.NewSQLiteEngine(db)
	sourceID := int64(7)
	filter := query.TextFilter{
		SourceID:       &sourceID,
		ParticipantIDs: []int64{11},
		Pagination:     query.Pagination{Limit: 1, Offset: 1},
		SortField:      query.TextSortByName,
		SortDirection:  query.SortAsc,
	}
	first, err := engine.TextSnapshotRevision(t.Context(), query.TextSnapshotScope{Filter: filter})
	require.NoError(err)
	assert.NotEmpty(first)

	filter.Pagination = query.Pagination{Limit: 500, Offset: 0}
	samePageSet, err := engine.TextSnapshotRevision(t.Context(), query.TextSnapshotScope{Filter: filter})
	require.NoError(err)
	assert.Equal(first, samePageSet)

	_, err = db.Exec(`
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title)
			VALUES (703, 8, 'chat-703', 'direct_chat', 'Unrelated');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
			VALUES (803, 703, 8, 'message-803', 'imessage', '2026-08-20 12:00:00', 'unrelated', 12);
		INSERT INTO conversation_participants (conversation_id, participant_id) VALUES (703, 12);
	`)
	require.NoError(err)
	outOfScope, err := engine.TextSnapshotRevision(t.Context(), query.TextSnapshotScope{Filter: filter})
	require.NoError(err)
	assert.Equal(first, outOfScope)

	_, err = db.Exec(`
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title)
			VALUES (704, 7, 'chat-704', 'direct_chat', 'Zulu');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
			VALUES (804, 704, 7, 'message-804', 'imessage', '2026-08-20 13:00:00', 'matching', 11);
		INSERT INTO conversation_participants (conversation_id, participant_id) VALUES (704, 11);
	`)
	require.NoError(err)
	added, err := engine.TextSnapshotRevision(t.Context(), query.TextSnapshotScope{Filter: filter})
	require.NoError(err)
	assert.NotEqual(first, added)

	_, err = db.Exec(`UPDATE conversations SET title = 'Aardvark' WHERE id = 704`)
	require.NoError(err)
	reordered, err := engine.TextSnapshotRevision(t.Context(), query.TextSnapshotScope{Filter: filter})
	require.NoError(err)
	assert.NotEqual(added, reordered)

	filter.SortDirection = query.SortDesc
	differentSort, err := engine.TextSnapshotRevision(t.Context(), query.TextSnapshotScope{Filter: filter})
	require.NoError(err)
	assert.NotEqual(reordered, differentSort)
}

func TestSQLiteTextSnapshotRevisionMessagesTracksMembership(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	db := f.Store.DB()
	_, err := db.Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES (7, 'whatsapp', 'owner@chat.test');
		INSERT INTO participants (id, phone_number, display_name) VALUES (11, '+15550000011', 'Alice');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title) VALUES
			(701, 7, 'chat-701', 'direct_chat', 'Alpha'),
			(702, 7, 'chat-702', 'direct_chat', 'Beta');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id) VALUES
			(801, 701, 7, 'message-801', 'whatsapp', '2026-08-20 10:00:00', 'first', 11);
		INSERT INTO message_bodies (message_id, body_text) VALUES (801, 'full first body');
		UPDATE messages SET content_changed_at = '2020-01-01 00:00:00' WHERE id = 801;
	`)
	require.NoError(err)

	engine := query.NewSQLiteEngine(db)
	conversationID := int64(701)
	filter := query.TextFilter{
		Pagination:    query.Pagination{Limit: 1, Offset: 1},
		SortDirection: query.SortAsc,
	}
	scope := query.TextSnapshotScope{ConversationID: &conversationID, Filter: filter}
	first, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.NotEmpty(first)

	scope.Filter.Pagination = query.Pagination{Limit: 500, Offset: 0}
	samePageSet, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.Equal(first, samePageSet)

	_, err = db.Exec(`UPDATE message_bodies SET body_text = 'changed full body' WHERE message_id = 801`)
	require.NoError(err)
	_, err = db.Exec(`UPDATE messages SET content_changed_at = '2021-01-01 00:00:00' WHERE id = 801`)
	require.NoError(err)
	bodyRevision, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.NotEqual(first, bodyRevision)

	_, err = db.Exec(`
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
			VALUES (802, 701, 7, 'message-802', 'whatsapp', '2026-08-20 11:00:00', 'second', 11)
	`)
	require.NoError(err)
	_, err = db.Exec(`UPDATE messages SET content_changed_at = '2022-01-01 00:00:00' WHERE id = 802`)
	require.NoError(err)
	added, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.NotEqual(bodyRevision, added)

	_, err = db.Exec(`DELETE FROM messages WHERE id = 802`)
	require.NoError(err)
	deleted, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.Equal(bodyRevision, deleted)

	_, err = db.Exec(`
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
			VALUES (802, 701, 7, 'message-802', 'whatsapp', '2026-08-20 11:00:00', 'second', 11)
	`)
	require.NoError(err)
	_, err = db.Exec(`UPDATE messages SET content_changed_at = '2022-01-01 00:00:00' WHERE id = 802`)
	require.NoError(err)
	reinserted, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.NotEqual(deleted, reinserted)

	_, err = db.Exec(`UPDATE messages SET conversation_id = 702 WHERE id = 802`)
	require.NoError(err)
	moved, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.Equal(bodyRevision, moved)

	scope.Filter.ContactName = "different logical request"
	differentFilter, err := engine.TextSnapshotRevision(t.Context(), scope)
	require.NoError(err)
	assert.NotEqual(moved, differentFilter)
}

func TestSQLiteTextSnapshotReadsReturnPageAndFullRevisionTogether(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	db := f.Store.DB()
	_, err := db.Exec(`
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
		UPDATE messages SET content_changed_at = '2020-01-01 00:00:00';
	`)
	require.NoError(err)

	engine := query.NewSQLiteEngine(db)
	conversationFilter := query.TextFilter{
		Pagination:    query.Pagination{Limit: 1, Offset: 1},
		SortField:     query.TextSortByName,
		SortDirection: query.SortAsc,
	}
	conversations, conversationRevision, err := engine.ListConversationsSnapshot(
		t.Context(), conversationFilter,
	)
	require.NoError(err)
	require.Len(conversations, 1)
	assert.Equal(int64(702), conversations[0].ConversationID)
	assert.NotEmpty(conversationRevision)

	conversationFilter.Pagination = query.Pagination{Limit: 500}
	fullConversationRevision, err := engine.TextSnapshotRevision(
		t.Context(), query.TextSnapshotScope{Filter: conversationFilter},
	)
	require.NoError(err)
	assert.Equal(conversationRevision, fullConversationRevision)

	messageFilter := query.TextFilter{
		Pagination:    query.Pagination{Limit: 2, Offset: 1},
		SortDirection: query.SortAsc,
	}
	messages, messageRevision, err := engine.ListConversationMessagesSnapshot(
		t.Context(), 701, messageFilter,
	)
	require.NoError(err)
	require.Len(messages, 2)
	assert.Equal([]int64{802, 803}, []int64{messages[0].ID, messages[1].ID})
	assert.NotEmpty(messageRevision)

	messageFilter.Pagination = query.Pagination{Limit: 500}
	conversationID := int64(701)
	fullMessageRevision, err := engine.TextSnapshotRevision(t.Context(), query.TextSnapshotScope{
		ConversationID: &conversationID,
		Filter:         messageFilter,
	})
	require.NoError(err)
	assert.Equal(messageRevision, fullMessageRevision)
}
