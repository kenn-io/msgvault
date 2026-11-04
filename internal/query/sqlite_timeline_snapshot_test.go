package query_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSQLiteTimelineSnapshotProbeUsesCoveringIndex(t *testing.T) {
	require := require.New(t)
	s := testutil.NewSQLiteTestStore(t)
	rows, err := s.DB().Query("EXPLAIN QUERY PLAN "+query.SQLiteTimelineSnapshotProbe, 1, 1, 1)
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
		roster bool
	}{
		{"sent_at edit", exec(`UPDATE messages SET sent_at = '2020-01-01 09:00:00' WHERE id = 802`), false},
		{"body edit", exec(`UPDATE message_bodies SET body_text = 'updated body' WHERE message_id = 802`), false},
		// Raw SQL leaves updated_at alone, as a retitle within the same second would.
		{"chat retitle", exec(`UPDATE conversations SET title = 'Updated' WHERE id = 701`), false},
		{"roster removal", func(s *store.Store) error { return s.ReplaceConversationParticipants(701, nil) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			s := testutil.NewSQLiteTestStore(t)
			fixture := `
				INSERT INTO sources (id, source_type, identifier) VALUES (7, 'imessage', 'owner@example.com');
				INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title) VALUES
					(701, 7, 'chat-701', 'direct_chat', 'Original'), (702, 7, 'chat-702', 'direct_chat', 'Other');
				INSERT INTO participants (id, email_address, display_name) VALUES (11, 'sender@example.com', 'Original');
				INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, sender_id) VALUES
					(801, 701, 7, 'message-801', 'imessage', '2020-01-01 10:00:00', 11),
					(802, 701, 7, 'message-802', 'imessage', '2020-01-01 11:00:00', 11);
				INSERT INTO message_bodies (message_id, body_text) VALUES (802, 'original body');
				UPDATE messages SET content_changed_at = '2020-01-01 00:00:00';
			`
			if tc.roster {
				fixture = `
					INSERT INTO sources (id, source_type, identifier) VALUES (7, 'matrix', 'owner@example.com');
					INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type) VALUES (701, 7, 'chat-701', 'group_chat');
					INSERT INTO participants (id, email_address) VALUES (11, 'member@example.com'), (12, 'sender@example.com');
					INSERT INTO conversation_participants (conversation_id, participant_id) VALUES (701, 11);
					INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, sender_id, content_changed_at) VALUES
						(801, 701, 7, 'message-801', 'matrix', '2020-01-01 10:00:00', 12, '2020-01-01 00:00:00'),
						(802, 701, 7, 'message-802', 'matrix', '2020-01-01 11:00:00', 12, '2020-01-01 00:00:00');
				`
			}
			_, err := s.DB().Exec(fixture)
			require.NoError(err)
			engine := query.NewSQLiteEngine(s.DB())
			filter := query.TextFilter{Pagination: query.Pagination{Limit: 1}, SortDirection: query.SortAsc}
			if tc.roster {
				filter.ParticipantIDs = []int64{11}
			}
			page, before, err := engine.ListConversationMessagesSnapshot(t.Context(), 701, filter)
			require.NoError(err)
			if tc.roster {
				require.Len(page, 1)
				assert.Equal(int64(801), page[0].ID)
				filter.Pagination.Offset = 1
			}

			require.NoError(tc.change(s))
			if !tc.roster {
				_, err = s.DB().Exec(`UPDATE messages SET content_changed_at = '2021-01-01 00:00:00' WHERE content_changed_at > '2022-01-01'`)
				require.NoError(err)
			}
			page, after, err := engine.ListConversationMessagesSnapshot(t.Context(), 701, filter)
			require.NoError(err)
			assert.NotEqual(before, after)
			if tc.roster {
				assert.Empty(page)
			}
		})
	}
}

func TestSQLiteTimelineSnapshotRevisionStableOutsideRecentWindow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s := testutil.NewSQLiteTestStore(t)
	_, err := s.DB().Exec(`INSERT INTO sources (id, source_type, identifier) VALUES (7, 'imessage', 'owner@example.com');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type) VALUES (701, 7, 'chat-701', 'direct_chat');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type) VALUES (801, 701, 7, 'recent-message', 'imessage')`)
	require.NoError(err)
	engine := query.NewSQLiteEngine(s.DB())
	conversationID := int64(701)
	scope := query.TextSnapshotScope{ConversationID: &conversationID}
	read := func(offset string) (string, string) {
		_, err := s.DB().Exec(`UPDATE messages SET content_changed_at = strftime('%Y-%m-%d %H:%M:%f', 'now', ?) WHERE id = 801`, offset)
		require.NoError(err)
		first, err := engine.TextSnapshotRevision(t.Context(), scope)
		require.NoError(err)
		second, err := engine.TextSnapshotRevision(t.Context(), scope)
		require.NoError(err)
		return first, second
	}
	first, second := read("+1 day")
	assert.Equal(first, second)
	first, second = read("-1 second")
	assert.Equal(first, second)
}

