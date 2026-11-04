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
	require := require.New(t)
	f := storetest.New(t)
	rows, err := f.Store.DB().Query("EXPLAIN QUERY PLAN "+query.SQLiteTimelineSnapshotProbe, 1, 1, 1)
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
	assert.Contains(t, plan.String(), "COVERING INDEX idx_messages_conversation_content_changed")
	assert.NotContains(t, plan.String(), "SCAN m")
}

func TestSQLiteTimelineSnapshotRevisionTracksChanges(t *testing.T) {
	exec := func(query string) func(*store.Store) error {
		return func(s *store.Store) error { _, err := s.DB().Exec(query); return err }
	}
	for _, tc := range []struct {
		name   string
		change func(*store.Store) error
	}{
		{"insert", exec(`INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at)
			VALUES (803, 701, 7, 'message-803', 'imessage', '2020-01-01 12:00:00')`)},
		{"hard delete", exec(`DELETE FROM messages WHERE id = 802`)},
		{"sent_at edit", exec(`UPDATE messages SET sent_at = '2020-01-01 09:00:00' WHERE id = 802`)},
		{"move out", exec(`UPDATE messages SET conversation_id = 702 WHERE id = 802`)},
		{"body edit", exec(`UPDATE message_bodies SET body_text = 'updated body' WHERE message_id = 802`)},
		{"sender rename", func(s *store.Store) error {
			return s.RepairParticipantDisplayNames([]store.ParticipantDisplayNameRepair{{ParticipantID: 11, DisplayName: "Updated"}})
		}},
		// Raw SQL leaves updated_at alone, as a retitle within the same second would.
		{"chat retitle", exec(`UPDATE conversations SET title = 'Updated' WHERE id = 701`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			s := storetest.New(t).Store
			_, err := s.DB().Exec(`
				INSERT INTO sources (id, source_type, identifier) VALUES (7, 'imessage', 'owner@example.com');
				INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title) VALUES
					(701, 7, 'chat-701', 'direct_chat', 'Original'), (702, 7, 'chat-702', 'direct_chat', 'Other');
				INSERT INTO participants (id, email_address, display_name) VALUES (11, 'sender@example.com', 'Original');
				INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, sender_id) VALUES
					(801, 701, 7, 'message-801', 'imessage', '2020-01-01 10:00:00', 11),
					(802, 701, 7, 'message-802', 'imessage', '2020-01-01 11:00:00', 11);
				INSERT INTO message_bodies (message_id, body_text) VALUES (802, 'original body');
				UPDATE messages SET content_changed_at = '2020-01-01 00:00:00';
			`)
			require.NoError(err)
			engine := query.NewSQLiteEngine(s.DB())
			filter := query.TextFilter{Pagination: query.Pagination{Limit: 1}, SortDirection: query.SortAsc}
			_, before, err := engine.ListConversationMessagesSnapshot(t.Context(), 701, filter)
			require.NoError(err)
			filter.Pagination.Offset = 1
			_, next, err := engine.ListConversationMessagesSnapshot(t.Context(), 701, filter)
			require.NoError(err)
			assert.Equal(before, next)

			require.NoError(tc.change(s))
			_, err = s.DB().Exec(`UPDATE messages SET content_changed_at = '2021-01-01 00:00:00' WHERE content_changed_at > '2022-01-01'`)
			require.NoError(err)
			_, after, err := engine.ListConversationMessagesSnapshot(t.Context(), 701, filter)
			require.NoError(err)
			assert.NotEqual(before, after)
		})
	}
}

func TestSQLiteTimelineSnapshotRevisionFencesRecentChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("recent-message")
	engine := query.NewSQLiteEngine(f.Store.DB())
	scope := query.TextSnapshotScope{ConversationID: &f.ConvID}
	read := func(offset string) (string, string) {
		_, err := f.Store.DB().Exec(`UPDATE messages SET content_changed_at = strftime('%Y-%m-%d %H:%M:%f', 'now', ?) WHERE id = ?`, offset, id)
		require.NoError(err)
		first, err := engine.TextSnapshotRevision(t.Context(), scope)
		require.NoError(err)
		second, err := engine.TextSnapshotRevision(t.Context(), scope)
		require.NoError(err)
		return first, second
	}
	// A stamp ahead of the clock keeps the fence active if the runner pauses
	// between the two reads.
	first, second := read("+1 second")
	assert.NotEqual(first, second)
	first, second = read("-1 second")
	assert.Equal(first, second)
}
