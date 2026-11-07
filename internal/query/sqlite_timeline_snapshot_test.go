package query_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestSQLiteTimelineSnapshotProbeUsesCoveringIndex(t *testing.T) {
	s := testutil.NewSQLiteTestStore(t)
	plan := dbtest.ExplainQueryPlan(t, s.DB(), query.SQLiteTimelineSnapshotProbe, 1, 1, 1, 1)
	assert.Contains(t, plan, "COVERING INDEX idx_messages_conversation_content_changed")
	assert.NotContains(t, plan, "SCAN m")
}

func TestSQLiteTimelineSnapshotRevisionTracksChanges(t *testing.T) {
	exec := func(query string) func(*store.Store) error {
		return func(s *store.Store) error { _, err := s.DB().ExecContext(t.Context(), query); return err }
	}
	for _, tc := range []struct {
		name   string
		change func(*store.Store) error
		roster bool
	}{
		{"body edit", exec(`UPDATE message_bodies SET body_text = 'updated body' WHERE message_id = 802`), false},
		// Raw SQL leaves updated_at alone, as a retitle within the same second would.
		{"chat retitle", exec(`UPDATE conversations SET title = 'Updated' WHERE id = 701`), false},
		{"chat source ID repair", exec(`UPDATE conversations SET source_conversation_id = 'repaired-chat-701' WHERE id = 701`), false},
		{"roster removal", func(s *store.Store) error { return s.ReplaceConversationParticipants(701, nil) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			s := testutil.NewSQLiteTestStore(t)
			messageType := "imessage"
			if tc.roster {
				messageType = "matrix"
			}
			dbtest.SeedTextTimeline(t, s.DB(), messageType)
			_, err := s.DB().ExecContext(t.Context(), `INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, sender_id)
				VALUES (802, 701, 7, 'message-802', ?, '2020-01-01 11:00:00', 11)`, messageType)
			require.NoError(err)
			fixture := `
				INSERT INTO message_bodies (message_id, body_text) VALUES (802, 'original body');
				UPDATE messages SET content_changed_at = '2020-01-01 00:00:00';
			`
			if tc.roster {
				fixture = `
					UPDATE conversations SET conversation_type = 'group_chat' WHERE id = 701;
					UPDATE participants SET email_address = 'member@example.com' WHERE id = 11;
					INSERT INTO participants (id, email_address) VALUES (12, 'sender@example.com');
					INSERT INTO conversation_participants (conversation_id, participant_id) VALUES (701, 11);
					UPDATE messages SET sender_id = 12, content_changed_at = '2020-01-01 00:00:00';
				`
			}
			_, err = s.DB().ExecContext(t.Context(), fixture)
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
				_, err = s.DB().ExecContext(t.Context(), `UPDATE messages SET content_changed_at = '2021-01-01 00:00:00' WHERE content_changed_at > '2022-01-01'`)
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
	_, err := s.DB().ExecContext(t.Context(), `INSERT INTO sources (id, source_type, identifier) VALUES (7, 'imessage', 'owner@example.com');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type) VALUES (701, 7, 'chat-701', 'direct_chat');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type) VALUES (801, 701, 7, 'recent-message', 'imessage')`)
	require.NoError(err)
	engine := query.NewSQLiteEngine(s.DB())
	conversationID := int64(701)
	scope := query.TextSnapshotScope{ConversationID: &conversationID}
	read := func(offset string) (string, string) {
		_, err := s.DB().ExecContext(t.Context(), `UPDATE messages SET content_changed_at = strftime('%Y-%m-%d %H:%M:%f', 'now', ?) WHERE id = 801`, offset)
		require.NoError(err)
		first, err := engine.TextSnapshotRevision(t.Context(), scope)
		require.NoError(err)
		second, err := engine.TextSnapshotRevision(t.Context(), scope)
		require.NoError(err)
		return first, second
	}
	first, second := read("-1 second")
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			s := testutil.NewSQLiteTestStore(t)
			dbtest.SeedTextTimeline(t, s.DB(), "imessage")
			_, err := s.DB().ExecContext(t.Context(), `
				INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, content_changed_at) VALUES
					(802, 701, 7, 'message-802', 'imessage', '2020-01-01 11:00:00', '2020-01-01 00:00:00');
				UPDATE messages SET sender_id = NULL, content_changed_at = '2020-01-01 00:00:00';
				INSERT INTO labels (id, source_id, name) VALUES (901, 7, 'Important');
				UPDATE participants SET display_name = 'Sender' WHERE id = 11;
				INSERT INTO participants (id, email_address, display_name) VALUES
					(12, 'other@example.com', 'Other');
				INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES
					(801, 11, 'from'), (802, 11, 'from');
			`)
			require.NoError(err)
			require.NoError(s.AddMessageLabels(t.Context(), 801, []int64{901}))
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
						require.NoError(s.AddMessageLabels(t.Context(), 802, []int64{901}))
					} else {
						require.NoError(s.ReplaceMessageLabels(802, nil))
					}
				case "search":
					if step == 0 {
						_, err = s.DB().ExecContext(t.Context(), `DELETE FROM messages_fts`)
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
				}
				page, after, err := engine.ListConversationMessagesSnapshot(t.Context(), conversationID, filter)
				require.NoError(err)
				assert.NotEqual(before, after)
				revision, err := engine.TextSnapshotRevision(t.Context(), scope)
				require.NoError(err)
				assert.Equal(after, revision)
				if tc.name == "search" && step == 0 {
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