func TestSQLiteTimelineSnapshotRevisionTracksFilteredMembership(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter query.TextFilter
	}{
		{"label", query.TextFilter{Label: "Important"}},
		{"search", query.TextFilter{SearchQuery: "needle"}},
		{"contact phone recipient", query.TextFilter{ContactPhone: "sender@example.com"}},
		{"contact name recipient", query.TextFilter{ContactName: "Sender"}},
		{"contact name repair", query.TextFilter{ContactName: "Sender"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			s := testutil.NewSQLiteTestStore(t)
			_, err := s.DB().Exec(`
				INSERT INTO sources (id, source_type, identifier) VALUES (7, 'imessage', 'owner@example.com');
				INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type) VALUES (701, 7, 'chat-701', 'direct_chat');
				INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, content_changed_at) VALUES
					(801, 701, 7, 'message-801', 'imessage', '2020-01-01 10:00:00', '2020-01-01 00:00:00'),
					(802, 701, 7, 'message-802', 'imessage', '2020-01-01 11:00:00', '2020-01-01 00:00:00');
				INSERT INTO labels (id, source_id, name) VALUES (901, 7, 'Important');
				INSERT INTO participants (id, email_address, display_name) VALUES
					(11, 'sender@example.com', 'Sender'), (12, 'other@example.com', 'Other');
				INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES
					(801, 11, 'from'), (802, 11, 'from');
			`)
			require.NoError(err)
			require.NoError(s.AddMessageLabels(801, []int64{901}))
			require.NoError(s.UpsertFTS(801, "", "needle", "", "", ""))
			require.NoError(s.UpsertFTS(802, "", "needle", "", "", ""))
			engine := query.NewSQLiteEngine(s.DB())
			conversationID := int64(701)
			filter := tc.filter
			filter.Pagination = query.Pagination{Limit: 1}
			filter.SortDirection = query.SortAsc
			scope := query.TextSnapshotScope{ConversationID: &conversationID, Filter: filter}
			page, before, err := engine.ListConversationMessagesSnapshot(t.Context(), conversationID, filter)
			require.NoError(err)
			require.Len(page, 1)
			assert.Equal(int64(801), page[0].ID)

			for step := range 2 {
				switch tc.name {
				case "label":
					if step == 0 {
						require.NoError(s.AddMessageLabels(802, []int64{901}))
					} else {
						require.NoError(s.ReplaceMessageLabels(802, nil))
					}
				case "search":
					if step == 0 {
						_, err = s.DB().Exec(`DELETE FROM messages_fts`)
						require.NoError(err)
					} else {
						require.NoError(s.UpsertFTS(801, "", "needle", "", "", ""))
						require.NoError(s.UpsertFTS(802, "", "needle", "", "", ""))
					}
				case "contact phone recipient", "contact name recipient":
					participantID := int64(12)
					if step == 1 {
						participantID = 11
					}
					require.NoError(s.ReplaceMessageRecipientsContext(t.Context(), 801, "from", []int64{participantID}, nil))
				case "contact name repair":
					displayName := "Renamed"
					if step == 1 {
						displayName = "Sender"
					}
					require.NoError(s.RepairParticipantDisplayNames([]store.ParticipantDisplayNameRepair{{ParticipantID: 11, DisplayName: displayName}}))
				}
				page, after, err := engine.ListConversationMessagesSnapshot(t.Context(), conversationID, filter)
				require.NoError(err)
				assert.NotEqual(before, after)
				revision, err := engine.TextSnapshotRevision(t.Context(), scope)
				require.NoError(err)
				assert.Equal(after, revision)
				if (tc.name == "search" || tc.name == "contact name repair") && step == 0 {
					assert.Empty(page)
				} else if (tc.name == "contact phone recipient" || tc.name == "contact name recipient") && step == 0 {
					require.Len(page, 1)
					assert.Equal(int64(802), page[0].ID)
				} else {
					require.Len(page, 1)
					assert.Equal(int64(801), page[0].ID)
				}
				before = after
			}
		})
	}
}
