package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedMembershipEmbeddingGuard(tb testing.TB, s *Store, size int) {
	tb.Helper()
	seedLiveMessages(tb, s, size)
	conversationID, err := s.EnsureConversation(1, "beeper-chat", "Beeper chat")
	require.NoError(tb, err)
	for _, id := range []string{"beeper-1", "beeper-2", "beeper-3"} {
		messageID, err := s.UpsertMessage(&Message{SourceID: 1, SourceMessageID: id, ConversationID: conversationID, MessageType: "beeper"})
		require.NoError(tb, err)
		require.NoError(tb, s.UpsertMessageBody(messageID, sql.NullString{String: "Synthetic body", Valid: true}, sql.NullString{}))
	}
}

func TestMembershipEmbeddingGuardSeeksBeeperIndex(t *testing.T) {
	requirements := require.New(t)
	s, err := OpenForTest(filepath.Join(t.TempDir(), "embedding-guard.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(s.Close()) })
	requirements.NoError(s.InitSchema())
	s.db.SetMaxOpenConns(1)
	seedMembershipEmbeddingGuard(t, s, 30000)
	// A mixed archive gives the timeline index a low average conversation size,
	// while all 30,000 Beeper messages belong to one conversation.
	_, err = s.db.Exec(`UPDATE messages SET message_type = 'whatsapp' WHERE conversation_id = 1`)
	requirements.NoError(err)
	_, err = s.db.Exec(`
		WITH RECURSIVE n(i) AS (SELECT 4 UNION ALL SELECT i + 1 FROM n WHERE i < 30000)
		INSERT INTO messages (source_id, source_message_id, conversation_id, message_type)
		SELECT 1, 'beeper-' || i, 2, 'beeper' FROM n`)
	requirements.NoError(err)
	_, err = s.db.Exec(`
		WITH RECURSIVE n(i) AS (SELECT 3 UNION ALL SELECT i + 1 FROM n WHERE i < 10002)
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type)
		SELECT i, 1, 'email-' || i, 'email' FROM n`)
	requirements.NoError(err)
	_, err = s.db.Exec(`
		INSERT INTO messages (source_id, source_message_id, conversation_id, message_type)
		SELECT 1, source_conversation_id, id, 'email' FROM conversations WHERE id >= 3`)
	requirements.NoError(err)
	_, err = s.db.Exec(`
		INSERT INTO message_bodies (message_id, body_text)
		SELECT id, 'Synthetic body' FROM messages
		WHERE NOT EXISTS (SELECT 1 FROM message_bodies WHERE message_id = messages.id)`)
	requirements.NoError(err)
	for _, stats := range []string{"no-stats", "analyzed"} {
		t.Run(stats, func(t *testing.T) {
			if stats == "analyzed" {
				// Collect the full distribution rather than the connection's bounded sample.
				_, err := s.db.Exec("PRAGMA analysis_limit = 0; ANALYZE")
				require.NoError(t, err)
			}
			for _, event := range []struct {
				name  string
				args  []any
				seeks int
			}{
				{"insert", []any{1}, 1},
				{"update", []any{1, 1, 1, 1, "member", "admin", nil, nil, nil, nil, 1, 1}, 2},
				{"delete", []any{1}, 1},
			} {
				t.Run(event.name, func(t *testing.T) {
					requirements := require.New(t)
					assertions := assert.New(t)
					var trigger string
					requirements.NoError(s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`, "trg_embedding_changes_membership_"+event.name).Scan(&trigger))
					_, guard, found := strings.Cut(trigger, "WHEN")
					requirements.True(found)
					guard, _, found = strings.Cut(guard, "BEGIN")
					requirements.True(found)
					for _, field := range []string{"conversation_id", "participant_id", "role", "joined_at", "left_at"} {
						guard = strings.NewReplacer("NEW."+field, "?", "OLD."+field, "?").Replace(guard)
					}
					plan := explainPlan(t, s, "SELECT "+guard, event.args...)
					assertions.Equal(event.seeks, strings.Count(plan, "idx_messages_beeper_conversation (conversation_id=? AND message_type=?)"), plan)
					assertions.NotContains(plan, "idx_messages_conversation (")
					assertions.NotContains(plan, "idx_messages_type")
				})
			}
		})
	}
}
